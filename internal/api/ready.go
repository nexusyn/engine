package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/storage"
)

// ReadyResponse é o payload do /ready.
// Distinto de /health: ready verifica deps (DB), health só responde "processo vivo".
type ReadyResponse struct {
	Status    string            `json:"status"` // "ok" | "degraded"
	Checks    map[string]string `json:"checks"` // por componente
	Timestamp time.Time         `json:"timestamp"`
}

// ReadyHandler retorna 200 se DB ok, 503 caso contrário.
// Use em probes que devem retirar tráfego se deps caírem (k8s readinessProbe, Traefik).
func ReadyHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		checks := map[string]string{}
		ok := true

		if err := storage.Health(r.Context(), pool); err != nil {
			checks["postgres"] = "fail: " + err.Error()
			ok = false
		} else {
			checks["postgres"] = "ok"
		}

		status := "ok"
		code := http.StatusOK
		if !ok {
			status = "degraded"
			code = http.StatusServiceUnavailable
		}

		resp := ReadyResponse{
			Status:    status,
			Checks:    checks,
			Timestamp: time.Now().UTC(),
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(resp)
	}
}
