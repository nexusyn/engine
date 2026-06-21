package llm

import (
	"context"
	"errors"
)

// StreamingProvider é implementada por providers que suportam streaming SSE.
// Opcional — providers sem suporte ainda podem se conformar só ao Provider básico.
//
// Contrato:
//   - Stream retorna canal lendo StreamChunk até fechar
//   - Erros transientes APÓS o primeiro chunk são emitidos como chunk.Err
//   - Erros ANTES do primeiro chunk vão no error de retorno (Router pode tentar fallback)
//   - Último chunk pode ter Done=true + Usage preenchido (best-effort por provider)
//   - Canceling ctx encerra goroutine produtora; canal é fechado
type StreamingProvider interface {
	Provider
	Stream(ctx context.Context, prompt Prompt) (<-chan StreamChunk, error)
}

// StreamChunk é uma unidade de streaming de um provider LLM.
//
// Tipicamente:
//   - {Delta: "hello"} ... {Delta: " world"} ... {Done: true, TokensIn:N, TokensOut:M, Provider:..., Model:..., LatencyMs:...}
//   - Em erro mid-stream: {Err: someError, Done: true}
//
// Provider/Model/LatencyMs preenchidos pelo provider no chunk Done — permite
// ao caller saber EXATAMENTE qual provider foi efetivamente usado (importante
// quando Router escolheu fallback) e medir latência total do stream.
type StreamChunk struct {
	Delta     string // pedaço de texto incremental (vazio se Done sem texto)
	Done      bool   // último chunk do stream
	Err       error  // erro mid-stream (parse, network, app)
	TokensIn  int    // preenchido só em chunks Done (best-effort)
	TokensOut int    // idem
	Provider  string // preenchido no chunk Done — vendor real usado (não Router)
	Model     string // preenchido no chunk Done
	LatencyMs int    // tempo decorrido do Stream call até o Done
}

// ErrStreamNotSupported é retornado quando Router.Stream é chamado mas
// nenhum provider implementa StreamingProvider.
var ErrStreamNotSupported = errors.New("llm: nenhum provider suporta streaming")

// AsStreaming faz type assertion segura. Retorna nil se p não suporta stream.
func AsStreaming(p Provider) StreamingProvider {
	if sp, ok := p.(StreamingProvider); ok {
		return sp
	}
	return nil
}
