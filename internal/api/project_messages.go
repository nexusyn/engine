package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/tenant"
)

// ProjectMessage representa uma mensagem entre projetos/agentes.
type ProjectMessage struct {
	ID             int64      `json:"id"`
	FromProject    string     `json:"from_project"`
	ToProject      string     `json:"to_project"`
	SenderAgent    string     `json:"sender_agent"`
	Subject        string     `json:"subject"`
	Body           string     `json:"body"`
	Status         string     `json:"status"` // unread | claimed | resolved
	ClaimedByAgent string     `json:"claimed_by_agent,omitempty"`
	ClaimedAt      *time.Time `json:"claimed_at,omitempty"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
	ResolutionNote string     `json:"resolution_note,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

type sendMessageRequest struct {
	FromProject string `json:"from_project"`
	ToProject   string `json:"to_project"`
	SenderAgent string `json:"sender_agent"`
	Subject     string `json:"subject"`
	Body        string `json:"body"`
}

// ProjectMessageSendHandler — POST /v1/messages/send
func ProjectMessageSendHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		if !requireOrgActive(w, r, pool, orgID) {
			return
		}

		var req sendMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json body")
			return
		}

		req.ToProject = strings.TrimSpace(req.ToProject)
		req.Subject = strings.TrimSpace(req.Subject)
		req.Body = strings.TrimSpace(req.Body)

		if req.ToProject == "" || req.Subject == "" || req.Body == "" {
			writeError(w, http.StatusBadRequest, "to_project, subject, and body are required")
			return
		}

		var msg ProjectMessage
		err = tenant.RunWithTenant(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			row := tx.QueryRow(r.Context(), `
				INSERT INTO inter_project_messages 
				(organization_id, from_project, to_project, sender_agent, subject, body, status)
				VALUES ($1, $2, $3, $4, $5, $6, 'unread')
				RETURNING id, from_project, to_project, sender_agent, subject, body, status, created_at
			`, orgID, strings.TrimSpace(req.FromProject), req.ToProject, strings.TrimSpace(req.SenderAgent), req.Subject, req.Body)

			return row.Scan(
				&msg.ID, &msg.FromProject, &msg.ToProject, &msg.SenderAgent,
				&msg.Subject, &msg.Body, &msg.Status, &msg.CreatedAt,
			)
		})

		if err != nil {
			writeInternalError(w, "project message send", err)
			return
		}

		writeJSON(w, http.StatusCreated, msg)
	}
}

// ProjectMessageInboxHandler — GET /v1/messages/inbox?project=<slug>&status=<status>
func ProjectMessageInboxHandler(pool *pgxpool.Pool) http.HandlerFunc {
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
		status := strings.TrimSpace(r.URL.Query().Get("status"))
		if status == "" {
			status = "unread"
		}

		msgs, err := FetchInboxMessages(r.Context(), pool, orgID, project, status, 10)
		if err != nil {
			writeInternalError(w, "project message inbox", err)
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"messages": msgs,
			"count":    len(msgs),
		})
	}
}

