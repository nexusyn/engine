package api

import (
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/orgconfig"
	"github.com/nexusyn/engine/internal/tenant"
)

// ModelConfigGetHandler — GET /v1/config/models: retorna a config de modelo
// por etapa da org. Etapas ausentes usam o default global (.env).
func ModelConfigGetHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		cfg, err := orgconfig.Load(r.Context(), pool, orgID)
		if err != nil {
			writeInternalError(w, "config load", err)
			return
		}
		if cfg == nil {
			cfg = []orgconfig.ModelChoice{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"stages": cfg})
	}
}

// ModelConfigPutHandler — PUT /v1/config/models: grava/atualiza a escolha de
// UMA etapa (body = ModelChoice). Override do default global pra essa org.
func ModelConfigPutHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		var c orgconfig.ModelChoice
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
		defer func() { _ = r.Body.Close() }()
		if err := orgconfig.Upsert(r.Context(), pool, orgID, c); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "saved", "stage": c.Stage})
	}
}
