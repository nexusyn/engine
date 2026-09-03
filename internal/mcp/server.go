// Package mcp expõe a memória do NEXUS via Model Context Protocol, permitindo
// que agentes (Claude Code, Cursor, Hermes, openclaw) salvem e busquem
// memórias. Servidor HTTP streamable montado em /v1/mcp atrás do auth.Middleware
// — cada request resolve a org pelo Bearer token (multi-tenant, stateless).
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/riverqueue/river"
	"golang.org/x/time/rate"

	domains "github.com/nexusyn/engine/internal/core/domain"
	"github.com/nexusyn/engine/internal/core/graph"
	"github.com/nexusyn/engine/internal/core/guideline"
	"github.com/nexusyn/engine/internal/core/ingest"
	"github.com/nexusyn/engine/internal/core/query"
	"github.com/nexusyn/engine/internal/job"
	"github.com/nexusyn/engine/internal/metering"
	"github.com/nexusyn/engine/internal/tenant"
)

// Deps são as dependências do servidor MCP.
type Deps struct {
	QuerySvc *query.Service
	Insert   *river.Client[pgx.Tx] // insert-only (enfileira IngestJob)
	Pool     *pgxpool.Pool         // resolve agent (find-or-create) + metering
	Version  string
}

type addMemoryIn struct {
	Content string `json:"content" jsonschema:"texto da memória a salvar (fato, preferência, decisão, lição)"`
	Title   string `json:"title,omitempty" jsonschema:"título curto opcional"`
	Domain  string `json:"domain,omitempty" jsonschema:"opcional, default 'memory'. Só ENTRADA: 'memory' (observação bruta — quase sempre esta), 'knowledge' (referência curada), 'guideline' (padrão obrigatório) ou 'skill' (catálogo de skill no formato SKILL.md). NÃO use wiki/lesson/decision/error nem domínios próprios: esses são DERIVADOS automaticamente das memórias pelo compile; qualquer valor fora da lista vira 'memory'."`
	Agent   string `json:"agent,omitempty" jsonschema:"slug da IA que está salvando (ex: claude, cursor, gemini) — agrupa a memória por agente"`
	Project string `json:"project,omitempty" jsonschema:"slug do projeto (ex: nexusyn, reachyn) — segmenta a memória por projeto dentro da org; vazio = global/geral"`
}

// normalizeInputDomain protege o pipeline: vazio ou domínio fora da lista de
// entrada (memory/knowledge/guideline/skill) — inclusive os derivados wiki/lesson/decision/
// error e qualquer custom — vira "memory". Fonte única em internal/core/domain
// (mesma regra aplicada também no ingest HTTP/worker). Sem isso, salvar com domain
// errado some das telas e trava a destilação (incidente 2026-06-12: domain=project).
func normalizeInputDomain(d string) string {
	return domains.NormalizeInput(d)
}

// maxMemoryBytes limita o tamanho do conteúdo aceito por add_memory/update_memory
// via MCP. Sem teto, um agente (ou prompt injection) pode despejar payloads
// enormes → custo de embedding/armazenamento e DoS. 256 KB cobre memórias
// legítimas com folga.
const maxMemoryBytes = 256 * 1024

// hasWrite gate de escrita: as tools de mutação (add/update/delete) exigem a
// ability "write" (ou "*"). Sem isto, qualquer token que alcança /v1/mcp tem
// CRUD total — não dá pra mintar uma key read-only.
func hasWrite(ctx context.Context) bool {
	return tenant.HasAbility(tenant.AbilitiesFromContext(ctx), "write")
}

// orgSuspended bloqueia o data-plane do MCP de uma org suspensa (AUD-010, paridade com o
// gate HTTP de query/search/ingest). Usa is_org_suspended (migration 0030). FAIL-OPEN:
// erro no check NÃO bloqueia (resiliência — bug nosso não derruba o agente).
func orgSuspended(ctx context.Context, pool *pgxpool.Pool, orgID int64) bool {
	if pool == nil {
		return false
	}
	var s bool
	if err := pool.QueryRow(ctx, "SELECT is_org_suspended($1)", orgID).Scan(&s); err != nil {
		return false
	}
	return s
}

// auditEvent grava um evento append-only na tabela `events` (audit log com RLS).
// DEVE rodar dentro de uma tx com tenant setado (RLS FORCE em events) — para as
// mutações é a MESMA tx, então a auditoria é atômica com a ação. Best-effort: um
// erro de auditoria loga mas não derruba a operação. Injeta sempre o token_id do
// ATOR (quem chamou a tool) no payload — sem isso a forense liga o quê/quando mas
// não o quem (agent_id é 0 em update/delete, que não recebem `agent`).
func auditEvent(ctx context.Context, tx pgx.Tx, orgID, agentID int64, kind string, payload map[string]any) {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["token_id"] = tenant.TokenIDFromContext(ctx)
	pj, _ := json.Marshal(payload)
	var agentArg any
	if agentID > 0 {
		agentArg = agentID
	}
	if _, e := tx.Exec(ctx,
		`INSERT INTO events (organization_id, agent_id, kind, payload) VALUES ($1, $2, $3, $4)`,
		orgID, agentArg, kind, pj); e != nil {
		slog.Warn("mcp: audit event insert falhou", "kind", kind, "org_id", orgID, "err", e)
	}
}

