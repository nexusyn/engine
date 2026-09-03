package api

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/metering"
)

// AdminSettingsGetHandler — GET /v1/admin/settings: estado dos flags globais (admin-only).
func AdminSettingsGetHandler(_ *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"enforce_limits": metering.Enforced()})
	}
}

// AdminEnforcementPutHandler — PUT /v1/admin/settings/enforcement {"enabled":bool}: liga/
// desliga o enforcement de cota GLOBALMENTE (botão do admin). Persiste em platform_settings
// (sobrevive a restart) + atualiza o flag em memória (efeito imediato). BETA: default OFF.
// Admin-only (RequireAbility no router).
func AdminEnforcementPutHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if !decodeJSONBody(w, r, &req) {
			return
		}

		val := "false"
		if req.Enabled {
			val = "true"
		}
		if _, err := pool.Exec(r.Context(),
			`INSERT INTO platform_settings (key, value, updated_at) VALUES ('enforce_limits', $1, now())
			 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, val); err != nil {
			writeInternalError(w, "set enforcement", err)
			return
		}
		metering.SetEnforced(req.Enabled) // efeito imediato (sem esperar restart)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enforce_limits": req.Enabled})
	}
}
