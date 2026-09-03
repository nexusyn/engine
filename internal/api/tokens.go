package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/auth"
	"github.com/nexusyn/engine/internal/tenant"
)

// writeJSON escreve uma resposta JSON com o status dado.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// tokenInfo é a view pública de um api_token — NUNCA inclui o segredo/hash.
type tokenInfo struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Abilities  []string   `json:"abilities"`
	LastUsedAt *time.Time `json:"last_used_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

// TokensListHandler — GET /v1/tokens: lista os api_tokens da org (sem o segredo).
func TokensListHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		// Esconde o token EM USO nesta request (ex: o token de infra que o console
		// usa no proxy). Não é uma key gerenciável pelo usuário — e o DELETE já o
		// recusa (auto-lockout). Some da lista pra não confundir / não tentar apagar.
		currentTokenID := tenant.TokenIDFromContext(r.Context())
		pg := parsePageParams(r, 100, 500)
		out := []tokenInfo{}
		total := 0
		err = tenant.RunWithTenantReadOnly(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			// total casa o mesmo filtro (exclui o token em uso) — pra paginação.
			if e := tx.QueryRow(r.Context(),
				`SELECT count(*) FROM api_tokens WHERE organization_id = $1 AND id <> $2`,
				orgID, currentTokenID).Scan(&total); e != nil {
				return e
			}
			rows, qerr := tx.Query(r.Context(),
				`SELECT id, name, abilities, last_used_at, expires_at, created_at
				 FROM api_tokens WHERE organization_id = $1 AND id <> $2
				 ORDER BY created_at DESC LIMIT $3 OFFSET $4`, orgID, currentTokenID, pg.Limit, pg.Offset)
			if qerr != nil {
				return qerr
			}
			defer rows.Close()
			for rows.Next() {
				var t tokenInfo
				if serr := rows.Scan(&t.ID, &t.Name, &t.Abilities, &t.LastUsedAt, &t.ExpiresAt, &t.CreatedAt); serr != nil {
					return serr
				}
				out = append(out, t)
			}
			return rows.Err()
		})
		if err != nil {
			writeInternalError(w, "tokens list", err)
			return
		}
		writeJSON(w, http.StatusOK, withPageMeta(
			map[string]any{"count": len(out), "tokens": out},
			total, pg.Limit, pg.Offset, len(out)))
	}
}

type createTokenReq struct {
	Name          string `json:"name"`
	ExpiresInDays int    `json:"expires_in_days"`
}

// TokenCreateHandler — POST /v1/tokens {name, expires_in_days}: cria um token.
// Retorna o token EM TEXTO PURO uma única vez (não dá pra recuperar depois).
func TokenCreateHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		var req createTokenReq
		if !decodeJSONBody(w, r, &req) {
			return
		}
		if req.Name == "" {
			req.Name = "dashboard"
		}

		randomHex, hashHex, gerr := auth.GenerateToken()
		if gerr != nil {
			writeInternalError(w, "token gen", gerr)
			return
		}

		// Keys mintadas pelo dashboard são de DATA PLANE (apps/agentes do cliente):
		// NÃO recebem "admin" nem "*", então não podem mexer em config de modelo /
		// eval (RequireAbility("admin") → 403). Tokens de operador (com "*") são
		// criados fora daqui (SQL/provisioning). Antes o default da coluna era '{*}'
		// — todo dashboard-key nascia com poder total; isto fecha o furo.
		clientAbilities := []string{"ingest", "query", "search", "mcp", "read", "write"}

		var id int64
		var expiresAt *time.Time
		err = tenant.RunWithTenant(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			// expires_in_days <= 0 → token sem expiração (NULL).
			return tx.QueryRow(r.Context(),
				`INSERT INTO api_tokens (organization_id, name, token_hash, abilities, expires_at)
				 VALUES ($1, $2, $3, $4, CASE WHEN $5 > 0 THEN now() + ($5 || ' days')::interval ELSE NULL END)
				 RETURNING id, expires_at`,
				orgID, req.Name, hashHex, clientAbilities, req.ExpiresInDays,
			).Scan(&id, &expiresAt)
		})
		if err != nil {
			writeInternalError(w, "token create", err)
			return
		}

		writeJSON(w, http.StatusCreated, map[string]any{
			"id":         id,
			"name":       req.Name,
			"token":      auth.FormatToken(id, randomHex), // mostrado UMA vez
			"expires_at": expiresAt,
		})
	}
}

// TokenDeleteHandler — DELETE /v1/tokens/{id}: revoga um token da org.
// Recusa deletar o token EM USO na própria request (evita auto-lockout).
func TokenDeleteHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		id, perr := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if perr != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, "invalid token id")
			return
		}
		if id == tenant.TokenIDFromContext(r.Context()) {
			writeError(w, http.StatusBadRequest, "não dá pra revogar o token em uso nesta request")
			return
		}
		var deleted int64
		err = tenant.RunWithTenant(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			ct, derr := tx.Exec(r.Context(),
				`DELETE FROM api_tokens WHERE id = $1 AND organization_id = $2`, id, orgID)
			if derr != nil {
				return derr
			}
			deleted = ct.RowsAffected()
			return nil
		})
		if err != nil {
			writeInternalError(w, "token delete", err)
			return
		}
		if deleted == 0 {
			writeError(w, http.StatusNotFound, "token não encontrado")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "id": id})
	}
}
