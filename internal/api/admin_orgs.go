package api

import (
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/auth"
)

// AdminCreateOrgRequest — body do POST /v1/admin/orgs (control-plane).
type AdminCreateOrgRequest struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// AdminCreateOrgResponse devolve o id da org nova + o token bootstrap (em texto
// puro, UMA vez). O console guarda o token cifrado e o usa como token da org.
type AdminCreateOrgResponse struct {
	OrgID int64  `json:"org_id"`
	Token string `json:"token"`
}

// AdminCreateOrgHandler — POST /v1/admin/orgs: provisiona uma org NOVA + token
// bootstrap RESTRITO (data-plane, sem admin/*). Control-plane: gated por
// RequireAbility("admin") no router — só o operador (MASTER token) chama.
//
// Usa a função SECURITY DEFINER create_org_with_token (migration 0018) pra
// bypassar a RLS de organizations/api_tokens (o handler roda ligado à org do
// MASTER token, mas cria uma org diferente).
func AdminCreateOrgHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req AdminCreateOrgRequest
		if !decodeJSONBody(w, r, &req) {
			return
		}

		req.Slug = strings.ToLower(strings.TrimSpace(req.Slug))
		req.Name = strings.TrimSpace(req.Name)
		if req.Slug == "" {
			writeError(w, http.StatusBadRequest, "slug é obrigatório")
			return
		}
		if req.Name == "" {
			req.Name = req.Slug
		}

		// Token bootstrap da org nova: data-plane restrito (mesmo conjunto das keys
		// de cliente) — sem admin/* → cliente não muda config/modelo nem via proxy.
		randomHex, hashHex, gerr := auth.GenerateToken()
		if gerr != nil {
			writeInternalError(w, "token gen", gerr)
			return
		}
		abilities := []string{"ingest", "query", "search", "mcp", "read", "write"}

		var orgID, tokenID int64
		err := pool.QueryRow(r.Context(),
			`SELECT org_id, token_id FROM create_org_with_token($1, $2, $3, $4)`,
			req.Slug, req.Name, hashHex, abilities,
		).Scan(&orgID, &tokenID)
		if err != nil {
			// Colisão de slug (unique_violation) → 409 pro console gerar outro.
			if strings.Contains(err.Error(), "organizations_slug_key") || strings.Contains(err.Error(), "duplicate key") {
				writeError(w, http.StatusConflict, "slug já existe: "+req.Slug)
				return
			}
			writeInternalError(w, "create org", err)
			return
		}

		writeJSON(w, http.StatusCreated, AdminCreateOrgResponse{
			OrgID: orgID,
			Token: auth.FormatToken(tokenID, randomHex), // mostrado UMA vez
		})
	}
}