// auditKind escolhe o `kind` do evento conforme o domínio. Writes de guideline
// (control-plane que dirige todos os agentes da org) ganham um kind dedicado
// ("guideline.added/updated/deleted") pra serem alertáveis/filtráveis à parte das
// memórias comuns. (R3 da reauditoria.)
func auditKind(domain, action string) string {
	if domain == "guideline" {
		return "guideline." + action
	}
	return "memory." + action
}

// clampSearchLimit limita o `limit` do search_memory: default 20 e TETO 100. O
// default 20 é o sweet spot validado no LongMemEval (idêntico ao /v1/query e ao
// LME_LIMIT do bench) — o piso anterior de 5 fazia o recall via MCP (a memória que
// os agentes de fato usam) ver só 5 chunks e perder fatos que o bench, rodando com
// 20, media como presentes. Sem o teto, um limit gigante propaga pra busca
// vetorial/rerank → query cara (DoS de custo/latência). (R1 da reauditoria.)
func clampSearchLimit(n int) int {
	if n <= 0 {
		return 20
	}
	if n > 100 {
		return 100
	}
	return n
}

// maxSnippetBytes limita o trecho cru de cada fonte devolvido por search_memory.
// O snippet preserva o fato EXATO (host, id, número, endpoint) que a `answer`
// destilada pode resumir e perder — o agente lê o verbatim, não só o resumo.
const maxSnippetBytes = 400

// snippet recorta o conteúdo cru de um chunk pra caber na resposta sem quebrar no
// meio de uma palavra. Vazio → vazio (omitido no JSON).
func snippet(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxSnippetBytes {
		return s
	}
	cut := s[:maxSnippetBytes]
	if i := strings.LastIndexByte(cut, ' '); i > maxSnippetBytes/2 {
		cut = cut[:i]
	}
	return cut + "…"
}

// maxTitleBytes limita o título aceito via MCP (R7). Título é cosmético; truncar
// é mais amigável que rejeitar.
const maxTitleBytes = 500

// maxAgentSlugBytes limita o slug do agente (R7) — ResolveAgent é find-or-create,
// então slugs arbitrariamente longos só sujam a tabela de agents.
const maxAgentSlugBytes = 64

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// auditSearch liga a auditoria de leitura (search_memory) na tabela events.
// Default ON; desligar via MCP_AUDIT_SEARCH=false se o volume incomodar.
var auditSearch = envBool("MCP_AUDIT_SEARCH", true)

// Throttle de MUTAÇÃO por token (in-memory). A quota mensal cobre só `query`;
// add/update/delete não tinham limite → um agente comprometido/prompt-injected
// podia apagar/sobrescrever memórias em loop. Token bucket por token_id corta o
// loop sem afetar uso normal. Reseta no restart (ok p/ anti-abuso). Configurável
// via MCP_MUTATION_RPM (default 60/min; 0 desliga).
var (
	mutLimiters   = map[int64]*rate.Limiter{}
	mutLimitersMu sync.Mutex
	mutRPM        = envInt("MCP_MUTATION_RPM", 60)
)

func allowMutation(ctx context.Context) bool {
	tokenID := tenant.TokenIDFromContext(ctx)
	if tokenID == 0 || mutRPM <= 0 {
		return true
	}
	mutLimitersMu.Lock()
	lim, ok := mutLimiters[tokenID]
	if !ok {
		lim = rate.NewLimiter(rate.Limit(float64(mutRPM)/60.0), mutRPM/2+1)
		mutLimiters[tokenID] = lim
	}
	mutLimitersMu.Unlock()
	return lim.Allow()
}

// Throttle de LEITURA por token — baseline SEMPRE ativo, independente de
// NEXUS_ENFORCE_LIMITS (billing). Mesmo padrão do allowMutation acima, mas para
// search_memory/get_related/graph_overview: essas tools já custam embed+rerank+
// LLM (search_memory) ou travessia de grafo, e hoje não tinham NENHUM teto de
// throughput fora da quota mensal (que só bloqueia com enforcement ligado — OFF
// em prod). Mesmo nome de env do baseline HTTP equivalente
// (internal/api/ratelimit.go), consistente pro operador configurar um valor só:
// NEXUS_QUERY_RPM (default 120/min por token; 0 desliga).
var (
	readLimiters   = map[int64]*rate.Limiter{}
	readLimitersMu sync.Mutex
	readRPM        = envInt("NEXUS_QUERY_RPM", 120)
)

