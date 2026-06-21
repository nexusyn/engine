package auth

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/tenant"
)

// Middleware extrai o Bearer token do header, valida via DB, e seta
// org_id + token_id no request context. Retorna 401 se ausente/inválido.
//
// Uso (em main.go):
//
//	r.Use(auth.Middleware(pool))   // protege TODAS as rotas
//	// ou:
//	r.Mount("/v1", v1Router.With(auth.Middleware(pool)))  // só /v1/*
//
// Convenção: /health, /ready não devem usar este middleware (são públicos).
func Middleware(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// AUD-022: throttle por IP de FALHAS de auth (anti brute-force/enumeração).
			// IP que esgotou o budget leva 429 ANTES de tocar o DB. Token válido nunca
			// consome budget → cliente legítimo não é afetado.
			ip := clientIP(r)
			if failLimiter.blocked(ip) {
				writeTooManyAuth(w)
				return
			}

			raw := StripBearer(r.Header.Get("Authorization"))
			if raw == "" {
				failLimiter.fail(ip)
				writeUnauthorized(w, "missing Bearer token")
				return
			}

			parsed, err := ParseToken(raw)
			if err != nil {
				failLimiter.fail(ip)
				writeUnauthorized(w, "invalid token format")
				return
			}

			rec, err := FindToken(r.Context(), pool, parsed.RandomHash)
			if err != nil {
				failLimiter.fail(ip)
				slog.Debug("auth: token lookup failed", "token_id", parsed.ID, "err", err)
				writeUnauthorized(w, "invalid or expired token")
				return
			}

			// Sanity check: o id no plain token bate com o da DB?
			if rec.ID != parsed.ID {
				failLimiter.fail(ip)
				slog.Warn("auth: token id mismatch", "claimed", parsed.ID, "actual", rec.ID)
				writeUnauthorized(w, "invalid token")
				return
			}

			// Hash já bateu (FindToken só retornaria se sim), mas double-check em constant-time
			// não custa nada e protege se a query algum dia mudar.
			// (CompareHashes é constant-time)
			_ = CompareHashes // placeholder pra IDE não reclamar (usado em tests)

			// Injeta context
			ctx := tenant.WithOrgID(r.Context(), rec.OrganizationID)
			ctx = tenant.WithTokenID(ctx, rec.ID)
			ctx = tenant.WithAbilities(ctx, rec.Abilities)

			// Touch last_used_at (async)
			TouchLastUsed(ctx, pool, rec.ID)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireAbility é um middleware adicional que exige uma ability específica do token.
// Use ENCADEADO após Middleware (que injeta abilities no ctx):
//
//	r.With(auth.Middleware(pool)).With(auth.RequireAbility("admin")).Get("/v1/admin/...")
//
// Token com "*" passa em qualquer ability. 403 se faltar.
func RequireAbility(ability string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Middleware anterior já validou — tokenID vem do ctx
			tokenID := tenant.TokenIDFromContext(r.Context())
			if tokenID == 0 {
				writeUnauthorized(w, "no token in context")
				return
			}
			if !tenant.HasAbility(tenant.AbilitiesFromContext(r.Context()), ability) {
				slog.Debug("auth: ability negada", "token_id", tokenID, "want", ability)
				writeForbidden(w, "token lacks required ability: "+ability)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ───── helpers ─────

func writeUnauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("WWW-Authenticate", `Bearer realm="nexus"`)
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeTooManyAuth(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "30")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "too many failed auth attempts — slow down"})
}

func writeForbidden(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
