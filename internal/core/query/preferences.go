package query

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/tenant"
)

// PreferenceFact é um fato durable do usuário (likes, dislikes, lessons, habits)
// que vive no grafo como entity de kind preference|lesson. Surge no prompt
// como bloco KNOWN PREFERENCES antes dos chunks de retrieval.
//
// Sprint 1.5 — padrão ByteRover/Letta: preferences são first-class facts,
// injetados sempre que a query é de tipo recommendation/suggestion.
type PreferenceFact struct {
	Name      string // ex: "I love spicy food"
	Kind      string // preference | lesson
	Polarity  string // like | dislike | neutral (de attributes)
	Category  string // food | media | schedule | ... (de attributes)
	Detail    string // texto livre adicional (de attributes)
	EdgeKinds []string
}

// LoadKnownFacts puxa preferences + lessons do grafo da org.
// Limite default 30 (Letta core memory tipicamente cabe em ~1k tokens).
//
// Filtra por kind ∈ {preference, lesson}. Categoria opcional restringe ainda
// mais (ex: "food" pra "what should I eat"). Vazia = todas.
func LoadKnownFacts(ctx context.Context, pool *pgxpool.Pool, orgID int64, category string, limit int) ([]PreferenceFact, error) {
	if limit <= 0 {
		limit = 30
	}
	var facts []PreferenceFact
	err := tenant.RunWithTenantReadOnly(ctx, pool, orgID, func(tx pgx.Tx) error {
		q := `
			SELECT name, kind,
			       COALESCE(attributes->>'polarity', '') AS polarity,
			       COALESCE(attributes->>'category', '') AS category,
			       COALESCE(attributes->>'detail', '')   AS detail
			FROM entities
			WHERE kind IN ('preference', 'lesson')
		`
		args := []any{}
		if category != "" {
			q += " AND attributes->>'category' = $1"
			args = append(args, category)
		}
		q += fmt.Sprintf(" ORDER BY transaction_time DESC LIMIT %d", limit)

		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("preferences query: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var f PreferenceFact
			if err := rows.Scan(&f.Name, &f.Kind, &f.Polarity, &f.Category, &f.Detail); err != nil {
				return fmt.Errorf("preferences scan: %w", err)
			}
			facts = append(facts, f)
		}
		return rows.Err()
	})
	return facts, err
}

// preferenceQueryRegex detecta perguntas tipo "what can I", "recommend",
// "suggest", "what should I", etc. — em EN e PT-BR. Quando bate, query
// pipeline ativa preference injection.
//
// Mantém propositalmente liberal — falsos positivos só anexam contexto extra
// (não dói LLM), falsos negativos perdem ganho.
var preferenceQueryRegex = regexp.MustCompile(
	`(?i)(` +
		// EN — recommendation/advice verbs & nouns. STEMS (no trailing \b) so
		// plurals/suffixes match too: "suggest" → suggestions, "recommend" →
		// recommendations. (Fix: old `\bsuggestion\b` missed the plural.)
		`recommend|suggest|advice|advis|` +
		`\btips?\b|\bideas?\b|` +
		`what (can|should|could|would|might) i\b|\bshould i\b|` +
		`do you think|good idea|` +
		`thinking (of|about)|trying to (decide|choose|pick)|deciding\b|` +
		`any (good|new|other|better)\b|` +
		`\b(i\s+)?(love|like|likes|hate|enjoy|prefer|avoid|dislike)\b|` +
		// PT-BR
		`recomend|sugest|sugere|me indica|alguma (dica|ideia|sugest)|\bdicas?\b|` +
		`o que (devo|posso|eu (devo|posso))\b|o que voc[eê] acha|vale a pena|` +
		`gosto de|odeio|prefiro|evito|adoro|detesto` +
		`)`,
)

// IsPreferenceQuery é o gate público pro preference injection.
func IsPreferenceQuery(q string) bool {
	return preferenceQueryRegex.MatchString(q)
}

// FormatKnownFacts renderiza facts como bloco markdown pra prompt. Vazio se
// não houver facts. Garante determinismo (sort estável).
func FormatKnownFacts(facts []PreferenceFact) string {
	if len(facts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("KNOWN PREFERENCES & LESSONS (user's durable facts — apply these in recommendations):\n")
	for _, f := range facts {
		b.WriteString("- ")
		switch f.Polarity {
		case "dislike", "avoid":
			b.WriteString("AVOID: ")
		case "like", "prefer":
			b.WriteString("LIKES: ")
		}
		b.WriteString(f.Name)
		extras := []string{}
		if f.Category != "" {
			extras = append(extras, "category="+f.Category)
		}
		if f.Detail != "" {
			extras = append(extras, f.Detail)
		}
		if len(extras) > 0 {
			b.WriteString(" (")
			b.WriteString(strings.Join(extras, "; "))
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	return b.String()
}
