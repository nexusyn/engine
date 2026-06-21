// Package eval roda um mini-LongMemEval embutido contra um provider de geração,
// julgando com um provider juiz independente. É o backend do endpoint /v1/eval
// ("test performance" do dashboard): isola o MODELO — injeta o contexto
// completo no prompt (sem retrieval), então o score reflete a capacidade do
// modelo, não a recuperação. Retorna accuracy + latência por categoria.
package eval

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/nexusyn/engine/internal/core/query"
	"github.com/nexusyn/engine/internal/provider/llm"
)

//go:embed eval_set.json
var evalSetRaw []byte

// concurrency limita gerações/julgamentos simultâneos (mantém o endpoint
// síncrono sob ~30-40s e respeita rate limits dos providers).
const concurrency = 6

// Case é um caso de avaliação (contexto completo + gold).
type Case struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Question string `json:"question"`
	Context  string `json:"context"`
	Gold     string `json:"gold"`
}

// Cases retorna o eval-set embutido.
func Cases() ([]Case, error) {
	var cs []Case
	if err := json.Unmarshal(evalSetRaw, &cs); err != nil {
		return nil, fmt.Errorf("eval: parse set: %w", err)
	}
	return cs, nil
}

// TypeStat é o placar de uma categoria.
type TypeStat struct {
	Correct int `json:"correct"`
	Total   int `json:"total"`
}

// Report agrega o resultado do eval.
type Report struct {
	Total       int                 `json:"total"`
	Correct     int                 `json:"correct"`
	Accuracy    float64             `json:"accuracy"`
	ByType      map[string]TypeStat `json:"by_type"`
	LatencyP50  int                 `json:"latency_p50_ms"`
	GenProvider string              `json:"gen_provider"`
	GenModel    string              `json:"gen_model"`
}

// Run avalia o eval-set com gen (gerador) + judge (juiz independente).
// systemPrompt = o system prompt de produção (passado pelo caller pra ser
// representativo). Roda os casos em paralelo (bounded).
func Run(ctx context.Context, gen, judge llm.Provider, systemPrompt string) (*Report, error) {
	cases, err := Cases()
	if err != nil {
		return nil, err
	}

	type outcome struct {
		typ string
		ok  bool
		lat int
	}
	results := make([]outcome, len(cases))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, c := range cases {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, c Case) {
			defer wg.Done()
			defer func() { <-sem }()
			user := "CONTEXT (user's own past conversations, MOST RECENT FIRST):\n\n" + c.Context + "\n\nQUESTION: " + c.Question
			// Espelha o pipeline real: count/ordering/duração ligam thinking
			// (senão knowledge-update/multi-session/temporal subcontam/desordenam).
			maxTok, think := 1024, 0
			if query.NeedsReasoning(c.Question) {
				think = query.ReasoningBudget
				maxTok = query.ReasoningBudget + 2048
			}
			res, gerr := gen.Complete(ctx, llm.Prompt{System: systemPrompt, User: user, MaxTokens: maxTok, Temperature: 0.2, ThinkingBudget: think})
			o := outcome{typ: c.Type}
			if gerr == nil {
				o.lat = res.LatencyMs
				ans := strings.TrimSpace(res.Content)
				o.ok = ans != "" && judgeAnswer(ctx, judge, c, ans)
			}
			results[i] = o
		}(i, c)
	}
	wg.Wait()

	rep := &Report{ByType: map[string]TypeStat{}, GenProvider: gen.Name(), GenModel: gen.Model()}
	var lats []int
	for _, o := range results {
		st := rep.ByType[o.typ]
		st.Total++
		if o.ok {
			st.Correct++
			rep.Correct++
		}
		rep.ByType[o.typ] = st
		rep.Total++
		if o.lat > 0 {
			lats = append(lats, o.lat)
		}
	}
	if rep.Total > 0 {
		rep.Accuracy = float64(rep.Correct) / float64(rep.Total)
	}
	if len(lats) > 0 {
		sort.Ints(lats)
		rep.LatencyP50 = lats[len(lats)/2]
	}
	return rep, nil
}

func judgeAnswer(ctx context.Context, judge llm.Provider, c Case, answer string) bool {
	tmpl := judgeGeneric
	switch c.Type {
	case "single-session-preference":
		tmpl = judgePreference
	case "temporal-reasoning":
		tmpl = judgeTemporal
	}
	prompt := strings.NewReplacer("{question}", c.Question, "{answer}", c.Gold, "{response}", answer).Replace(tmpl)
	res, err := judge.Complete(ctx, llm.Prompt{User: prompt, MaxTokens: 8, Temperature: 0})
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(res.Content)), "yes")
}

const judgeGeneric = `I will give you a question, a correct answer, and a response from a model. Please answer yes if the response contains the correct answer. Otherwise answer no.
LENIENCY: minor unit/phrasing differences are OK; accept if the response contains the correct value(s) or equivalent intermediate steps. Answer no only if factually wrong, missing the key value, or a guess.

Question: {question}
Correct Answer: {answer}
Model Response: {response}

Is the model response correct? Answer yes or no only.`

const judgePreference = `I will give you a question about a user's preferences, the correct answer (the user's actual preferences), and a model's response. Answer yes if the response captures the same key preferences as the correct answer (even if phrased differently); minor wording or extra valid suggestions consistent with the user's taste are OK. Answer no for generic recommendations that ignore the user's specific tastes, or anything contradictory.

Question: {question}
Correct Answer: {answer}
Model Response: {response}

Is the model response correct? Answer yes or no only.`

const judgeTemporal = `I will give you a question about temporal reasoning (dates, durations, ordering), the correct answer, and a model's response. Answer yes if the response contains the correct date/duration/relationship, even if phrased differently ("3 months" == "90 days"; "March 2024" == "03/2024"). Answer no if wrong, missing, or a guess.

Question: {question}
Correct Answer: {answer}
Model Response: {response}

Is the model response correct? Answer yes or no only.`
