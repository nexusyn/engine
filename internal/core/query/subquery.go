package query

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nexusyn/engine/internal/provider/llm"
)

// SubQueryGen usa o LLM pra decompor uma pergunta em N sub-queries focadas
// em fatos atômicos. Útil pra multi-hop (ex: "How many years older is my
// grandma than me?" → ["my age", "grandma's age"]).
//
// Retorna lista vazia (não nil) se o LLM julgar que a pergunta é atômica.
// Falha silenciosa (erro logado, lista vazia retornada) — caller deve
// fallback pra single-query.
//
// Custo: 1 chamada LLM extra (~500ms pra Gemini Flash). Só ativar quando
// vale a pena (MultiHop opt-in).
type SubQueryGen struct {
	llm llm.Provider
}

func NewSubQueryGen(p llm.Provider) *SubQueryGen {
	return &SubQueryGen{llm: p}
}

const subQuerySystemPrompt = `You decompose a user question into atomic sub-queries when the question requires combining MULTIPLE distinct facts.

OUTPUT STRICT JSON: {"sub_queries": ["query 1", "query 2", ...]}

RULES:
1. If the question is ATOMIC (single fact lookup), return {"sub_queries": []}.
2. Otherwise, output 2-5 sub-queries, each targeting ONE specific fact OR ONE anchor date.
3. Sub-queries should be short, keyword-focused (think: "what to search").
4. NEVER include reasoning/inference in sub-queries — only facts to look up.
5. Preserve the user's language (PT-BR or EN — match input).
6. Output ONLY JSON, no preamble, no code fence.

TEMPORAL QUESTIONS — decompose into (event/topic) + (anchor date) when ANY of these apply:
- "when did I…" / "quando eu…"     → ["<event> date", "<event>"]
- "since when…" / "desde quando…"  → ["start of <topic>", "<topic>"]
- "how long ago…" / "há quanto tempo…" → ["<event> date", "<event>"]
- "before/after X…" / "antes/depois de X…" → ["X date", "<other event>"]
- "in (month/year)…" / "em (mês/ano)…" → ["<month/year> events", "<month/year> activities"]
- "how many days/months/years…" → split into start AND end anchors

PREFERENCE/RECOMMENDATION — decompose into (likes) + (dislikes) + (history):
- "recommend me…" / "me recomenda…" → ["user likes", "user dislikes", "previously tried"]
- "what should I…" / "o que (eu) devo…" → ["user preferences", "user constraints"]

MULTI-PART QUESTIONS — when ONE question contains TWO sub-questions about different time periods, generate sub-queries for EACH part:
- "previously / now" / "antes / agora" → ["<topic> previously count/value", "<topic> current count/value"]
- "when started / currently" → ["<topic> initial value", "<topic> current value"]
- "did I / do I" / "fazia / faço" → ["<topic> past frequency", "<topic> current frequency"]

EXAMPLES:
Q: "How many years older is my grandma than me?"
A: {"sub_queries": ["my age", "grandma age"]}

Q: "What did I say about Luna's birthday?"
A: {"sub_queries": []}

Q: "Quanto tempo entre minha mudança pra SP e o início do trabalho na ACME?"
A: {"sub_queries": ["mudança São Paulo data", "início trabalho ACME data"]}

Q: "When did I last go to the dentist?"
A: {"sub_queries": ["dentist appointment date", "dentist visit"]}

Q: "How long ago did I start learning Go?"
A: {"sub_queries": ["Go learning start date", "started Go"]}

Q: "Quantos meses faltam até o aniversário da Luna?"
A: {"sub_queries": ["Luna aniversário data", "data atual"]}

Q: "What did I do in March 2025?"
A: {"sub_queries": ["March 2025 events", "March 2025 activities", "março 2025"]}

Q: "Antes de me mudar pra SP, em que cidade eu morava?"
A: {"sub_queries": ["mudança São Paulo data", "cidade anterior"]}

Q: "Recommend me a podcast"
A: {"sub_queries": ["user podcast preferences", "podcasts disliked", "podcasts listened recently"]}

Q: "What books should I read?"
A: {"sub_queries": ["book preferences", "reading habits", "books finished"]}

Q: "How many engineers do I lead now? How many did I lead when I started?"
A: {"sub_queries": ["current engineers count", "initial engineers count", "team size"]}

Q: "How often did I play tennis previously and how often do I play now?"
A: {"sub_queries": ["tennis frequency previously", "tennis frequency currently"]}

Q: "What's the temperature today?"
A: {"sub_queries": []}`

type subQueryResponse struct {
	SubQueries []string `json:"sub_queries"`
}

// Generate decompõe `question` em sub-queries via LLM. Lista vazia se atômica.
func (s *SubQueryGen) Generate(ctx context.Context, question string) ([]string, error) {
	if s.llm == nil {
		return nil, fmt.Errorf("subquery: LLM provider nil")
	}
	prompt := llm.Prompt{
		System:      subQuerySystemPrompt,
		User:        "Question: " + question,
		MaxTokens:   256,
		Temperature: 0.0,
		JSONMode:    true,
	}
	res, err := s.llm.Complete(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("subquery llm: %w", err)
	}
	content := strings.TrimSpace(res.Content)
	// Tolerância a code fence (mesmo padrão do entities/extractor)
	if strings.HasPrefix(content, "```") {
		if i := strings.Index(content, "\n"); i >= 0 {
			content = content[i+1:]
		}
		content = strings.TrimSuffix(strings.TrimSpace(content), "```")
		content = strings.TrimSpace(content)
	}
	var parsed subQueryResponse
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return nil, fmt.Errorf("subquery: bad json: %w", err)
	}
	// Sanitize: trim, drop vazios
	out := make([]string, 0, len(parsed.SubQueries))
	for _, q := range parsed.SubQueries {
		q = strings.TrimSpace(q)
		if q != "" {
			out = append(out, q)
		}
	}
	return out, nil
}
