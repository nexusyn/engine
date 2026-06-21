package llm

import (
	"context"

	"golang.org/x/time/rate"
)

// newLimiter cria um token bucket limiter pra `rps` requests/segundo.
// Burst = max(rps, 1) — permite picos curtos. Se rps <= 0 retorna nil
// (sem rate limit; chamadas passam direto).
//
// Day 21: cada Provider tem seu próprio limiter (Gemini, Anthropic, MiniMax),
// configurado via env_var RATE_LIMIT_RPS no respectivo prefixo. Evita 429s
// em uso intensivo (bench, batch ingest) sem coordenação entre callers.
func newLimiter(rps int) *rate.Limiter {
	if rps <= 0 {
		return nil
	}
	burst := rps
	if burst < 1 {
		burst = 1
	}
	return rate.NewLimiter(rate.Limit(rps), burst)
}

// waitLimiter bloqueia até obter token do limiter; respeita ctx cancellation.
// Se limiter é nil, retorna imediatamente (sem rate limit).
func waitLimiter(ctx context.Context, limiter *rate.Limiter) error {
	if limiter == nil {
		return nil
	}
	return limiter.Wait(ctx)
}
