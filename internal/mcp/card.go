package mcp

import (
	"encoding/json"
	"net/http"
)

// ServerCard retorna a especificação pública do servidor MCP para registries (Smithery, Glama)
// sem necessidade de autenticação prévia.
type ServerCard struct {
	Schema         string           `json:"$schema,omitempty"`
	ServerInfo     ServerInfo       `json:"serverInfo"`
	Authentication ServerAuth       `json:"authentication"`
	Tools          []ServerToolCard `json:"tools"`
}

type ServerInfo struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

type ServerAuth struct {
	Required bool     `json:"required"`
	Schemes  []string `json:"schemes"`
}

type ServerToolCard struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// ServerCardHandler entrega o manifesto /.well-known/mcp/server-card.json
func ServerCardHandler(version string) http.HandlerFunc {
	card := ServerCard{
		Schema: "https://smithery.ai/docs/config#server-card",
		ServerInfo: ServerInfo{
			Name:        "nexusyn",
			Version:     version,
			Description: "Long-term persistent memory for AI agents with hybrid search (DiskANN + FTS), bi-temporal knowledge graph, and 81.1% LongMemEval accuracy.",
		},
		Authentication: ServerAuth{
			Required: true,
			Schemes:  []string{"bearer"},
		},
		Tools: []ServerToolCard{
			{
				Name:        "add_memory",
				Description: "Salva uma memória durável no NEXUS (fato, preferência, decisão, lição). Use quando o usuário compartilhar algo que vale lembrar entre sessões.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content": map[string]any{"type": "string", "description": "texto da memória a salvar (fato, preferência, decisão, lição)"},
						"title":   map[string]any{"type": "string", "description": "título curto opcional"},
						"domain":  map[string]any{"type": "string", "description": "opcional, default 'memory'. 'memory', 'knowledge', 'guideline' ou 'skill'"},
						"agent":   map[string]any{"type": "string", "description": "slug da IA que está salvando (ex: claude, cursor, gemini)"},
						"project": map[string]any{"type": "string", "description": "slug do projeto (ex: nexusyn, reachyn) para segmentação"},
					},
					"required": []string{"content"},
				},
			},
			{
				Name:        "search_memory",
				Description: "Busca na memória com recuperação híbrida (vetor DiskANN + busca textual BM25 + expansão de grafo) e síntese fundamentada com citação de fontes.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query":   map[string]any{"type": "string", "description": "o que recuperar da memória do usuário"},
						"limit":   map[string]any{"type": "integer", "description": "nº de fontes a considerar (default 20, máx 100)"},
						"project": map[string]any{"type": "string", "description": "filtra a busca por projeto (traz o projeto + as memórias globais sem projeto); vazio = busca em tudo"},
					},
					"required": []string{"query"},
				},
			},
			{
				Name:        "get_guideline",
				Description: "Retorna as guidelines vigentes da org (padrões obrigatórios) de forma determinística, sem busca. Chame no início pra saber o que é padrão antes de agir.",
				InputSchema: map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
			{
				Name:        "get_related",
				Description: "Explora o GRAFO de conhecimento: dada uma entidade/conceito (nome aproximado), retorna o que está conectado a ela em 1-2 hops com relação e distância.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"entity":    map[string]any{"type": "string", "description": "nome da entidade/conceito a explorar no grafo (match aproximado por similaridade)"},
						"max_depth": map[string]any{"type": "integer", "description": "nº de hops a partir da entidade (default 2, máx 5)"},
					},
					"required": []string{"entity"},
				},
			},
			{
				Name:        "graph_overview",
				Description: "Panorama do GRAFO de conhecimento da org: total de entidades/relações, distribuição por tipo, TEMAS CENTRAIS e CONECTORES.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"top_n": map[string]any{"type": "integer", "description": "quantos itens em central/connectors (default 10, máx 50)"},
					},
				},
			},
			{
				Name:        "update_memory",
				Description: "Edita uma memória existente IN-PLACE (substitui o conteúdo e re-embeda). Use pra CORRIGIR memória desatualizada em vez de adicionar outra contradizendo.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":      map[string]any{"type": "integer", "description": "id da memória a editar (obtido via search_memory)"},
						"content": map[string]any{"type": "string", "description": "novo conteúdo markdown completo (substitui o anterior)"},
						"title":   map[string]any{"type": "string", "description": "novo título (opcional; mantém o atual se vazio)"},
						"project": map[string]any{"type": "string", "description": "reatribui o projeto (slug: nexusyn, reachyn…); vazio = mantém o projeto atual"},
					},
					"required": []string{"id", "content"},
				},
			},
			{
				Name:        "delete_memory",
				Description: "Apaga uma memória da busca (soft-delete + remove os chunks do recall, mantendo histórico). Use pra remover memória errada/obsoleta.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id": map[string]any{"type": "integer", "description": "id da memória a apagar (obtido via search_memory)"},
					},
					"required": []string{"id"},
				},
			},
		},
	}

	payload, _ := json.MarshalIndent(card, "", "  ")

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}
}
