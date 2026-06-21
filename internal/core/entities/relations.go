package entities

import "strings"

// Fase 0.5 (GraphRAG, 2026-06-18) — normalização do vocabulário de relações.
//
// PROBLEMA: o LLM extrator gerava edge kinds livremente → 700 kinds distintos
// em prod (62% cauda longa, ≤2 ocorrências). Traversal "por tipo de relação"
// é inútil com 700 sinônimos espalhados (caused/causes/resulted_in/motivated…).
//
// SOLUÇÃO simétrica ao que já trava entities (sanitizeEntities + allowedKinds):
//   - canonicalEdgeKinds = conjunto FECHADO de relações.
//   - edgeSynonyms = mapa raw→canônico (gerado dos 700 kinds reais).
//   - NormalizeEdgeKind colapsa qualquer kind: canônico passa direto; sinônimo
//     vira o canônico; o resto cai em "relates_to" (fallback seguro, sem perda
//     de direção da aresta).
//
// REGRA DE DIREÇÃO: a aresta é from -[kind]-> to. Sinônimos só colapsam quando
// preservam o sentido. Inversos (caused_by, used_by, owns vs owned_by…) NÃO
// viram o canônico direto — caem em relates_to pra nunca inverter semântica.

// canonicalEdgeKinds é o conjunto fechado de relações do grafo. Os 6 últimos
// (1-to-1) coincidem com oneToOneKinds (cardinality.go) e DEVEM permanecer
// válidos pra não quebrar o supersede bi-temporal.
var canonicalEdgeKinds = map[string]bool{
	// genéricos
	"mentions": true, "relates_to": true,
	// causal / efeito
	"caused": true, "prevents": true, "mitigates": true, "fixes": true,
	"enables": true, "triggers": true,
	// estrutura
	"contains": true, "part_of": true, "depends_on": true, "has_attribute": true,
	// dados / fluxo
	"uses": true, "produces": true, "stores": true, "exposes": true,
	"connects_to": true, "routes_to": true,
	// comportamento
	"implements": true, "defines": true, "configures": true, "validates": true,
	"calls": true,
	// conhecimento
	"references": true, "documents": true, "decided": true, "contradicts": true,
	"alternative_to": true, "replaces": true,
	// pessoa / lugar / tempo
	"located_in": true, "happened_at": true, "works_for": true, "owned_by": true,
	// preferência / transição
	"prefers": true, "avoids": true, "changed_from": true,
	// 1-to-1 herdados (cardinality.go) — manter válidos
	"lives_in": true, "current_role": true, "current_employer": true,
	"current_address": true, "married_to": true, "reports_to": true,
}