// ProjectMessageClaimHandler — POST /v1/messages/{id}/claim
func ProjectMessageClaimHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		if !requireOrgActive(w, r, pool, orgID) {
			return
		}

		idStr := chi.URLParam(r, "id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid message id")
			return
		}

		var req struct {
			Agent string `json:"agent"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		now := time.Now().UTC()
		var msg ProjectMessage
		err = tenant.RunWithTenant(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			row := tx.QueryRow(r.Context(), `
				UPDATE inter_project_messages
				SET status = 'claimed', claimed_by_agent = $1, claimed_at = $2
				WHERE id = $3 AND organization_id = $4 AND status = 'unread'
				RETURNING id, from_project, to_project, sender_agent, subject, body, status, claimed_by_agent, claimed_at, created_at
			`, strings.TrimSpace(req.Agent), now, id, orgID)

			return row.Scan(
				&msg.ID, &msg.FromProject, &msg.ToProject, &msg.SenderAgent,
				&msg.Subject, &msg.Body, &msg.Status, &msg.ClaimedByAgent, &msg.ClaimedAt, &msg.CreatedAt,
			)
		})

		if err != nil {
			if strings.Contains(err.Error(), "no rows") {
				writeError(w, http.StatusNotFound, "message not found or already claimed")
				return
			}
			writeInternalError(w, "project message claim", err)
			return
		}

		writeJSON(w, http.StatusOK, msg)
	}
}

// ProjectMessageResolveHandler — POST /v1/messages/{id}/resolve
func ProjectMessageResolveHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		if !requireOrgActive(w, r, pool, orgID) {
			return
		}

		idStr := chi.URLParam(r, "id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid message id")
			return
		}

		var req struct {
			ResolutionNote string `json:"resolution_note"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		now := time.Now().UTC()
		var msg ProjectMessage
		err = tenant.RunWithTenant(r.Context(), pool, orgID, func(tx pgx.Tx) error {
			row := tx.QueryRow(r.Context(), `
				UPDATE inter_project_messages
				SET status = 'resolved', resolved_at = $1, resolution_note = $2
				WHERE id = $3 AND organization_id = $4
				RETURNING id, from_project, to_project, sender_agent, subject, body, status, resolution_note, resolved_at, created_at
			`, now, strings.TrimSpace(req.ResolutionNote), id, orgID)

			return row.Scan(
				&msg.ID, &msg.FromProject, &msg.ToProject, &msg.SenderAgent,
				&msg.Subject, &msg.Body, &msg.Status, &msg.ResolutionNote, &msg.ResolvedAt, &msg.CreatedAt,
			)
		})

		if err != nil {
			writeInternalError(w, "project message resolve", err)
			return
		}

		writeJSON(w, http.StatusOK, msg)
	}
}

// FetchInboxMessages busca mensagens destinadas a um projeto.
func FetchInboxMessages(ctx context.Context, pool *pgxpool.Pool, orgID int64, toProject string, status string, limit int) ([]ProjectMessage, error) {
	if pool == nil {
		return nil, nil
	}
	out := []ProjectMessage{}
	err := tenant.RunWithTenantReadOnly(ctx, pool, orgID, func(tx pgx.Tx) error {
		query := `
			SELECT id, from_project, to_project, sender_agent, subject, body, status, claimed_by_agent, claimed_at, resolved_at, resolution_note, created_at
			FROM inter_project_messages
			WHERE organization_id = $1
		`
		args := []any{orgID}
		if toProject != "" {
			query += fmt.Sprintf(" AND (to_project = $%d OR to_project = '')", len(args)+1)
			args = append(args, toProject)
		}
		if status != "" {
			query += fmt.Sprintf(" AND status = $%d", len(args)+1)
			args = append(args, status)
		}
		query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", len(args)+1)
		args = append(args, limit)

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var m ProjectMessage
			if err := rows.Scan(
				&m.ID, &m.FromProject, &m.ToProject, &m.SenderAgent,
				&m.Subject, &m.Body, &m.Status, &m.ClaimedByAgent,
				&m.ClaimedAt, &m.ResolvedAt, &m.ResolutionNote, &m.CreatedAt,
			); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// renderInboxSection renderiza avisos de mensagens pendentes para o pacote de contexto.
func renderInboxSection(msgs []ProjectMessage) string {
	if len(msgs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## 📬 Mensagens Pendentes de Outros Projetos (Inbox)\n\n")
	for _, m := range msgs {
		from := m.FromProject
		if from == "" {
			from = "global"
		}
		agent := m.SenderAgent
		if agent == "" {
			agent = "agente"
		}
		fmt.Fprintf(&b, "- ⚠️ **[%s]** De: `%s` (%s) · Assunto: **%s**\n", from, agent, m.CreatedAt.UTC().Format("02/01 15:04"), m.Subject)
		fmt.Fprintf(&b, "  > %s\n\n", strings.ReplaceAll(strings.TrimSpace(m.Body), "\n", " "))
	}
	return b.String()
}
