package query

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nexusyn/engine/internal/core/profile"
	"github.com/nexusyn/engine/internal/core/search"
	"github.com/nexusyn/engine/internal/provider/llm"
)

// StreamEventKind discrimina o tipo de evento SSE emitido pelo QueryStream.
type StreamEventKind string

const (
	StreamEventSources StreamEventKind = "sources" // search results (1 evento, antes dos deltas)
	StreamEventDelta   StreamEventKind = "delta"   // chunk de texto do LLM (vários)
	StreamEventDone    StreamEventKind = "done"    // sinaliza fim + usage (1 evento, último)
	StreamEventError   StreamEventKind = "error"   // erro mid-stream
)

// StreamEvent é o que QueryStream emite no canal.
// JSON-serializável: handler SSE escreve `event: <kind>\ndata: {json}\n\n`.
type StreamEvent struct {
	Kind    StreamEventKind `json:"kind"`
	Sources []search.Result `json:"sources,omitempty"`
	Delta   string          `json:"delta,omitempty"`
	Usage   *Usage          `json:"usage,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// QueryStream roda o pipeline (search → rerank → LLM stream) emitindo
// eventos progressivamente:
//  1. 'sources' assim que o SEARCH termina (~1s; top-N do RRF, pré-rerank —
//     o rerank só reordena/refina o grounding, e custaria segundos de tela
//     parada se segurasse o evento)
//  2. 'delta' a cada chunk do LLM
//  3. 'done' com usage final
//
// Erros de validação/search viram erro de retorno (antes de abrir canal).
// Erros dali em diante (rerank, setup do LLM, mid-stream) vão como event
// 'error' no canal — os headers SSE já foram escritos pelo caller.
//
// Requer que o LLM provider implemente StreamingProvider (Router cuida disso).
func (s *Service) QueryStream(ctx context.Context, orgID int64, opts Options) (<-chan StreamEvent, error) {
	opts.Defaults()
	if opts.Question == "" {
		return nil, fmt.Errorf("query stream: question vazia")
	}
	if s.llm == nil {
		return nil, fmt.Errorf("query stream: LLM provider não configurado")
	}
	// Mesma resolução do Query síncrono: temporal → modelo dedicado, senão
	// org_model_config > default global. Sem isso o stream ignoraria o
	// override por org e geraria com outro modelo que não o configurado.
	// Se o provider resolvido não suporta streaming, degrada pro default
	// global (graceful, mesmo espírito do llmForOrg).
	prov := s.llmForQuery(ctx, orgID, opts.Question)
	streamingLLM := llm.AsStreaming(prov)
	if streamingLLM == nil {
		slog.Debug("query stream: provider da org não suporta stream, usando default",
			"org_id", orgID, "provider", prov.Name())
		streamingLLM = llm.AsStreaming(s.llm)
	}
	if streamingLLM == nil {
		return nil, llm.ErrStreamNotSupported
	}

	// Sprint multi-session-recall — count queries sobem limit (mesma lógica do Query)
	effectiveLimit := EffectiveLimit(opts.Question, opts.Limit)

	// 1. Search — mesmo pipeline do Query síncrono
	searchLimit := effectiveLimit * 5
	if searchLimit < 20 {
		searchLimit = 20
	}
	mode := search.Mode(opts.Mode)
	if mode == "" {
		mode = search.ModeHybrid
	}
	// Sprint 3.2 — graph time-travel (mesma lógica do Query síncrono)
	var asOf *time.Time
	if a := DetectQueryAsOf(opts.Question, time.Now().UTC()); !a.IsZero() {
		asOf = &a
	}
	// MultiHop opt-in (mesmo padrão do Query síncrono)
	var (
		results []search.Result
		err     error
	)
	if opts.MultiHop {
		results, err = s.searchMultiHop(ctx, orgID, opts, mode, searchLimit)
	} else {
		results, err = s.search.Search(ctx, orgID, search.Options{
			Query:  opts.Question,
			Limit:  searchLimit,
			Mode:   mode,
			Domain: opts.Domain,
			AsOf:   asOf,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("query stream: search: %w", err)
	}

	// 2. Canal aberto JÁ, logo após o search: o caller escreve os headers SSE
	// e o evento 'sources' chega em ~1s, enquanto rerank + setup do LLM (que
	// custam vários segundos) rodam na goroutine. Antes tudo isso acontecia
	// PRÉ-canal e o TTFB do stream era ~5s de tela parada.
	//
	// Sources = top-N do RRF pós-search (pré-rerank). Cópia defensiva: o
	// rerank/sort abaixo reordena `results` enquanto o caller serializa.
	// Erros a partir daqui viram event 'error' (os headers já foram).
	displayN := len(results)
	if displayN > effectiveLimit {
		displayN = effectiveLimit
	}
	display := append([]search.Result(nil), results[:displayN]...)

	out := make(chan StreamEvent, 16)
	go func() {
		defer close(out)

		select {
		case out <- StreamEvent{Kind: StreamEventSources, Sources: display}:
		case <-ctx.Done():
			return
		}

		emitErr := func(err error) {
			select {
			case out <- StreamEvent{Kind: StreamEventError, Error: err.Error()}:
			case <-ctx.Done():
			}
		}

		reranked := false
		// 3. Rerank (opcional, mesma lógica do Query)
		reranker := s.reranker
		if s.resolver != nil {
			reranker = s.resolver.Reranker(ctx) // config global ao vivo
		}
		if reranker != nil && len(results) > 1 {
			docs := make([]string, len(results))
			for i, r := range results {
				docs[i] = r.Content
			}
			ranked, rerr := reranker.Rerank(ctx, opts.Question, docs, effectiveLimit)
			if rerr == nil && len(ranked) > 0 {
				reorder := make([]search.Result, 0, len(ranked))
				for _, item := range ranked {
					if item.Index >= 0 && item.Index < len(results) {
						r := results[item.Index]
						r.Score = item.Score
						r.Source = "reranked"
						reorder = append(reorder, r)
					}
				}
				results = reorder
				reranked = true
			}
		}
		if len(results) > effectiveLimit {
			results = results[:effectiveLimit]
		}

		// Sprint 1.3 — ordenar chunks por session date desc antes do prompt
		SortByDateDesc(results)

		// Sprint 1.5 — preference injection (mesma lógica do Query síncrono)
		var knownFacts []PreferenceFact
		var userProfile profile.Profile
		if s.pool != nil && IsPreferenceQuery(opts.Question) {
			if facts, lerr := LoadKnownFacts(ctx, s.pool, orgID, "", 30); lerr == nil {
				knownFacts = facts
			}
			// Sprint 3.1 — user profile blob
			if p, perr := profile.LoadProfile(ctx, s.pool, orgID); perr == nil {
				userProfile = p
			} else {
				slog.Debug("query stream: load profile failed (continuing)", "err", perr, "org_id", orgID)
			}
		}

		// Sprint 2.3 — date-math determinístico (mesma lógica do Query síncrono)
		computed := ComputeDateMath(opts.Question, time.Now().UTC())

		// 4. Build prompt
		system := buildSystemPrompt()
		user := buildUserPrompt(opts.Question, results, knownFacts, computed, userProfile)

		// 5. Estabelece o LLM stream (erro aqui = event 'error', headers já foram)
		maxTokens, thinkingBudget := 1024, 0
		if NeedsReasoning(opts.Question) {
			thinkingBudget = ReasoningBudget
			maxTokens = ReasoningBudget + 2048
		}
		llmCh, err := streamingLLM.Stream(ctx, llm.Prompt{
			System:         system,
			User:           user,
			MaxTokens:      maxTokens,
			Temperature:    0.2,
			ThinkingBudget: thinkingBudget,
		})
		if err != nil {
			emitErr(fmt.Errorf("query stream: llm: %w", err))
			return
		}

		var tokensIn, tokensOut, latencyMs int
		// Provider/Model vem do chunk Done — refleta o vendor REAL usado
		// (importante quando o Router escolheu fallback). Fallback se chunk
		// vazio (provider antigo): usa Name() do streamingLLM.
		providerName := streamingLLM.Name()
		modelName := streamingLLM.Model()
		for chunk := range llmCh {
			if chunk.Err != nil {
				select {
				case out <- StreamEvent{Kind: StreamEventError, Error: chunk.Err.Error()}:
				case <-ctx.Done():
				}
				return
			}
			if chunk.Delta != "" {
				select {
				case out <- StreamEvent{Kind: StreamEventDelta, Delta: chunk.Delta}:
				case <-ctx.Done():
					return
				}
			}
			if chunk.Done {
				tokensIn = chunk.TokensIn
				tokensOut = chunk.TokensOut
				latencyMs = chunk.LatencyMs
				if chunk.Provider != "" {
					providerName = chunk.Provider
				}
				if chunk.Model != "" {
					modelName = chunk.Model
				}
				break
			}
		}

		usage := &Usage{
			LLMProvider: providerName,
			LLMModel:    modelName,
			TokensIn:    tokensIn,
			TokensOut:   tokensOut,
			LatencyMs:   latencyMs,
			Reranked:    reranked,
		}
		select {
		case out <- StreamEvent{Kind: StreamEventDone, Usage: usage}:
		case <-ctx.Done():
		}
	}()

	return out, nil
}
