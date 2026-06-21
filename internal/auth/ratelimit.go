package auth

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// authFailLimiter — anti brute-force/enumeração de token (AUD-022). Bucket por IP que SÓ
// é consumido em FALHA de auth → cliente legítimo (token válido) NUNCA é throttled. Quando
// o IP esgota o budget, as próximas tentativas levam 429 (antes de tocar o DB). Burst 20,
// refil 1 a cada 3s (~20 falhas/min sustentadas por IP). In-memory (instância única).
type authFailLimiter struct {
	mu sync.Mutex
	m  map[string]*rate.Limiter
}

var failLimiter = &authFailLimiter{m: make(map[string]*rate.Limiter)}

// failLimiterMaxIPs — teto de IPs rastreados; reset cru se estourar (anti-blowup por IP spoof).
const failLimiterMaxIPs = 50_000

func (l *authFailLimiter) get(ip string) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.m) > failLimiterMaxIPs {
		l.m = make(map[string]*rate.Limiter)
	}
	lim, ok := l.m[ip]
	if !ok {
		lim = rate.NewLimiter(rate.Every(3*time.Second), 20)
		l.m[ip] = lim
	}
	return lim
}

// blocked: o IP esgotou o budget de falhas? (checado ANTES de tentar autenticar).
func (l *authFailLimiter) blocked(ip string) bool { return l.get(ip).Tokens() < 1 }

// fail: consome 1 token do budget (chamado em cada falha de auth).
func (l *authFailLimiter) fail(ip string) { l.get(ip).Allow() }

// clientIP extrai o IP real do cliente. Atrás do Traefik: X-Forwarded-For (1º hop) / X-Real-IP.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		return xr
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