// edgeSynonyms mapeia kinds não-canônicos para o canônico (MESMA direção).
// Gerado dos 700 kinds reais de prod (2026-06-18); 157 entradas cobrem ~35% das
// edges em canônicos ricos. Kinds ausentes daqui e não-canônicos → "relates_to".
// DIREÇÃO PRESERVADA: todo kind inverso (_by, owns, hosts, serves…) fica FORA
// deste mapa de propósito (cai em relates_to) pra nunca inverter a aresta.
var edgeSynonyms = map[string]string{
	"related_to": "relates_to",
	// uses
	"used_in": "uses", "uses_provider": "uses", "uses_service": "uses",
	"used_for": "uses", "can_use": "uses", "uses_technology": "uses",
	"used": "uses", "uses_flag": "uses", "uses_mode": "uses", "uses_config": "uses",
	"uses_parameter": "uses", "will_use": "uses",
	// contains
	"includes": "contains", "has_component": "contains", "has_part": "contains",
	"has_phase": "contains", "has_route": "contains", "has_property": "contains",
	"has_method": "contains", "has_column": "contains", "has_service": "contains",
	"has_instance": "contains", "has_command": "contains", "has_mode": "contains",
	"has_feature": "contains", "has_link": "contains",
	// located_in
	"implemented_in": "located_in", "stored_in": "located_in", "stores_in": "located_in",
	"documented_in": "located_in", "tested_in": "located_in", "deployed_on": "located_in",
	"runs_on": "located_in", "tested_on": "located_in", "validated_on": "located_in",
	"applied_in": "located_in", "deployed_to": "located_in", "configured_in": "located_in",
	"locates_in": "located_in", "implemented_on": "located_in",
	// part_of
	"belongs_to": "part_of", "is_instance_of": "part_of", "instance_of": "part_of",
	"is_type_of": "part_of", "is_a": "part_of", "version_of": "part_of",
	"endpoint_of": "part_of", "member_of": "part_of",
	// produces
	"creates": "produces", "created": "produces", "produced": "produces",
	"generates": "produces", "generated": "produces", "outputs": "produces",
	// calls
	"executes": "calls", "performs": "calls", "runs": "calls", "dispatches": "calls",
	"executes_after": "calls", "performed": "calls",
	// depends_on
	"requires": "depends_on", "based_on": "depends_on", "gated_by": "depends_on",
	"needs": "depends_on", "constrained_by": "depends_on", "requires_env": "depends_on",
	"alternatively_requires": "depends_on",
	// routes_to
	"routes": "routes_to", "redirects_to": "routes_to", "feeds_into": "routes_to",
	"uploads_to": "routes_to", "writes_to": "routes_to", "sends": "routes_to",
	"dispatches_to": "routes_to", "sends_to": "routes_to", "syncs_to": "routes_to",
	"pushes_to": "routes_to", "persists_to": "routes_to", "outputs_to": "routes_to",
	"backed_up_to": "routes_to", "added_to": "routes_to", "copied_to": "routes_to",
	"routed_to": "routes_to",
	// validates
	"checks": "validates", "verifies": "validates", "verifies_quota": "validates",
	"evaluates": "validates", "monitors": "validates", "detects": "validates",
	"confirms": "validates", "tested": "validates", "tests": "validates",
	// fixes
	"resolves": "fixes", "solves": "fixes", "addresses": "fixes", "resolved": "fixes",
	"fixed": "fixes", "fixed_in": "fixes",
	// references
	"points_to": "references", "links_to": "references", "refers_to": "references",
	// connects_to
	"integrates_with": "connects_to", "connects": "connects_to", "connected": "connects_to",
	"connects_via": "connects_to", "binds_to": "connects_to",
	// caused
	"causes": "caused", "resulted_in": "caused", "produced_result": "caused",
	// has_attribute
	"has_quota": "has_attribute", "has_capability": "has_attribute", "has_flag": "has_attribute",
	"has_permission": "has_attribute", "has_credential": "has_attribute", "has_capital": "has_attribute",
	// prevents
	"protects": "prevents", "guards": "prevents", "blocks": "prevents", "excludes": "prevents",
	// enables
	"allows": "enables", "authorizes": "enables", "unlocks": "enables", "grants": "enables",
	// documents
	"describes": "documents", "states": "documents", "stated": "documents",
	// defines
	"specifies": "defines", "defines_service": "defines", "defines_limits": "defines",
	// works_for
	"specializes_in": "works_for",
	// implements
	"implemented_as": "implements", "implemented": "implements",
	// contradicts
	"differs_from": "contradicts", "contrasts_with": "contradicts",
	"violates": "contradicts", "incompatible_with": "contradicts",
	// alternative_to
	"falls_back_to": "alternative_to", "fallback_to": "alternative_to", "variant_of": "alternative_to",
	// replaces
	"merged_into": "replaces",
	// configures
	"configured_with": "configures",
	// stores
	"persists": "stores", "saved_as": "stores", "writes": "stores",
	// exposes
	"exposes_endpoint": "exposes", "exposed_as": "exposes",
	// happened_at
	"happens_at": "happened_at", "scheduled_for": "happened_at",
	// triggers
	"triggers_at": "triggers",
	// decided
	"decided_at": "decided",
}

// NormalizeEdgeKind colapsa um kind cru no vocabulário canônico fechado.
// Vazio → "relates_to". Canônico → ele mesmo. Sinônimo → canônico.
// Desconhecido → "relates_to" (fallback que nunca inverte a aresta).
func NormalizeEdgeKind(kind string) string {
	k := strings.ToLower(strings.TrimSpace(kind))
	if k == "" {
		return "relates_to"
	}
	if canonicalEdgeKinds[k] {
		return k
	}
	if c, ok := edgeSynonyms[k]; ok {
		return c
	}
	return "relates_to"
}

// confidenceDefaultScore é o score padrão por estado de provenance, usado quando
// o LLM não emite confidence_score explícito. extracted > inferred > ambiguous.
var confidenceDefaultScore = map[string]float64{
	"extracted": 1.0,
	"inferred":  0.7,
	"ambiguous": 0.4,
}

// normalizeConfidence valida o estado de provenance e coage o score pra (0,1].
// Estado ausente/inválido → "extracted" (conservador: assume extração direta,
// retrocompat com edges pré-Fase-2). Score ausente → default por estado; score
// fora de (0,1] → clamp.
func normalizeConfidence(state string, score float64) (string, float64) {
	s := strings.ToLower(strings.TrimSpace(state))
	if _, ok := confidenceDefaultScore[s]; !ok {
		s = "extracted"
	}
	if score <= 0 {
		score = confidenceDefaultScore[s]
	}
	if score > 1 {
		score = 1
	}
	return s, score
}
