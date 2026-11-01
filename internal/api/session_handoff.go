package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/tenant"
)

// HandoffItem representa um snapshot de passagem de bastão de uma sessão.
type HandoffItem struct {
	ID          int64     `json:"id"`
	Project     string    `json:"project"`
	AgentSource string    `json:"agent_source"`
	Branch      string    `json:"branch"`
	TaskSummary string    `json:"task_summary"`
	ActiveFiles []string  `json:"active_files"`
	NextSteps   string    `json:"next_steps"`
	ExpiresAt   time.Time `json:"expires_at"`
	CreatedAt   time.Time `json:"created_at"`
}

type createHandoffRequest struct {
	Project     string   `json:"project"`
	AgentSource string   `json:"agent_source"`
	Branch      string   `json:"branch"`
	TaskSummary string   `json:"task_summary"`
	ActiveFiles []string `json:"active_files"`
	NextSteps   string   `json:"next_steps"`
	TTLHours    int      `json:"ttl_hours"` // default 6h se <= 0 ou ausente, clamp max 72h
}

// SessionHandoffCreateHandler — POST /v1/sessions/handoff.
// Salva o snapshot da sessão que está terminando.
func SessionHandoffCreateHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		if !requireOrgActive(w, r, pool, orgID) {
			return
		}

		var req createHandoffRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json body")
			return
		}

		req.TaskSummary = strings.TrimSpace(req.TaskSummary)
		if req.TaskSummary == "" {
			writeError(w, http.StatusBadRequest, "task_summary is required")
			return
		}

		ttl := req.TTLHours
		if ttl <= 0 {
			ttl = 6
		} else if ttl > 72 {
			ttl = 72
		}

		expiresAt := time.Now().UTC().Add(time.Duration(ttl) * time.Hour)
		if req.ActiveFiles == nil {
			req.ActiveFiles = []string{}
		}

		var item HandoffItem
		err = tenant.RunWithTenant(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			row := tx.QueryRow(r.Context(), `
				INSERT INTO session_handoffs 
				(organization_id, project, agent_source, branch, task_summary, active_files, next_steps, expires_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
				RETURNING id, project, agent_source, branch, task_summary, active_files, next_steps, expires_at, created_at
			`, orgID, strings.TrimSpace(req.Project), strings.TrimSpace(req.AgentSource), strings.TrimSpace(req.Branch),
				req.TaskSummary, req.ActiveFiles, strings.TrimSpace(req.NextSteps), expiresAt)

			return row.Scan(
				&item.ID, &item.Project, &item.AgentSource, &item.Branch,
				&item.TaskSummary, &item.ActiveFiles, &item.NextSteps,
				&item.ExpiresAt, &item.CreatedAt,
			)
		})

		if err != nil {
			writeInternalError(w, "session handoff create", err)
			return
		}

		writeJSON(w, http.StatusCreated, item)
	}
}

// SessionHandoffLatestHandler — GET /v1/sessions/handoff/latest?project=<slug>.
// Retorna o último handoff ainda válido (expires_at > now()).
func SessionHandoffLatestHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		if !requireOrgActive(w, r, pool, orgID) {
			return
		}

		project := strings.TrimSpace(r.URL.Query().Get("project"))
		item, found, err := FetchLatestHandoff(r.Context(), pool, orgID, project)
		if err != nil {
			writeInternalError(w, "session handoff latest", err)
			return
		}

		if !found {
			writeJSON(w, http.StatusOK, map[string]any{"handoff": nil})
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{"handoff": item})
	}
}

// FetchLatestHandoff busca o último handoff válido da org (e projeto se fornecido).
func FetchLatestHandoff(ctx context.Context, pool *pgxpool.Pool, orgID int64, project string) (*HandoffItem, bool, error) {
	if pool == nil {
		return nil, false, nil
	}

	var item HandoffItem
	var found bool

	err := tenant.RunWithTenantReadOnly(ctx, pool, orgID, func(tx pgx.Tx) error {
		query := `
			SELECT id, project, agent_source, branch, task_summary, active_files, next_steps, expires_at, created_at
			FROM session_handoffs
			WHERE organization_id = $1 AND expires_at > now()
		`
		args := []any{orgID}
		if project != "" {
			query += " AND (project = $2 OR project = '')"
			args = append(args, project)
		}
		query += " ORDER BY created_at DESC LIMIT 1"

		row := tx.QueryRow(ctx, query, args...)
		if err := row.Scan(
			&item.ID, &item.Project, &item.AgentSource, &item.Branch,
			&item.TaskSummary, &item.ActiveFiles, &item.NextSteps,
			&item.ExpiresAt, &item.CreatedAt,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		found = true
		return nil
	})

	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	return &item, true, nil
}

// renderHandoffSection gera a seção Markdown compacta para o pacote de contexto.
func renderHandoffSection(item *HandoffItem) string {
	if item == nil || strings.TrimSpace(item.TaskSummary) == "" {
		return ""
	}

	var b strings.Builder
	b.WriteString("## 🔄 Última Sessão (Handoff Automático)\n\n")
	
	meta := []string{}
	if item.AgentSource != "" {
		meta = append(meta, fmt.Sprintf("Agente anterior: **%s**", item.AgentSource))
	}
	if item.Branch != "" {
		meta = append(meta, fmt.Sprintf("Branch: `%s`", item.Branch))
	}
	if !item.CreatedAt.IsZero() {
		meta = append(meta, fmt.Sprintf("Registrado em: %s", item.CreatedAt.UTC().Format("2006-01-02 15:04 UTC")))
	}
	if len(meta) > 0 {
		b.WriteString(strings.Join(meta, " · "))
		b.WriteString("\n\n")
	}

	b.WriteString("- **Tarefa em andamento:** ")
	b.WriteString(strings.TrimSpace(item.TaskSummary))
	b.WriteString("\n")

	if len(item.ActiveFiles) > 0 {
		b.WriteString("- **Arquivos ativos:** ")
		b.WriteString(strings.Join(item.ActiveFiles, ", "))
		b.WriteString("\n")
	}

	if strings.TrimSpace(item.NextSteps) != "" {
		b.WriteString("- **Próximos passos recomendados:** ")
		b.WriteString(strings.TrimSpace(item.NextSteps))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	return b.String()
}
