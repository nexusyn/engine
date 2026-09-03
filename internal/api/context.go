package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/core/guideline"
	"github.com/nexusyn/engine/internal/core/profile"
	"github.com/nexusyn/engine/internal/tenant"
)

// GET /v1/context monta o "pacote núcleo" de contexto da org — guidelines +
// perfil + índice do projeto — pronto pra um hook SessionStart EMPURRAR (push)
// pro contexto do agente no início da sessão, com 0 tool calls. Complementa o
// retrieval sob demanda (pull) do search_memory: o núcleo curado vem sempre, o
// resto continua via busca.
//
// Read-only, idempotente, tenant-scoped. Gated por billing (org suspensa → 403,
// AUD-010). O pacote renderizado respeita um teto de tokens (anti context-bloat
// e anti-custo) — os campos estruturados vêm completos, o campo `context` vem
// já truncado dentro do budget.
//
// Query params:
//   - project:    filtra o índice do projeto (estrito). Vazio = wiki recente da org.
//   - sections:   CSV allowlist das seções (guidelines,profile,index,recent). Ausente = todas.
//   - recent:     nº de últimas mudanças (default 5, clamp [0,20]; 0 = off).
//   - max_tokens: teto do pacote renderizado (default 1500, clamp [200, 4000]).
//   - format:     "json" (default) ou "markdown" (devolve só o pacote, text/markdown).

const (
	ctxDefaultMaxTokens = 1500
	ctxMinMaxTokens     = 200
	ctxMaxMaxTokens     = 4000
	ctxWikiLimit        = 12  // top-N páginas wiki do índice do projeto
	ctxWikiPreviewChars = 220 // preview por página (chars do content)
	ctxDefaultRecent    = 5   // últimas N mudanças (0 = seção off)
	ctxMaxRecent        = 20
	// ctxCharsPerToken: estimativa chars→tokens. O engine não tem tokenizer; para
	// prosa PT-BR ~4 chars/token é uma aproximação razoável (palavra média ~5
	// chars + espaço ≈ 1,5 token). É um TETO conservador: superdimensionar o
	// pacote em ~25% no pior caso é aceitável (anti context-bloat, não exato).
	ctxCharsPerToken = 4
)

// Seções do pacote (na ordem de render) e o peso de cada uma no budget. Quando o
// caller seleciona um subconjunto via ?sections=, os pesos são renormalizados
// sobre as seções escolhidas — quem fica usa o budget de quem saiu.
var (
	ctxSectionOrder   = []string{"guidelines", "profile", "index", "recent"}
	ctxSectionWeights = map[string]int{"guidelines": 50, "profile": 15, "index": 20, "recent": 15}
)

// ctxParseSections lê ?sections (CSV allowlist das seções). Ausente/vazio → todas.
// Só tokens válidos contam; se nenhum válido for passado, cai em "todas" (evita
// pacote vazio por typo). Retorna o conjunto habilitado.
func ctxParseSections(raw string) map[string]bool {
	all := func() map[string]bool {
		m := map[string]bool{}
		for _, s := range ctxSectionOrder {
			m[s] = true
		}
		return m
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return all()
	}
	enabled := map[string]bool{}
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.ToLower(strings.TrimSpace(tok))
		if _, ok := ctxSectionWeights[tok]; ok {
			enabled[tok] = true
		}
	}
	if len(enabled) == 0 {
		return all() // só tokens inválidos → não devolve pacote vazio
	}
	return enabled
}

type contextProfile struct {
	Content          string `json:"content"`
	PreferencesCount int    `json:"preferences_count"`
	LessonsCount     int    `json:"lessons_count"`
	BuiltAt          string `json:"built_at,omitempty"`
}

type contextWikiPage struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	Preview string `json:"preview"`
}

// contextRecentItem é uma memória recém-criada/editada do projeto ("últimas
// mudanças") — o agente vê o que mudou desde a última sessão.
type contextRecentItem struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	Domain    string `json:"domain"`
	CreatedAt string `json:"created_at"`
}

type contextMeta struct {
	ApproxTokens int      `json:"approx_tokens"` // estimativa do pacote renderizado (chars/4)
	MaxTokens    int      `json:"max_tokens"`    // teto pedido
	Truncated    bool     `json:"truncated"`     // true se alguma seção foi cortada pelo budget
	Sections     []string `json:"sections"`      // seções incluídas (ordem de render)
}

// ContextResponse: campos estruturados vêm COMPLETOS (sem corte); `context` é o
// pacote markdown já renderizado e capado no budget, pronto pra injetar.
type ContextResponse struct {
	OrganizationID int64               `json:"organization_id"`
	Project        string              `json:"project,omitempty"`
	Guidelines     []guideline.Item    `json:"guidelines"`
	Profile        contextProfile      `json:"profile"`
	ProjectPages   []contextWikiPage   `json:"project_pages"`
	RecentChanges  []contextRecentItem `json:"recent_changes"`
	Context        string              `json:"context"`
	Meta           contextMeta         `json:"meta"`
}

