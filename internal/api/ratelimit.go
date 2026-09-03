package api

import (
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/time/rate"

	"github.com/nexusyn/engine/internal/metering"
	"github.com/nexusyn/engine/internal/tenant"
)

// orgLimiter agrupa o token bucket de uma org com o max_rps que o originou —
// se o limite mudar em org_limits (upgrade de plano), o bucket é recriado.
type orgLimiter struct {
	limiter   *rate.Limiter
	maxRPS    int
	fetchedAt time.Time
}

// RateLimiter aplica org_limits.max_rps por org via token bucket in-memory
// (burst = max_rps; suficiente pra instância única — sem Redis por design).
// max_rps é cacheado com TTL pra não bater no banco a cada request.
//
// Enforcement gated por NEXUS_ENFORCE_LIMITS (mesmo kill-switch da quota):
// OFF → middleware vira passthrough. 0/sem linha = ilimitado. Fail-open.
type RateLimiter struct {
	pool *pgxpool.Pool
	ttl  time.Duration

	mu   sync.Mutex
	orgs map[int64]*orgLimiter
}

// NewRateLimiter cria o limiter com cache de max_rps por 60s.
func NewRateLimiter(pool *pgxpool.Pool) *RateLimiter {
	return &RateLimiter{pool: pool, ttl: 60 * time.Second, orgs: map[int64]*orgLimiter{}}
}

// maxRPS resolve o max_rps vigente da org (cache TTL; fail-open → 0/ilimitado).
func (rl *RateLimiter) maxRPS(r *http.Request, orgID int64) int {
	var v int
	_ = rl.pool.QueryRow(r.Context(),
		`SELECT coalesce(max_rps, 0) FROM org_limits WHERE organization_id = $1`, orgID).Scan(&v)
	return v
}

// allow consome 1 token do bucket da org. true = request passa.
func (rl *RateLimiter) allow(r *http.Request, orgID int64) bool {
	rl.mu.Lock()
	ol, ok := rl.orgs[orgID]
	if !ok || time.Since(ol.fetchedAt) > rl.ttl {
		rl.mu.Unlock()
		maxRPS := rl.maxRPS(r, orgID) // fora do lock: query não bloqueia outras orgs
		rl.mu.Lock()
		ol, ok = rl.orgs[orgID]
		if !ok || ol.maxRPS != maxRPS {
			ol = &orgLimiter{maxRPS: maxRPS}
			if maxRPS > 0 {
				ol.limiter = rate.NewLimiter(rate.Limit(maxRPS), maxRPS)
			}
			rl.orgs[orgID] = ol
		}
		ol.fetchedAt = time.Now()
	}
	limiter := ol.limiter
	rl.mu.Unlock()

	if limiter == nil { // 0 = ilimitado
		return true
	}
	return limiter.Allow()
}

// ───── Baseline de leitura SEMPRE ATIVO (independente do enforcement de billing) ─────
//
// O RateLimiter acima é gated por NEXUS_ENFORCE_LIMITS — em prod hoje esse switch
// está OFF, então /v1/query, /v1/search e /v1/query/stream ficam sem NENHUM teto
// de throughput por token válido. As mutações do MCP (add/update/delete_memory)
// já tinham esse problema resolvido: allowMutation em internal/mcp/server.go é um
// token bucket in-memory por token_id, sempre ativo, independente do billing.
// baselineReadLimiters espelha o MESMO padrão para as leituras (por org, já que
// os handlers HTTP resolvem orgID cedo e não token_id). Continua valendo mesmo
// depois que o enforcement de billing for ligado — é uma camada de proteção
// distinta, não uma cota de plano.
var (
	baselineReadLimiters   = map[int64]*rate.Limiter{}
	baselineReadLimitersMu sync.Mutex
	baselineReadRPM        = envQueryRPM()
)

// envQueryRPM lê NEXUS_QUERY_RPM (rpm por org); ausente/inválido → default 120.
// Mesma convenção do envInt em internal/mcp/server.go (MCP_MUTATION_RPM): 0 (ou
// negativo) É um valor válido explícito e SIGNIFICA "desligado" — ver
// allowBaselineRead, que trata <=0 como ilimitado.
func envQueryRPM() int {
	const def = 120
	v := os.Getenv("NEXUS_QUERY_RPM")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// allowBaselineRead consome 1 token do bucket de leitura da org. true = passa.
// SEMPRE ativo — não olha metering.Enforced(). Reseta no restart (ok pra
// anti-abuso, mesmo trade-off do allowMutation do MCP).
func allowBaselineRead(orgID int64) bool {
	if orgID == 0 || baselineReadRPM <= 0 {
		return true
	}
	baselineReadLimitersMu.Lock()
	lim, ok := baselineReadLimiters[orgID]
	if !ok {
		lim = rate.NewLimiter(rate.Limit(float64(baselineReadRPM)/60.0), baselineReadRPM/2+1)
		baselineReadLimiters[orgID] = lim
	}
	baselineReadLimitersMu.Unlock()
	return lim.Allow()
}

// Middleware é o http middleware (montar APÓS o auth — precisa da org no ctx).
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !metering.Enforced() {
			next.ServeHTTP(w, r)
			return
		}
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil { // sem org no ctx → deixa o handler responder 401
			next.ServeHTTP(w, r)
			return
		}
		if !rl.allow(r, orgID) {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests,
				"rate limit exceeded for your plan — slow down or upgrade for a higher rate")
			return
		}
		next.ServeHTTP(w, r)
	})
}
