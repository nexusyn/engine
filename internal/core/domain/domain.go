// Package domain centraliza a whitelist de domínios de ENTRADA do RAG.
//
// Segurança: um agente só pode ESCREVER memory/knowledge/guideline/skill. Os domínios
// DERIVADOS (wiki/lesson/decision/error) são produzidos exclusivamente pelo compile —
// escrevê-los direto forjaria "autoridade" e burlaria a destilação (envenenamento /
// manipulação de ranking). Fonte única usada por todos os caminhos de ingestão
// (MCP add_memory + ingest HTTP/worker).
//
// skill: catálogo de skills do agente (formato SKILL.md). Autoritativo e ISENTO de
// destilação — igual guideline, NÃO entra na fonte do compile (ver migration
// 0023_compile_state.sql, que destila só memory+knowledge).
package domain

import "strings"

// InputDomains — domínios que um agente pode escrever.
var InputDomains = map[string]bool{"memory": true, "knowledge": true, "guideline": true, "skill": true}

// NormalizeInput devolve um domínio de entrada válido: faz trim+lowercase e, se vazio
// ou fora da whitelist (inclusive os derivados), devolve "memory". Idempotente.
func NormalizeInput(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	if !InputDomains[d] {
		return "memory"
	}
	return d
}
