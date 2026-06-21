// Package profile sintetiza preferences/lessons da org em documento markdown
// consultável e injetado em queries detected como preference-flavored.
//
// Sprint 3.1: BuildUserProfileJob (River PeriodicJob 1h) → user_profiles row.
// Read em /v1/query via LoadProfile + FormatProfileBlock.
//
// Filosofia: extract entities é granular (1 fato → 1 entity); query precisa
// de contexto agregado. Profile preenche essa lacuna sem custo per-query.
package profile

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/provider/llm"
	"github.com/nexusyn/engine/internal/tenant"
)

// Profile é o snapshot da org armazenado em user_profiles.
type Profile struct {
	OrganizationID   int64
	Content          string // markdown
	PreferencesCount int
	LessonsCount     int
	BuiltAt          time.Time
}

// rawFact é um preference/lesson cru carregado do grafo, alimentado ao LLM.
type rawFact struct {
	Name     string
	Kind     string // preference | lesson
	Polarity string // like | dislike | neutral
	Category string
	Detail   string
}

// LoadProfile retorna o profile atual da org. Retorna `(Profile{}, nil)`
// quando não existe ainda (read-graceful — query path não bloqueia).
func LoadProfile(ctx context.Context, pool *pgxpool.Pool, orgID int64) (Profile, error) {
	var p Profile
	err := tenant.RunWithTenantReadOnly(ctx, pool, orgID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT organization_id, content, preferences_count, lessons_count, built_at
			FROM user_profiles
			WHERE organization_id = $1
		`, orgID)
		if err := row.Scan(&p.OrganizationID, &p.Content, &p.PreferencesCount, &p.LessonsCount, &p.BuiltAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // empty profile, no error
			}
			return err
		}
		return nil
	})
	return p, err
}

// FormatProfileBlock renderiza o profile como bloco markdown pra prompt.
// Retorna "" quando profile vazio (Content == "" ou nil).
func FormatProfileBlock(p Profile) string {
	if strings.TrimSpace(p.Content) == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("USER PROFILE (synthesized from durable preferences/lessons — anchor recommendations here):\n")
	b.WriteString(p.Content)
	if !strings.HasSuffix(p.Content, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// Build chama o LLM, sintetiza o profile e UPSERT em user_profiles.
//
// Estratégia: SELECT facts → render compact bullet list → LLM produz markdown
// estruturado → UPSERT. Idempotente — pode rodar com qualquer frequência.
//
// Quando a org tem 0 facts, faz UPSERT com Content vazio e Counts=0 (ainda
// mantém o row pra rastrear built_at).
func Build(ctx context.Context, pool *pgxpool.Pool, l llm.Provider, orgID int64) (Profile, error) {
	if l == nil {
		return Profile{}, fmt.Errorf("profile.Build: llm provider nil")
	}
	facts, err := loadFacts(ctx, pool, orgID)
	if err != nil {
		return Profile{}, fmt.Errorf("profile.Build: load facts: %w", err)
	}

	prefCount, lessonCount := countByKind(facts)

	var content string
	if len(facts) > 0 {
		content, err = synthesize(ctx, l, facts)
		if err != nil {
			return Profile{}, fmt.Errorf("profile.Build: synthesize: %w", err)
		}
	}

	p := Profile{
		OrganizationID:   orgID,
		Content:          content,
		PreferencesCount: prefCount,
		LessonsCount:     lessonCount,
		BuiltAt:          time.Now().UTC(),
	}
	if err := upsert(ctx, pool, p); err != nil {
		return Profile{}, fmt.Errorf("profile.Build: upsert: %w", err)
	}
	return p, nil
}

func loadFacts(ctx context.Context, pool *pgxpool.Pool, orgID int64) ([]rawFact, error) {
	var facts []rawFact
	err := tenant.RunWithTenantReadOnly(ctx, pool, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT name, kind,
			       COALESCE(attributes->>'polarity', '') AS polarity,
			       COALESCE(attributes->>'category', '') AS category,
			       COALESCE(attributes->>'detail', '')   AS detail
			FROM entities
			WHERE kind IN ('preference', 'lesson')
			ORDER BY kind, transaction_time DESC
			LIMIT 200
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f rawFact
			if err := rows.Scan(&f.Name, &f.Kind, &f.Polarity, &f.Category, &f.Detail); err != nil {
				return err
			}
			facts = append(facts, f)
		}
		return rows.Err()
	})
	return facts, err
}

func countByKind(facts []rawFact) (preferences, lessons int) {
	for _, f := range facts {
		switch f.Kind {
		case "preference":
			preferences++
		case "lesson":
			lessons++
		}
	}
	return
}

// renderFactsForLLM produz uma lista bullet compacta pra alimentar o LLM
// como contexto. Mantém polaridade + categoria + detail explícitos.
func renderFactsForLLM(facts []rawFact) string {
	var b strings.Builder
	for _, f := range facts {
		b.WriteString("- [")
		b.WriteString(f.Kind)
		b.WriteString("] ")
		if f.Polarity != "" && f.Polarity != "neutral" {
			b.WriteString(strings.ToUpper(f.Polarity))
			b.WriteString(": ")
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

func synthesize(ctx context.Context, l llm.Provider, facts []rawFact) (string, error) {
	system := `You synthesize a USER PROFILE document from raw preference/lesson facts.

Output format — Markdown with these sections (omit any section with no facts):

## Likes
- bullet of things the user enjoys, with category in parentheses when present

## Dislikes / Avoids
- bullet of things the user dislikes or actively avoids

## Style & Habits
- recurring behaviors, working preferences, defaults the user lives by

## Lessons learned
- durable lessons (kind=lesson) — short maxim form

Rules:
- Be terse. No preamble. No "user prefers" — just state the fact.
- Group thematically. Combine related bullets ("Loves cold brew and espresso" rather than two lines).
- Preserve specificity (categories, exclusions) — don't generalize away nuance.
- Output ONLY the markdown. No code fences. No explanation.`

	user := "Raw facts:\n\n" + renderFactsForLLM(facts) + "\nSynthesize the USER PROFILE now."

	res, err := l.Complete(ctx, llm.Prompt{
		System:      system,
		User:        user,
		MaxTokens:   1024,
		Temperature: 0.2,
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Content), nil
}

func upsert(ctx context.Context, pool *pgxpool.Pool, p Profile) error {
	return tenant.RunWithTenant(ctx, pool, p.OrganizationID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO user_profiles (organization_id, content, preferences_count, lessons_count, built_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, NOW())
			ON CONFLICT (organization_id) DO UPDATE SET
			    content = EXCLUDED.content,
			    preferences_count = EXCLUDED.preferences_count,
			    lessons_count = EXCLUDED.lessons_count,
			    built_at = EXCLUDED.built_at,
			    updated_at = NOW()
		`, p.OrganizationID, p.Content, p.PreferencesCount, p.LessonsCount, p.BuiltAt)
		return err
	})
}

// OrgsWithFacts retorna IDs de orgs com pelo menos 1 entity preference/lesson.
// Usado pelo PeriodicJob scheduler pra saber quais profiles rebuildar.
//
// Chama list_orgs_with_profile_facts() — SECURITY DEFINER function (migration
// 0014) com owner nexus_service (BYPASSRLS). Caller é nexus_app, sem
// elevação de privilégio aqui no Go.
func OrgsWithFacts(ctx context.Context, pool *pgxpool.Pool) ([]int64, error) {
	rows, err := pool.Query(ctx, `SELECT organization_id FROM list_orgs_with_profile_facts()`)
	if err != nil {
		return nil, fmt.Errorf("profile.OrgsWithFacts: %w", err)
	}
	defer rows.Close()
	var orgs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		orgs = append(orgs, id)
	}
	return orgs, rows.Err()
}
