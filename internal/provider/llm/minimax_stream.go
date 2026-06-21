package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Stream implementa StreamingProvider via chatcompletion_v2 com stream:true
// (SSE OpenAI-compatible: `data: {...}` com choices[0].delta.content).
//
// Reasoning (M2.7+): o modelo emite chain-of-thought antes da resposta —
// inline como <think>...</think> no content e/ou no campo separado
// delta.reasoning_content. Ambos são suprimidos: reasoning_content é
// ignorado e o content passa pelo thinkStripper incremental (a tag pode
// chegar PARTIDA entre chunks, então o strip pós-completo de Complete não
// serve aqui).
//
// O chunk final do MiniMax repete a mensagem inteira em choices[0].message —
// só delta.content é emitido, senão a resposta duplicaria.
func (m *MiniMaxProvider) Stream(ctx context.Context, prompt Prompt) (<-chan StreamChunk, error) {
	if m.apiKey == "" {
		return nil, errors.New("minimax stream: API key não configurada")
	}
	if err := waitLimiter(ctx, m.limiter); err != nil {
		return nil, fmt.Errorf("minimax stream: rate limit: %w", err)
	}
	start := time.Now()

	messages := make([]minimaxMessage, 0, 2)
	if prompt.System != "" {
		messages = append(messages, minimaxMessage{Role: "system", Content: prompt.System})
	}
	messages = append(messages, minimaxMessage{Role: "user", Content: prompt.User})

	body := minimaxRequest{
		Model:       m.model,
		Messages:    messages,
		MaxTokens:   prompt.MaxTokens,
		Temperature: prompt.Temperature,
		Stream:      true,
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("minimax stream: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/v1/text/chatcompletion_v2", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("minimax stream: new req: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("minimax stream: http: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		msg := string(b)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, &HTTPError{Status: resp.StatusCode, Body: msg, Provider: "minimax"}
	}

	out := make(chan StreamChunk, 16)
	go minimaxReadSSE(ctx, resp.Body, out, m.Name(), m.model, start)
	return out, nil
}

// minimaxStreamEvent é o shape de cada `data:` do stream (OpenAI-compatible).
type minimaxStreamEvent struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"` // CoT em canal separado — nunca emitir
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	BaseResp struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
}

func minimaxReadSSE(ctx context.Context, body io.ReadCloser, out chan<- StreamChunk, providerName, model string, start time.Time) {
	defer close(out)
	defer func() { _ = body.Close() }()

	emit := func(c StreamChunk) bool {
		select {
		case out <- c:
			return true
		case <-ctx.Done():
			return false
		}
	}

	var tokensIn, tokensOut int
	stripper := &thinkStripper{}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}

		var evt minimaxStreamEvent
		if err := json.Unmarshal([]byte(payload), &evt); err != nil {
			emit(StreamChunk{Err: fmt.Errorf("minimax stream: bad json: %w", err), Done: true})
			return
		}

		// MiniMax sinaliza erro de aplicação via base_resp mesmo no SSE.
		if code := evt.BaseResp.StatusCode; code != 0 && code != 200 {
			emit(StreamChunk{Err: fmt.Errorf("minimax stream: app err %d: %s", code, evt.BaseResp.StatusMsg), Done: true})
			return
		}

		if evt.Usage.PromptTokens > 0 {
			tokensIn = evt.Usage.PromptTokens
		}
		if evt.Usage.CompletionTokens > 0 {
			tokensOut = evt.Usage.CompletionTokens
		}

		if len(evt.Choices) == 0 {
			continue
		}
		// Só delta.content — choices[0].message do chunk final repetiria tudo,
		// e reasoning_content é CoT (suprimido).
		if delta := stripper.feed(evt.Choices[0].Delta.Content); delta != "" {
			if !emit(StreamChunk{Delta: delta}) {
				return
			}
		}
	}

	if err := scanner.Err(); err != nil {
		emit(StreamChunk{Err: fmt.Errorf("minimax stream: read: %w", err), Done: true})
		return
	}

	// EOF natural — descarrega resto seguro do stripper e fecha com usage.
	if tail := stripper.flush(); tail != "" {
		if !emit(StreamChunk{Delta: tail}) {
			return
		}
	}
	emit(StreamChunk{
		Done:      true,
		TokensIn:  tokensIn,
		TokensOut: tokensOut,
		Provider:  providerName,
		Model:     model,
		LatencyMs: int(time.Since(start).Milliseconds()),
	})
}

