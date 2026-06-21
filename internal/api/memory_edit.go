package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/core/ingest"
	"github.com/nexusyn/engine/internal/job"
	"github.com/nexusyn/engine/internal/tenant"
)

type memoryUpdateReq struct {
	Title   string `json:"title,omitempty"`
	Content string `json:"content"`
}

// MemoryGetHandler — GET /v1/memories/{id}: uma memória com o CONTENT completo
// (a lista só traz preview). Usado pelo editor do dashboard pra pré-preencher.
func MemoryGetHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		id, perr := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if perr != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, "invalid id")
			return
		}
		var (
			title, content, domain, agent string
			found                         bool
		)
		err = tenant.RunWithTenantReadOnly(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			e := tx.QueryRow(r.Context(),
				`SELECT p.title, p.content, p.domain, COALESCE(a.slug, '')
				 FROM pages p LEFT JOIN agents a ON a.id = p.agent_id
				 WHERE p.id = $1 AND p.organization_id = $2 AND p.valid_to IS NULL`,
				id, orgID).Scan(&title, &content, &domain, &agent)
			if errors.Is(e, pgx.ErrNoRows) {
				return nil
			}
			if e != nil {
				return e
			}
			found = true
			return nil
		})
		if err != nil {
			writeInternalError(w, "get", err)
			return
		}
		if !found {
			writeError(w, http.StatusNotFound, "memory not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "title": title, "content": content, "domain": domain, "agent": agent,
		})
	}
}

// MemoryUpdateHandler — PUT /v1/memories/{id}: edita o markdown (content) e/ou
// title de uma memória IN-PLACE e RE-EMBEDA. Foco: curadoria de Wiki/Knowledge.
//
// Crítico: editar o texto exige re-chunk + re-embed, senão a busca/RAG continua
// devolvendo o conteúdo antigo. Aqui: UPDATE page → apaga chunks → re-chunka →
// insere chunks (embedding NULL) → enfileira EmbedBatch (recalcula). Re-extração
// de entidades fica pra v2 (o crítico p/ recall é o re-embed).
func MemoryUpdateHandler(pool *pgxpool.Pool) http.HandlerFunc {
	insertClient, _ := job.NewInsertOnlyClient(pool) // pra enfileirar o re-embed
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		id, perr := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if perr != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, "invalid id")
			return
		}
		var req memoryUpdateReq
		if derr := json.NewDecoder(r.Body).Decode(&req); derr != nil {
			writeError(w, http.StatusBadRequest, "invalid json: "+derr.Error())
			return
		}
		defer func() { _ = r.Body.Close() }()
		if req.Content == "" {
			writeError(w, http.StatusBadRequest, "content é obrigatório")
			return
		}

		chunks := ingest.ChunkText(req.Content, ingest.DefaultChunkConfig())
		found := false
		err = tenant.RunWithTenant(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			ct, e := tx.Exec(r.Context(),
				`UPDATE pages SET content = $2, title = COALESCE(NULLIF($3, ''), title)
				 WHERE id = $1 AND organization_id = $4 AND valid_to IS NULL`,
				id, req.Content, req.Title, orgID)
			if e != nil {
				return e
			}
			if ct.RowsAffected() == 0 {
				return nil // não achou a memória (current) → 404
			}
			found = true
			// re-chunk: apaga os antigos, insere os novos (embedding NULL → batch pega)
			if _, e := tx.Exec(r.Context(), `DELETE FROM chunks WHERE page_id = $1 AND organization_id = $2`, id, orgID); e != nil {
				return e
			}
			for _, c := range chunks {
				if _, e := tx.Exec(r.Context(),
					`INSERT INTO chunks (organization_id, page_id, position, content) VALUES ($1, $2, $3, $4)`,
					orgID, id, c.Position, c.Content); e != nil {
					return e
				}
			}
			return nil
		})
		if err != nil {
			writeInternalError(w, "update", err)
			return
		}
		if !found {
			writeError(w, http.StatusNotFound, "memory not found")
			return
		}

		// re-embed async (detached, sobrevive ao return)
		if insertClient != nil {
			go func() {
				bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = job.EnqueueEmbedBatch(bg, insertClient, 1*time.Second)
			}()
		}

		writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "updated", "chunks": len(chunks)})
	}
}

// MemoryDeleteHandler — DELETE /v1/memories/{id}: remove a memória da busca.
// Soft-delete bi-temporal (valid_to = now() → some dos índices "current") +
// hard-delete dos chunks (sai do recall). Mantém histórico da page.
func MemoryDeleteHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		id, perr := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if perr != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, "invalid id")
			return
		}
		found := false
		err = tenant.RunWithTenant(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			ct, e := tx.Exec(r.Context(),
				`UPDATE pages SET valid_to = now()
				 WHERE id = $1 AND organization_id = $2 AND valid_to IS NULL`, id, orgID)
			if e != nil {
				return e
			}
			if ct.RowsAffected() == 0 {
				return nil
			}
			found = true
			_, e = tx.Exec(r.Context(), `DELETE FROM chunks WHERE page_id = $1 AND organization_id = $2`, id, orgID)
			return e
		})
		if err != nil {
			writeInternalError(w, "delete", err)
			return
		}
		if !found {
			writeError(w, http.StatusNotFound, "memory not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "deleted"})
	}
}