// ContextHandler — GET /v1/context.
func ContextHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := tenant.OrgIDFromContext(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no tenant in context")
			return
		}
		if !requireOrgActive(w, r, pool, orgID) { // AUD-010: org suspensa → 403
			return
		}

		project := strings.TrimSpace(r.URL.Query().Get("project"))
		maxTokens := ctxClampMaxTokens(r.URL.Query().Get("max_tokens"))
		recentN := ctxClampRecent(r.URL.Query().Get("recent"))
		enabled := ctxParseSections(r.URL.Query().Get("sections"))

		// Busca SÓ as seções habilitadas (pula o trabalho de DB do que foi excluído).
		var guidelines []guideline.Item
		if enabled["guidelines"] {
			guidelines, err = guideline.Fetch(r.Context(), pool, orgID)
			if err != nil {
				writeInternalError(w, "context guidelines", err)
				return
			}
		}
		var prof profile.Profile
		if enabled["profile"] {
			prof, err = profile.LoadProfile(r.Context(), pool, orgID) // read-graceful
			if err != nil {
				writeInternalError(w, "context profile", err)
				return
			}
		}
		wikiPages := []contextWikiPage{}
		if enabled["index"] {
			wikiPages, err = fetchContextWiki(r.Context(), pool, orgID, project)
			if err != nil {
				writeInternalError(w, "context wiki", err)
				return
			}
		}
		// "recent" precisa estar habilitada E com recentN>0.
		recent := []contextRecentItem{}
		if enabled["recent"] && recentN > 0 {
			recent, err = fetchContextRecent(r.Context(), pool, orgID, project, recentN)
			if err != nil {
				writeInternalError(w, "context recent", err)
				return
			}
		}

		pack, truncated := renderContextPack(orgID, project, guidelines, prof, wikiPages, recent, enabled, maxTokens*ctxCharsPerToken)

		if strings.EqualFold(r.URL.Query().Get("format"), "markdown") {
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(pack))
			return
		}

		profileBuiltAt := ""
		if !prof.BuiltAt.IsZero() {
			profileBuiltAt = prof.BuiltAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		// Seções que de fato entraram (recent só conta com recentN>0).
		sections := []string{}
		for _, s := range ctxSectionOrder {
			if !enabled[s] || (s == "recent" && recentN == 0) {
				continue
			}
			sections = append(sections, s)
		}
		writeJSON(w, http.StatusOK, ContextResponse{
			OrganizationID: orgID,
			Project:        project,
			Guidelines:     guidelines,
			Profile: contextProfile{
				Content:          prof.Content,
				PreferencesCount: prof.PreferencesCount,
				LessonsCount:     prof.LessonsCount,
				BuiltAt:          profileBuiltAt,
			},
			ProjectPages:  wikiPages,
			RecentChanges: recent,
			Context:       pack,
			Meta: contextMeta{
				ApproxTokens: utf8.RuneCountInString(pack) / ctxCharsPerToken,
				MaxTokens:    maxTokens,
				Truncated:    truncated,
				Sections:     sections,
			},
		})
	}
}

// ctxClampMaxTokens lê ?max_tokens e clampa em [ctxMinMaxTokens, ctxMaxMaxTokens].
// Valor ausente/inválido → ctxDefaultMaxTokens.
func ctxClampMaxTokens(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return ctxDefaultMaxTokens
	}
	if n < ctxMinMaxTokens {
		return ctxMinMaxTokens
	}
	if n > ctxMaxMaxTokens {
		return ctxMaxMaxTokens
	}
	return n
}

// ctxClampRecent lê ?recent e clampa em [0, ctxMaxRecent]. Ausente/inválido →
// ctxDefaultRecent. "0" explícito desliga a seção de últimas mudanças.
func ctxClampRecent(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ctxDefaultRecent
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return ctxDefaultRecent
	}
	if n > ctxMaxRecent {
		return ctxMaxRecent
	}
	return n
}

// fetchContextRecent busca as últimas N memórias do projeto (exclui guidelines —
// são regras estáveis, não "mudanças"). Filtro estrito de project, ordenado por
// recência. Backing da seção "Últimas mudanças".
func fetchContextRecent(ctx context.Context, pool *pgxpool.Pool, orgID int64, project string, limit int) ([]contextRecentItem, error) {
	out := []contextRecentItem{}
	err := tenant.RunWithTenantReadOnly(ctx, pool, orgID, func(tx pgx.Tx) error {
		// pageFilter cobre org + valid_to + project; soma o recorte de domain.
		where, args := pageFilter(orgID, "", project, "", "")
		listArgs := append(append([]any{}, args...), limit)
		rows, e := tx.Query(ctx, fmt.Sprintf(
			`SELECT id, title, domain, created_at
			 FROM pages
			 WHERE %s AND domain <> 'guideline'
			 ORDER BY created_at DESC
			 LIMIT $%d`, where, len(args)+1), listArgs...)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var it contextRecentItem
			var createdAt time.Time
			if e := rows.Scan(&it.ID, &it.Title, &it.Domain, &createdAt); e != nil {
				return e
			}
			it.CreatedAt = createdAt.UTC().Format("2006-01-02")
			out = append(out, it)
		}
		return rows.Err()
	})
	return out, err
}