func allowRead(ctx context.Context) bool {
	tokenID := tenant.TokenIDFromContext(ctx)
	if tokenID == 0 || readRPM <= 0 {
		return true
	}
	readLimitersMu.Lock()
	lim, ok := readLimiters[tokenID]
	if !ok {
		lim = rate.NewLimiter(rate.Limit(float64(readRPM)/60.0), readRPM/2+1)
		readLimiters[tokenID] = lim
	}
	readLimitersMu.Unlock()
	return lim.Allow()
}

// hasFlag testa pertencimento EXATO de uma ability (sem expandir o curinga "*").
// Usado para flags-deny aditivas (ex.: "no_delete"): um token "*" NÃO contém
// literalmente "no_delete", então não é barrado — só quem traz a flag explícita.
func hasFlag(ctx context.Context, flag string) bool {
	for _, a := range tenant.AbilitiesFromContext(ctx) {
		if a == flag {
			return true
		}
	}
	return false
}

type addMemoryOut struct {
	JobID  int64  `json:"job_id"`
	Status string `json:"status"`
}

type searchIn struct {
	Query   string `json:"query" jsonschema:"o que recuperar da memória do usuário"`
	Limit   int    `json:"limit,omitempty" jsonschema:"nº de fontes a considerar (default 20, máx 100)"`
	Project string `json:"project,omitempty" jsonschema:"filtra a busca por projeto (traz o projeto + as memórias globais sem projeto); vazio = busca em tudo"`
}

type sourceRef struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	Domain  string `json:"domain,omitempty"`
	Project string `json:"project,omitempty"` // "" = global
	Snippet string `json:"snippet,omitempty"` // trecho cru do chunk top da fonte — preserva o fato exato que a answer pode destilar/omitir
}

// guidelineItem é uma guideline (padrão obrigatório) da org — memória de domain="guideline".
// Alias do tipo canônico em core/guideline (fonte única reusada por MCP e HTTP).
type guidelineItem = guideline.Item

type searchOut struct {
	// Guideline vem FIXADA no topo de toda busca: padrões obrigatórios da org que o
	// agente deve seguir SEMPRE, independente de a busca casar. Ler antes de agir.
	Guideline []guidelineItem `json:"guideline,omitempty"`
	Answer    string          `json:"answer"`
	Sources   []sourceRef     `json:"sources,omitempty"`
}

type getGuidelineIn struct{}

type getGuidelineOut struct {
	Guideline []guidelineItem `json:"guideline"`
	Count     int             `json:"count"`
}

type getRelatedIn struct {
	Entity   string `json:"entity" jsonschema:"nome da entidade/conceito a explorar no grafo (match aproximado por similaridade)"`
	MaxDepth int    `json:"max_depth,omitempty" jsonschema:"nº de hops a partir da entidade (default 2, máx 5)"`
}

// relatedNode é uma entidade vizinha no grafo, com a relação e a distância.
type relatedNode struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	Relation string   `json:"relation"` // edge kind que conectou
	Depth    int      `json:"depth"`
	Path     []string `json:"path,omitempty"`
}

type getRelatedOut struct {
	Seed    string        `json:"seed"` // nome canônico da entidade resolvida
	Related []relatedNode `json:"related,omitempty"`
	Count   int           `json:"count"`
}

type graphOverviewIn struct {
	TopN int `json:"top_n,omitempty" jsonschema:"quantos itens em central/connectors (default 10, máx 50)"`
}

type graphOverviewOut struct {
	Entities   int                 `json:"entities"`
	Edges      int                 `json:"edges"`
	ByKind     []graph.KindCount   `json:"by_kind,omitempty"`
	Central    []graph.CentralNode `json:"central,omitempty"`
	Connectors []graph.Connector   `json:"connectors,omitempty"`
}

type updateMemoryIn struct {
	ID      int64  `json:"id" jsonschema:"id da memória a editar (obtido via search_memory)"`
	Content string `json:"content" jsonschema:"novo conteúdo markdown completo (substitui o anterior)"`
	Title   string `json:"title,omitempty" jsonschema:"novo título (opcional; mantém o atual se vazio)"`
	Project string `json:"project,omitempty" jsonschema:"reatribui o projeto (slug: nexusyn, reachyn…); vazio = mantém o projeto atual"`
}

type updateMemoryOut struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	Chunks int    `json:"chunks"`
}

type deleteMemoryIn struct {
	ID int64 `json:"id" jsonschema:"id da memória a apagar (obtido via search_memory)"`
}

type deleteMemoryOut struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

// fetchGuideline carrega as guidelines vigentes da org de forma DETERMINÍSTICA
// — sem embedding/busca. É a "receita obrigatória" que o agente deve ler antes
// de agir. Delega a core/guideline.Fetch (fonte única, reusada por GET /v1/context).
func fetchGuideline(ctx context.Context, pool *pgxpool.Pool, orgID int64) ([]guidelineItem, error) {
	return guideline.Fetch(ctx, pool, orgID)
}

