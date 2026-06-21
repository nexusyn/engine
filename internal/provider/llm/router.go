package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// HTTPError é o erro tipado retornado pelos providers em HTTP não-200.
// Permite ao Router decidir quais erros valem retry/fallback.
type HTTPError struct {
	Status   int
	Body     string
	Provider string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s: http %d: %s", e.Provider, e.Status, e.Body)
}

// isRetryable retorna true para status codes onde vale tentar fallback.
// 429 (rate limit), 408 (timeout), 5xx (server errors). NÃO inclui 4xx
// não-retryable (400, 401, 403, 404) porque indicam bug no request.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		s := httpErr.Status
		return s == 429 || s == 408 || (s >= 500 && s <= 599)
	}
	// Erros de rede (DNS, connection refused, timeout) — wrapped pelo http client
	// não são *HTTPError, mas valem fallback.
	return errors.Is(err, context.DeadlineExceeded) || isNetError(err)
}

// isNetError é heurística pra "erro de rede" baseada na string —
// alternativa seria checar net.Error via errors.As. Aceita ambos.
func isNetError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range []string{"connection refused", "no such host", "i/o timeout", "EOF"} {
		if contains(msg, marker) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Router implementa Provider tentando primary → fallbacks em ordem.
// Cada provider tem 1 tentativa direta (sem retry intra-provider — fallback
// é a estratégia de retry). Falhas não-retryable (4xx que não 429) abortam
// sem cair pra fallback.
type Router struct {
	primary   Provider
	fallbacks []Provider
}

// NewRouter monta o router. primary pode ser nil se fallbacks tem ao menos 1.
func NewRouter(primary Provider, fallbacks ...Provider) *Router {
	return &Router{primary: primary, fallbacks: fallbacks}
}

// Name retorna o nome do primary (ou primeiro fallback se primary nil).
func (r *Router) Name() string {
	if r.primary != nil {
		return r.primary.Name() + "+fallbacks"
	}
	if len(r.fallbacks) > 0 {
		return r.fallbacks[0].Name() + "+fallbacks"
	}
	return "router-empty"
}

// Model retorna o model do primary (ou primeiro fallback).
func (r *Router) Model() string {
	if r.primary != nil {
		return r.primary.Model()
	}
	if len(r.fallbacks) > 0 {
		return r.fallbacks[0].Model()
	}
	return ""
}

// Stream tenta primary; se o ESTABELECIMENTO do stream falhar com erro
// retryable, tenta fallbacks na ordem. Uma vez estabelecido (1º chunk
// pode chegar), erros mid-stream propagam direto — sem rebobinar.
//
// Provider precisa implementar StreamingProvider; caso contrário pula.
// Se NENHUM provider na chain suporta stream, retorna ErrStreamNotSupported.
func (r *Router) Stream(ctx context.Context, prompt Prompt) (<-chan StreamChunk, error) {
	candidates := make([]Provider, 0, 1+len(r.fallbacks))
	if r.primary != nil {
		candidates = append(candidates, r.primary)
	}
	candidates = append(candidates, r.fallbacks...)

	if len(candidates) == 0 {
		return nil, errors.New("llm router stream: nenhum provider disponível")
	}

	var lastErr error
	supportsStreamCount := 0
	for i, p := range candidates {
		sp := AsStreaming(p)
		if sp == nil {
			continue // provider não implementa stream — pula
		}
		supportsStreamCount++

		ch, err := sp.Stream(ctx, prompt)
		if err == nil {
			if i > 0 {
				slog.Info("llm router stream: fallback succeeded", "provider", p.Name(), "tier", i)
			}
			return ch, nil
		}

		lastErr = err
		if !isRetryable(err) {
			slog.Warn("llm router stream: erro não-retryable, abortando",
				"provider", p.Name(), "tier", i, "err", err)
			return nil, fmt.Errorf("llm router stream: %w", err)
		}

		slog.Warn("llm router stream: provider falhou no setup, tentando próximo",
			"provider", p.Name(), "tier", i, "err", err)
	}

	if supportsStreamCount == 0 {
		return nil, ErrStreamNotSupported
	}
	return nil, fmt.Errorf("llm router stream: todos providers falharam, último erro: %w", lastErr)
}

// Complete tenta primary; se falhar com erro retryable, tenta fallbacks na
// ordem. Erros não-retryable (4xx bug do request) abortam sem fallback.
// Retorna o primeiro Result bem-sucedido, ou o último erro acumulado.
func (r *Router) Complete(ctx context.Context, prompt Prompt) (Result, error) {
	candidates := make([]Provider, 0, 1+len(r.fallbacks))
	if r.primary != nil {
		candidates = append(candidates, r.primary)
	}
	candidates = append(candidates, r.fallbacks...)

	if len(candidates) == 0 {
		return Result{}, errors.New("llm router: nenhum provider disponível")
	}

	var lastErr error
	for i, p := range candidates {
		start := time.Now()
		res, err := p.Complete(ctx, prompt)
		if err == nil {
			if i > 0 {
				slog.Info("llm router: fallback succeeded",
					"provider", p.Name(),
					"tier", i,
					"latency_ms", time.Since(start).Milliseconds(),
				)
			}
			return res, nil
		}

		lastErr = err
		if !isRetryable(err) {
			// Bug no request — não vale tentar outro provider
			slog.Warn("llm router: erro não-retryable, abortando",
				"provider", p.Name(),
				"tier", i,
				"err", err,
			)
			return Result{}, fmt.Errorf("llm router: %w", err)
		}

		slog.Warn("llm router: provider falhou, tentando próximo",
			"provider", p.Name(),
			"tier", i,
			"remaining", len(candidates)-i-1,
			"err", err,
		)
	}

	return Result{}, fmt.Errorf("llm router: todos %d providers falharam, último erro: %w", len(candidates), lastErr)
}
