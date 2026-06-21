// Package api contém os HTTP handlers (chi).
//
// Convenção: handlers NÃO têm regra de negócio. Eles:
//  1. Decodam request (JSON, query params, headers)
//  2. Validam input
//  3. Chamam camada core/ com Context
//  4. Encodam resposta (JSON ou SSE)
//
// Regra: nenhum handler importa pgx ou batalha direta com DB — vai via core/ ou storage/.
package api

import (
	"encoding/json"
	"net/http"
	"time"
)

// HealthResponse é o payload retornado pelo /health.
// Mantemos minúsculo e simples — health é hot path (chamado por load balancers, k8s probes).
type HealthResponse struct {
	Status    string    `json:"status"`            // "ok" sempre que servidor responde
	Version   string    `json:"version,omitempty"` // setado pelo main via ldflags
	Commit    string    `json:"commit,omitempty"`  // git SHA curto
	Timestamp time.Time `json:"timestamp"`
}

// HealthHandler retorna um handler stateless que sempre devolve 200 OK.
// version/commit são injetados pelo caller (main) via closure.
//
// Por enquanto não checa Postgres/dependencies — esse é o /ready endpoint (a criar).
// Health = "processo respondendo"; Ready = "dependencies OK pra servir tráfego".
func HealthHandler(version, commit string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := HealthResponse{
			Status:    "ok",
			Version:   version,
			Commit:    commit,
			Timestamp: time.Now().UTC(),
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}
}