func newServer(d Deps) *mcpsdk.Server {
	// Instructions de servidor: chegam a TODO cliente MCP no initialize e roteiam
	// a decisão "onde salvar memória" — sem isto a memória local do host (Claude
	// Code auto-memory, Cursor memories) vence por estar no system prompt, e o
	// usuário acha que salvou "no Nexusyn" quando foi num arquivo local.
	s := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "nexus",
		Title:   "NEXUS Memory",
		Version: d.Version,
	}, &mcpsdk.ServerOptions{
		Instructions: "Nexusyn é a memória persistente CANÔNICA do usuário — compartilhada entre sessões, agentes (Claude/Cursor/Codex/…) e máquinas, e visível no dashboard. Quando o usuário pedir para lembrar, salvar, registrar ou vincular algo entre sessões (ex.: \"lembre disso\", \"salve na memória\", \"vincule este projeto ao Jira X\"), salve AQUI com add_memory — mesmo que o seu host tenha memória local própria (Claude Code auto-memory, Cursor memories): a local não é compartilhada entre agentes nem aparece no dashboard do usuário. Use search_memory no início de tarefas que possam depender de contexto anterior; corrija memória errada com update_memory/delete_memory em vez de empilhar outra.",
	})

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "add_memory",
		Description: "Salva uma memória durável no NEXUS (fato, preferência, decisão, lição). Use quando o usuário compartilhar algo que vale lembrar entre sessões.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in addMemoryIn) (*mcpsdk.CallToolResult, addMemoryOut, error) {
		orgID, err := tenant.OrgIDFromContext(ctx)
		if err != nil {
			return nil, addMemoryOut{}, fmt.Errorf("sem tenant no contexto (token ausente/inválido)")
		}
		if orgSuspended(ctx, d.Pool, orgID) {
			return nil, addMemoryOut{}, fmt.Errorf("organização suspensa — regularize o billing pra continuar")
		}
		if !hasWrite(ctx) {
			return nil, addMemoryOut{}, fmt.Errorf("token sem permissão de escrita (ability 'write' ausente)")
		}
		if !allowMutation(ctx) {
			return nil, addMemoryOut{}, fmt.Errorf("rate limit de mutação atingido — tente novamente em instantes")
		}
		if strings.TrimSpace(in.Content) == "" {
			return nil, addMemoryOut{}, fmt.Errorf("content é obrigatório")
		}
		if len(in.Content) > maxMemoryBytes {
			return nil, addMemoryOut{}, fmt.Errorf("content excede o limite de %d KB", maxMemoryBytes/1024)
		}
		if len(in.Agent) > maxAgentSlugBytes {
			return nil, addMemoryOut{}, fmt.Errorf("agent slug excede %d caracteres", maxAgentSlugBytes)
		}
		domain := normalizeInputDomain(in.Domain) // só memory/knowledge/guideline; resto vira memory
		title := in.Title
		if title == "" {
			title = "Untitled"
		}
		if len(title) > maxTitleBytes {
			title = title[:maxTitleBytes]
		}
		// Quota de storage (gated por NEXUS_ENFORCE_LIMITS; OFF por padrão). Espelha
		// o /v1/ingest HTTP — sem isto, add_memory via MCP furava o teto do plano.
		// Fail-open: erro de checagem nunca bloqueia.
		if metering.Enforced() && d.Pool != nil {
			var maxPages, used int64
			_ = tenant.RunWithTenantReadOnly(ctx, d.Pool, orgID, func(tx pgx.Tx) error {
				_ = tx.QueryRow(ctx, `SELECT coalesce(max_pages, 0) FROM org_limits WHERE organization_id = $1`, orgID).Scan(&maxPages)
				if maxPages > 0 {
					_ = tx.QueryRow(ctx, `SELECT count(*) FROM pages WHERE organization_id = $1`, orgID).Scan(&used)
				}
				return nil
			})
			if maxPages > 0 && used >= maxPages {
				return nil, addMemoryOut{}, fmt.Errorf("storage limit reached for your plan — upgrade to add more memories")
			}
		}
		// Resolve a IA fonte (find-or-create) → amarra a memória ao agent.
		var agentID int64
		if in.Agent != "" && d.Pool != nil {
			agentID, _ = metering.ResolveAgent(ctx, d.Pool, orgID, in.Agent)
		}
		res, err := d.Insert.Insert(ctx, job.IngestArgs{
			OrganizationID: orgID,
			AgentID:        agentID,
			Title:          title,
			Content:        in.Content,
			Domain:         domain,
			Project:        metering.SanitizeProjectSlug(in.Project),
		}, &river.InsertOpts{})
		if err != nil {
			return nil, addMemoryOut{}, fmt.Errorf("enqueue: %w", err)
		}
		if d.Pool != nil {
			metering.Record(ctx, d.Pool, orgID, agentID, "ingest", "", "", 0, map[string]any{"via": "mcp"})
			_ = tenant.RunWithTenant(ctx, d.Pool, orgID, func(tx pgx.Tx) error {
				auditEvent(ctx, tx, orgID, agentID, auditKind(domain, "added"), map[string]any{
					"via": "mcp", "job_id": res.Job.ID, "domain": domain, "title": title,
				})
				return nil
			})
		}
		return nil, addMemoryOut{JobID: res.Job.ID, Status: "queued"}, nil
	})

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "search_memory",
		Description: "Busca na memória do NEXUS e retorna resposta grounded + fontes. Use pra lembrar fatos/preferências do usuário antes de responder.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in searchIn) (*mcpsdk.CallToolResult, searchOut, error) {
		orgID, err := tenant.OrgIDFromContext(ctx)
		if err != nil {
			return nil, searchOut{}, fmt.Errorf("sem tenant no contexto (token ausente/inválido)")
		}
		if orgSuspended(ctx, d.Pool, orgID) {
			return nil, searchOut{}, fmt.Errorf("organização suspensa — regularize o billing pra continuar")
		}
		if !allowRead(ctx) {
			return nil, searchOut{}, fmt.Errorf("rate limit de leitura atingido — tente novamente em instantes")
		}
		if strings.TrimSpace(in.Query) == "" {
			return nil, searchOut{}, fmt.Errorf("query é obrigatória")
		}
		// Quota mensal de queries — MCP passa pelo mesmo contador do HTTP.
		if d.Pool != nil {
			if q := metering.CheckAndIncrQuery(ctx, d.Pool, orgID, metering.Enforced()); !q.Allowed {
				return nil, searchOut{}, fmt.Errorf(
					"monthly query limit reached (%d/%d) — upgrade your plan for more queries", q.Used, q.Max)
			}
		}
		limit := clampSearchLimit(in.Limit)
		resp, err := d.QuerySvc.Query(ctx, orgID, query.Options{
			Question: in.Query,
			Limit:    limit,
			Mode:     "hybrid",
			Project:  metering.SanitizeProjectSlug(in.Project),
		})
		if err != nil {
			return nil, searchOut{}, fmt.Errorf("query: %w", err)
		}
		seen := map[int64]bool{}
		var srcs []sourceRef
		for _, r := range resp.Sources {
			if r.PageID == 0 || seen[r.PageID] {
				continue
			}
			seen[r.PageID] = true
			// resp.Sources já vem ordenado por relevância (pós-rerank), então o
			// primeiro chunk de cada page é o mais relevante dela — o snippet cru
			// garante que o fato exato chega ao agente mesmo se a answer o resumir.
			srcs = append(srcs, sourceRef{ID: r.PageID, Title: r.PageTitle, Domain: r.Domain, Project: r.Project, Snippet: snippet(r.Content)})
		}
		// Metering: search_memory não metrificava (uso MCP cego no relatório).
		if d.Pool != nil {
			metering.Record(ctx, d.Pool, orgID, 0, "query", resp.Usage.LLMProvider, resp.Usage.LLMModel,
				resp.Usage.LatencyMs, map[string]any{"via": "mcp"})
			// Auditoria de LEITURA: registra o que foi buscado + ids retornados, para
			// forense de exfiltração (metering só conta, não diz o quê). Query truncada
			// pra limitar PII/tamanho no log. (Pode desligar via MCP_AUDIT_SEARCH=false.)
			if auditSearch {
				ids := make([]int64, 0, len(srcs))
				for _, sr := range srcs {
					ids = append(ids, sr.ID)
				}
				q := in.Query
				if len(q) > 500 {
					q = q[:500]
				}
				_ = tenant.RunWithTenant(ctx, d.Pool, orgID, func(tx pgx.Tx) error {
					auditEvent(ctx, tx, orgID, 0, "memory.searched", map[string]any{
						"via": "mcp", "query": q, "result_ids": ids, "count": len(ids),
					})
					return nil
				})
			}
		}
		dir, _ := fetchGuideline(ctx, d.Pool, orgID) // fixa a guideline no topo (best-effort)
		return nil, searchOut{Guideline: dir, Answer: resp.Answer, Sources: srcs}, nil
	})

	// get_guideline: retorna as guidelinees vigentes da org de forma DETERMINÍSTICA
	// (sem busca/embedding). A "receita obrigatória" — ler no início, antes de agir.
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "get_guideline",
		Description: "Retorna as guidelines vigentes da org (padrões obrigatórios) de forma determinística, sem busca. Chame no início pra saber o que é padrão antes de agir. As guidelines também vêm fixadas no topo de search_memory. Edite-as com add_memory/update_memory/delete_memory usando domain=\"guideline\".",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ getGuidelineIn) (*mcpsdk.CallToolResult, getGuidelineOut, error) {
		orgID, err := tenant.OrgIDFromContext(ctx)
		if err != nil {
			return nil, getGuidelineOut{}, fmt.Errorf("sem tenant no contexto (token ausente/inválido)")
		}
		items, err := fetchGuideline(ctx, d.Pool, orgID)
		if err != nil {
			return nil, getGuidelineOut{}, fmt.Errorf("get_guideline: %w", err)
		}
		return nil, getGuidelineOut{Guideline: items, Count: len(items)}, nil
	})

	// update_memory: edita uma memória IN-PLACE e re-embeda. Espelha
	// MemoryUpdateHandler (api/memory_edit.go). Para CORRIGIR memória obsoleta
	// em vez de empilhar outra contradizendo. Pegue o id via search_memory.
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "update_memory",
		Description: "Edita uma memória existente IN-PLACE (substitui o conteúdo e re-embeda, então a busca passa a refletir). Use pra CORRIGIR memória desatualizada em vez de adicionar outra contradizendo. Também reatribui o projeto (campo project; vazio mantém o atual). Obtenha o id via search_memory.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in updateMemoryIn) (*mcpsdk.CallToolResult, updateMemoryOut, error) {
		orgID, err := tenant.OrgIDFromContext(ctx)
		if err != nil {
			return nil, updateMemoryOut{}, fmt.Errorf("sem tenant no contexto (token ausente/inválido)")
		}
		if orgSuspended(ctx, d.Pool, orgID) {
			return nil, updateMemoryOut{}, fmt.Errorf("organização suspensa — regularize o billing pra continuar")
		}
		if !hasWrite(ctx) {
			return nil, updateMemoryOut{}, fmt.Errorf("token sem permissão de escrita (ability 'write' ausente)")
		}
		if !allowMutation(ctx) {
			return nil, updateMemoryOut{}, fmt.Errorf("rate limit de mutação atingido — tente novamente em instantes")
		}
		if in.ID <= 0 {
			return nil, updateMemoryOut{}, fmt.Errorf("id é obrigatório")
		}
		if strings.TrimSpace(in.Content) == "" {
			return nil, updateMemoryOut{}, fmt.Errorf("content é obrigatório")
		}
		if len(in.Content) > maxMemoryBytes {
			return nil, updateMemoryOut{}, fmt.Errorf("content excede o limite de %d KB", maxMemoryBytes/1024)
		}
		chunks := ingest.ChunkText(in.Content, ingest.DefaultChunkConfig())
		found := false
		err = tenant.RunWithTenant(ctx, d.Pool, orgID, func(tx pgx.Tx) error {
			// Lê o estado atual ANTES de sobrescrever: para guardar o conteúdo
			// anterior no audit (update é in-place, então o snapshot no events é o
			// que torna a operação recuperável).
			var prevDomain, prevTitle, prevContent, prevProject string
			e := tx.QueryRow(ctx,
				`SELECT domain, title, content, COALESCE(project, '') FROM pages
				 WHERE id = $1 AND organization_id = $2 AND valid_to IS NULL`,
				in.ID, orgID).Scan(&prevDomain, &prevTitle, &prevContent, &prevProject)
			if errors.Is(e, pgx.ErrNoRows) {
				return nil // found=false → 404 amigável
			}
			if e != nil {
				return e
			}
			if _, e := tx.Exec(ctx,
				`UPDATE pages SET content = $2, title = COALESCE(NULLIF($3, ''), title),
				        project = COALESCE(NULLIF($5, ''), project)
				 WHERE id = $1 AND organization_id = $4 AND valid_to IS NULL`,
				in.ID, in.Content, in.Title, orgID, metering.SanitizeProjectSlug(in.Project)); e != nil {
				return e
			}
			found = true
			if _, e := tx.Exec(ctx, `DELETE FROM chunks WHERE page_id = $1 AND organization_id = $2`, in.ID, orgID); e != nil {
				return e
			}
			for _, c := range chunks {
				if _, e := tx.Exec(ctx,
					`INSERT INTO chunks (organization_id, page_id, position, content) VALUES ($1, $2, $3, $4)`,
					orgID, in.ID, c.Position, c.Content); e != nil {
					return e
				}
			}
			auditEvent(ctx, tx, orgID, 0, auditKind(prevDomain, "updated"), map[string]any{
				"via": "mcp", "page_id": in.ID, "domain": prevDomain,
				"prev_title": prevTitle, "prev_content": prevContent, "prev_project": prevProject,
			})
			return nil
		})
		if err != nil {
			return nil, updateMemoryOut{}, fmt.Errorf("update: %w", err)
		}
		if !found {
			return nil, updateMemoryOut{}, fmt.Errorf("memória %d não encontrada", in.ID)
		}
		if d.Insert != nil {
			_ = job.EnqueueEmbedBatch(ctx, d.Insert, 1*time.Second)
		}
		return nil, updateMemoryOut{ID: in.ID, Status: "updated", Chunks: len(chunks)}, nil
	})

	// delete_memory: soft-delete bi-temporal (valid_to=now()) + remove chunks do
	// recall. Espelha MemoryDeleteHandler (api/memory_edit.go). Mantém histórico.
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "delete_memory",
		Description: "Apaga uma memória da busca (soft-delete + remove os chunks do recall, mantendo histórico). Use pra remover memória errada/obsoleta. Obtenha o id via search_memory.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in deleteMemoryIn) (*mcpsdk.CallToolResult, deleteMemoryOut, error) {
		orgID, err := tenant.OrgIDFromContext(ctx)
		if err != nil {
			return nil, deleteMemoryOut{}, fmt.Errorf("sem tenant no contexto (token ausente/inválido)")
		}
		if orgSuspended(ctx, d.Pool, orgID) {
			return nil, deleteMemoryOut{}, fmt.Errorf("organização suspensa — regularize o billing pra continuar")
		}
		if !hasWrite(ctx) {
			return nil, deleteMemoryOut{}, fmt.Errorf("token sem permissão de escrita (ability 'write' ausente)")
		}
		// Granularidade opt-in: um token pode trazer a flag "no_delete" para gravar
		// (add/update) mas NUNCA apagar — menor privilégio para agentes que só
		// deveriam acumular memória. Tokens normais/"*" não têm a flag → apagam.
		if hasFlag(ctx, "no_delete") {
			return nil, deleteMemoryOut{}, fmt.Errorf("este token não pode apagar (ability 'no_delete')")
		}
		if !allowMutation(ctx) {
			return nil, deleteMemoryOut{}, fmt.Errorf("rate limit de mutação atingido — tente novamente em instantes")
		}
		if in.ID <= 0 {
			return nil, deleteMemoryOut{}, fmt.Errorf("id é obrigatório")
		}
		found := false
		err = tenant.RunWithTenant(ctx, d.Pool, orgID, func(tx pgx.Tx) error {
			// Lê domain+title antes pra auditar o que foi removido.
			var prevDomain, prevTitle string
			e := tx.QueryRow(ctx,
				`SELECT domain, title FROM pages
				 WHERE id = $1 AND organization_id = $2 AND valid_to IS NULL`,
				in.ID, orgID).Scan(&prevDomain, &prevTitle)
			if errors.Is(e, pgx.ErrNoRows) {
				return nil // found=false → 404 amigável
			}
			if e != nil {
				return e
			}
			ct, e := tx.Exec(ctx,
				`UPDATE pages SET valid_to = now() WHERE id = $1 AND organization_id = $2 AND valid_to IS NULL`,
				in.ID, orgID)
			if e != nil {
				return e
			}
			if ct.RowsAffected() == 0 {
				return nil
			}
			found = true
			if _, e := tx.Exec(ctx, `DELETE FROM chunks WHERE page_id = $1 AND organization_id = $2`, in.ID, orgID); e != nil {
				return e
			}
			auditEvent(ctx, tx, orgID, 0, auditKind(prevDomain, "deleted"), map[string]any{
				"via": "mcp", "page_id": in.ID, "domain": prevDomain, "title": prevTitle,
			})
			return nil
		})
		if err != nil {
			return nil, deleteMemoryOut{}, fmt.Errorf("delete: %w", err)
		}
		if !found {
			return nil, deleteMemoryOut{}, fmt.Errorf("memória %d não encontrada", in.ID)
		}
		return nil, deleteMemoryOut{ID: in.ID, Status: "deleted"}, nil
	})

	// get_related: explora o knowledge graph (Fase 3 GraphRAG). Resolve a entidade
	// por nome aproximado e expande 1-2 hops via graph.Traverse. Read-only, RLS via
	// contexto. Complementa search_memory (semântico) com a ESTRUTURA do grafo.
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "get_related",
		Description: "Explora o GRAFO de conhecimento: dada uma entidade/conceito (nome aproximado), retorna o que está conectado a ela em 1-2 hops (pessoas, conceitos, eventos, decisões, lições…), com a relação e a distância. Complementa search_memory (que é semântico) com a ESTRUTURA — útil pra 'o que se relaciona com X?' ou pra puxar o contexto vizinho de um tema. Retorna seed vazio se nada casar.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in getRelatedIn) (*mcpsdk.CallToolResult, getRelatedOut, error) {
		orgID, err := tenant.OrgIDFromContext(ctx)
		if err != nil {
			return nil, getRelatedOut{}, fmt.Errorf("sem tenant no contexto (token ausente/inválido)")
		}
		if orgSuspended(ctx, d.Pool, orgID) { // AUD-010: leitura do grafo é data-plane (conteúdo do cliente)
			return nil, getRelatedOut{}, fmt.Errorf("organização suspensa — regularize o billing pra continuar")
		}
		if !allowRead(ctx) {
			return nil, getRelatedOut{}, fmt.Errorf("rate limit de leitura atingido — tente novamente em instantes")
		}
		name := strings.TrimSpace(in.Entity)
		if name == "" {
			return nil, getRelatedOut{}, fmt.Errorf("get_related: 'entity' é obrigatório")
		}
		seedID, canonical, err := graph.ResolveEntityByName(ctx, d.Pool, orgID, name)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, getRelatedOut{Seed: "", Count: 0}, nil // nada casou
			}
			return nil, getRelatedOut{}, fmt.Errorf("get_related: resolve %q: %w", name, err)
		}
		hops, err := graph.Traverse(ctx, d.Pool, orgID, seedID, graph.Options{MaxDepth: in.MaxDepth, MaxResults: 50})
		if err != nil {
			return nil, getRelatedOut{}, fmt.Errorf("get_related: traverse: %w", err)
		}
		out := getRelatedOut{Seed: canonical, Related: make([]relatedNode, 0, len(hops))}
		for _, h := range hops {
			out.Related = append(out.Related, relatedNode{
				Name: h.Name, Kind: h.Kind, Relation: h.EdgeKind, Depth: h.Depth, Path: h.Path,
			})
		}
		out.Count = len(out.Related)
		return nil, out, nil
	})

	// graph_overview: panorama estrutural do grafo da org (Fase 4 GraphRAG).
	// Stats + temas centrais (god-nodes via degree) + conectores (kind diversity).
	// Read-only, RLS via contexto. Visão que a busca semântica não dá.
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "graph_overview",
		Description: "Panorama do GRAFO de conhecimento da org: total de entidades/relações, distribuição por tipo, TEMAS CENTRAIS (entidades mais conectadas) e CONECTORES (que ligam tipos distintos). Visão estrutural que a busca semântica não dá — útil pra 'do que minha memória mais trata?' ou um overview do que o sistema já sabe.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in graphOverviewIn) (*mcpsdk.CallToolResult, graphOverviewOut, error) {
		orgID, err := tenant.OrgIDFromContext(ctx)
		if err != nil {
			return nil, graphOverviewOut{}, fmt.Errorf("sem tenant no contexto (token ausente/inválido)")
		}
		if orgSuspended(ctx, d.Pool, orgID) { // AUD-010: leitura do grafo é data-plane (conteúdo do cliente)
			return nil, graphOverviewOut{}, fmt.Errorf("organização suspensa — regularize o billing pra continuar")
		}
		if !allowRead(ctx) {
			return nil, graphOverviewOut{}, fmt.Errorf("rate limit de leitura atingido — tente novamente em instantes")
		}
		ov, err := graph.GetOverview(ctx, d.Pool, orgID, in.TopN)
		if err != nil {
			return nil, graphOverviewOut{}, fmt.Errorf("graph_overview: %w", err)
		}
		return nil, graphOverviewOut{
			Entities:   ov.Entities,
			Edges:      ov.Edges,
			ByKind:     ov.ByKind,
			Central:    ov.Central,
			Connectors: ov.Connectors,
		}, nil
	})

	return s
}