// thinkStripper remove blocos <think>...</think> INCREMENTALMENTE — versão
// streaming do stripThinking. Mantém um buffer de texto ainda ambíguo (sufixo
// que pode ser começo de tag partida entre chunks) e só emite o que é
// comprovadamente resposta.
//
// Semântica alinhada ao stripThinking:
//   - conteúdo dentro de <think>...</think> é descartado
//   - bloco aberto sem fechamento até o EOF → descartado (flush retorna "")
//   - whitespace nas bordas é aparado como no TrimSpace: à esquerda direto;
//     à direita via retenção lazy (whitespace no fim de um delta só é emitido
//     quando chega texto real depois — no EOF ou antes de um <think> ele cai)
type thinkStripper struct {
	pending string // texto recebido ainda não classificado
	heldWS  string // whitespace retido aguardando texto real (trim à direita lazy)
	inThink bool
	emitted bool // já emitiu algo não-vazio (controla o trim à esquerda)
}

const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

// feed recebe um delta cru e retorna a parte segura pra emitir agora.
func (t *thinkStripper) feed(s string) string {
	if s == "" {
		return ""
	}
	t.pending += s
	var out strings.Builder

	for {
		if t.inThink {
			if i := strings.Index(t.pending, thinkClose); i >= 0 {
				t.pending = t.pending[i+len(thinkClose):]
				t.inThink = false
				continue
			}
			// Sem fechamento ainda: descarta tudo menos um sufixo que pode ser
			// começo de </think> partido.
			t.pending = t.pending[len(t.pending)-tagSuffixLen(t.pending, thinkClose):]
			break
		}

		if i := strings.Index(t.pending, thinkOpen); i >= 0 {
			out.WriteString(t.pending[:i])
			t.pending = t.pending[i+len(thinkOpen):]
			t.inThink = true
			continue
		}
		// Sem tag: emite tudo menos um sufixo que pode ser começo de <think>.
		hold := tagSuffixLen(t.pending, thinkOpen)
		out.WriteString(t.pending[:len(t.pending)-hold])
		t.pending = t.pending[len(t.pending)-hold:]
		break
	}

	return t.emitClean(out.String(), false)
}

// flush descarrega o que sobrou no buffer ao fim do stream. Dentro de um
// <think> aberto → descarta (mesma semântica do stripThinking truncado).
// Whitespace retido no fim cai (trim à direita, paridade com TrimSpace).
func (t *thinkStripper) flush() string {
	if t.inThink {
		t.pending, t.heldWS = "", ""
		return ""
	}
	out := t.pending
	t.pending = ""
	return t.emitClean(out, true)
}

// emitClean aplica os trims de borda: junta o whitespace retido, apara à
// esquerda enquanto nada foi emitido, e retém (ou descarta, se atEnd) o
// whitespace no fim do texto.
func (t *thinkStripper) emitClean(s string, atEnd bool) string {
	s = t.heldWS + s
	t.heldWS = ""
	if !t.emitted {
		s = strings.TrimLeft(s, " \t\r\n")
	}
	if trimmed := strings.TrimRight(s, " \t\r\n"); len(trimmed) < len(s) {
		if !atEnd {
			t.heldWS = s[len(trimmed):]
		}
		s = trimmed
	}
	if s != "" {
		t.emitted = true
	}
	return s
}

// tagSuffixLen retorna o tamanho do maior sufixo de s que é prefixo PRÓPRIO
// de tag (ou seja, uma tag possivelmente partida no fim do chunk).
func tagSuffixLen(s, tag string) int {
	max := len(tag) - 1
	if len(s) < max {
		max = len(s)
	}
	for n := max; n > 0; n-- {
		if strings.HasPrefix(tag, s[len(s)-n:]) {
			return n
		}
	}
	return 0
}