// fetchContextWiki busca as top-N páginas wiki da org (índice do projeto),
// reusando pageFilter (filtro estrito de project, igual ao browse). Ordena por
// recência. project vazio = wiki recente cross-project da org.
func fetchContextWiki(ctx context.Context, pool *pgxpool.Pool, orgID int64, project string) ([]contextWikiPage, error) {
	where, args := pageFilter(orgID, "wiki", project, "", "")
	out := []contextWikiPage{}
	err := tenant.RunWithTenantReadOnly(ctx, pool, orgID, func(tx pgx.Tx) error {
		listArgs := append(append([]any{}, args...), ctxWikiLimit)
		rows, e := tx.Query(ctx, fmt.Sprintf(
			`SELECT id, title, left(content, %d)
			 FROM pages
			 WHERE %s
			 ORDER BY created_at DESC
			 LIMIT $%d`, ctxWikiPreviewChars, where, len(args)+1), listArgs...)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var p contextWikiPage
			if e := rows.Scan(&p.ID, &p.Title, &p.Preview); e != nil {
				return e
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// renderContextPack monta o pacote markdown dentro do budget de runas. Os pesos
// das seções (guidelines 50, perfil 15, índice 20, recentes 15) são RENORMALIZADOS
// sobre as seções habilitadas — quem fica herda o budget de quem saiu. Espaço não
// usado por uma seção dentro do próprio cap NÃO é reaproveitado (mantém compacto).
// Retorna (pacote, truncated).
func renderContextPack(orgID int64, project string, guidelines []guideline.Item, prof profile.Profile, wiki []contextWikiPage, recent []contextRecentItem, enabled map[string]bool, maxRunes int) (string, bool) {
	total := 0
	for _, k := range ctxSectionOrder {
		if enabled[k] {
			total += ctxSectionWeights[k]
		}
	}
	if total == 0 {
		total = 1
	}
	rendered := map[string]string{
		"guidelines": renderGuidelines(guidelines),
		"profile":    renderProfileSection(prof),
		"index":      renderProjectIndex(project, wiki),
		"recent":     renderRecentSection(recent),
	}

	var b strings.Builder
	b.WriteString("# Nexusyn — Pacote de Contexto (org ")
	b.WriteString(strconv.FormatInt(orgID, 10))
	if project != "" {
		b.WriteString(", projeto ")
		b.WriteString(project)
	}
	b.WriteString(")\n\n")

	truncated := false
	for _, k := range ctxSectionOrder {
		if !enabled[k] {
			continue
		}
		capped, t := capRunes(rendered[k], maxRunes*ctxSectionWeights[k]/total)
		truncated = truncated || t
		if capped != "" {
			b.WriteString(capped)
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n", truncated
}

func renderGuidelines(items []guideline.Item) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Guidelines da org (ler e seguir SEMPRE)\n\n")
	for _, it := range items {
		b.WriteString("### ")
		b.WriteString(strings.TrimSpace(it.Title))
		b.WriteString("\n")
		b.WriteString(strings.TrimSpace(it.Content))
		b.WriteString("\n\n")
	}
	return b.String()
}

func renderProfileSection(prof profile.Profile) string {
	if strings.TrimSpace(prof.Content) == "" {
		return ""
	}
	return "## Perfil do usuário/org\n\n" + strings.TrimSpace(prof.Content) + "\n\n"
}

func renderProjectIndex(project string, pages []contextWikiPage) string {
	if len(pages) == 0 {
		return ""
	}
	var b strings.Builder
	if project != "" {
		b.WriteString("## Índice do projeto: " + project + "\n\n")
	} else {
		b.WriteString("## Índice (wiki recente)\n\n")
	}
	for _, p := range pages {
		b.WriteString("- **")
		b.WriteString(strings.TrimSpace(p.Title))
		b.WriteString("**")
		if prev := collapseWS(p.Preview); prev != "" {
			b.WriteString(" — ")
			b.WriteString(prev)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func renderRecentSection(items []contextRecentItem) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Últimas mudanças\n\n")
	for _, it := range items {
		b.WriteString("- ")
		b.WriteString(it.CreatedAt)
		b.WriteString(" · ")
		b.WriteString(strings.TrimSpace(it.Title))
		if it.Domain != "" {
			b.WriteString(" (")
			b.WriteString(it.Domain)
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// capRunes trunca s para no máximo max runas (rune-safe), anexando um marcador
// quando corta. Retorna (texto, truncated). max <= 0 com conteúdo → trunca tudo.
func capRunes(s string, max int) (string, bool) {
	if strings.TrimSpace(s) == "" {
		return "", false
	}
	if max <= 0 {
		return "", true
	}
	if utf8.RuneCountInString(s) <= max {
		return s, false
	}
	r := []rune(s)
	return strings.TrimRight(string(r[:max]), " \t\n") + "\n…[truncado]\n\n", true
}

// collapseWS colapsa runs de espaço/quebra em um único espaço (preview em 1 linha).
func collapseWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