// maxMCPBodyBytes limita o tamanho do corpo HTTP aceito por /v1/mcp — NEX-001:
// o transporte streamable do SDK MCP lê o body JSON-RPC inteiro na RAM antes de
// qualquer validação de tamanho (json.Decode não tinha nenhum http.MaxBytesReader
// no repo inteiro). Aplicado explicitamente aqui (não só via middleware do
// router HTTP) porque o mux.Handle("/v1/mcp", ...) delega pro SDK, que faz seu
// próprio parsing de body — não dá pra confiar só num decode em internal/api.
// Mesmo nome de env do cap HTTP (internal/api/bodylimit.go), consistente pro
// operador configurar um único valor: NEXUS_MAX_REQUEST_BODY_BYTES (default 8 MiB).
var maxMCPBodyBytes = int64(envInt("NEXUS_MAX_REQUEST_BODY_BYTES", 8*1024*1024))

// Handler retorna o http.Handler MCP (streamable HTTP, stateless). Monte atrás
// do auth.Middleware: cada request HTTP carrega o Bearer token, que resolve a
// org no contexto consumido pelos tools.
func Handler(d Deps) http.Handler {
	srv := newServer(d)
	inner := mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return srv },
		&mcpsdk.StreamableHTTPOptions{Stateless: true},
	)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// http.MaxBytesReader escreve o erro no ResponseWriter só quando o handler
		// downstream tenta ler ALÉM do limite — o SDK MCP trata isso como um erro
		// de leitura normal (sem panic; o middleware.Recoverer do router HTTP é a
		// rede de segurança adicional caso algum path não trate).
		r.Body = http.MaxBytesReader(w, r.Body, maxMCPBodyBytes)
		inner.ServeHTTP(w, r)
	})
}
