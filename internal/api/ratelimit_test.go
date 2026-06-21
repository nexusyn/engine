package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/nexusyn/engine/internal/metering"
	"github.com/nexusyn/engine/internal/tenant"
)

// seedLimiter injeta um bucket fresco no cache (TTL não expirado → o pool
// nunca é consultado; permite testar sem banco).
func seedLimiter(rl *RateLimiter, orgID int64, maxRPS int) {
	ol := &orgLimiter{maxRPS: maxRPS, fetchedAt: time.Now()}
	if maxRPS > 0 {
		ol.limiter = rate.NewLimiter(rate.Limit(maxRPS), maxRPS)
	}
	rl.orgs[orgID] = ol
}

func reqWithOrg(orgID int64) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/query", nil)
	return r.WithContext(tenant.WithOrgID(r.Context(), orgID))
}

func TestRateLimiter_BucketPerOrg(t *testing.T) {
	rl := NewRateLimiter(nil)
	seedLimiter(rl, 1, 2) // org 1: 2 rps (burst 2)
	seedLimiter(rl, 2, 0) // org 2: ilimitado

	r := reqWithOrg(1)
	if !rl.allow(r, 1) || !rl.allow(r, 1) {
		t.Fatal("burst de 2 deveria passar")
	}
	if rl.allow(r, 1) {
		t.Fatal("3ª request no mesmo instante deveria estourar o bucket")
	}
	// Org 2 ilimitada não é afetada pelo bucket da org 1.
	r2 := reqWithOrg(2)
	for i := 0; i < 10; i++ {
		if !rl.allow(r2, 2) {
			t.Fatal("org ilimitada (max_rps=0) nunca bloqueia")
		}
	}
}

func TestRateLimiter_MiddlewarePassthroughWhenOff(t *testing.T) {
	// Enforced() lê de um atomic.Bool carregado no init() (o env só vale no boot);
	// em teste, ligar/desligar é via SetEnforced — t.Setenv não tem efeito aqui.
	metering.SetEnforced(false)
	rl := NewRateLimiter(nil)
	seedLimiter(rl, 1, 1)

	h := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// Enforcement OFF → passthrough mesmo estourando o bucket.
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, reqWithOrg(1))
		if rec.Code != http.StatusOK {
			t.Fatalf("esperava 200 com enforcement OFF, veio %d", rec.Code)
		}
	}
}

func TestRateLimiter_Middleware429(t *testing.T) {
	metering.SetEnforced(true)
	t.Cleanup(func() { metering.SetEnforced(false) })
	rl := NewRateLimiter(nil)
	seedLimiter(rl, 1, 1) // 1 rps, burst 1

	h := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, reqWithOrg(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("1ª request deveria passar, veio %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, reqWithOrg(1))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("2ª request imediata deveria dar 429, veio %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 deveria mandar Retry-After")
	}
}
