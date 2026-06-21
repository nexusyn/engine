package entities

// Cardinality classifica edge kinds em "1-to-1" (atual única por par
// from→kind) ou "M-to-M" (múltiplas coexistem).
//
// Day 20: usado pelo Persist pra decidir supersede automático. Se uma
// nova edge com kind 1-to-1 chegar com to_id diferente da existente,
// a antiga ganha valid_to = now() (bi-temporal supersede do Graphiti).
//
// M-to-M edges (mentions, relates_to, caused, happened_at) coexistem —
// nenhuma supersede acontece. Múltiplos "mentions" entre as mesmas duas
// entities são OK (cada ingest pode reforçar a conexão).
//
// Critério pra incluir kind como 1-to-1:
//   - A relação é tipicamente exclusiva no tempo (mora em 1 lugar)
//   - Ou é uma propriedade "current_*" que deveria ser atualizada
//   - NÃO incluir "born_in" porque é imutável (não vale supersede)
//   - NÃO incluir "happened_at" porque eventos podem ter múltiplas datas válidas

// oneToOneKinds é a tabela canônica. Lower-case obrigatório (sanitize
// já força lower em edges).
var oneToOneKinds = map[string]bool{
	"located_in":       true, // pessoa mora em 1 lugar; place está em 1 região
	"lives_in":         true, // sinônimo de located_in
	"works_for":        true, // 1 empregador atual por vez
	"current_role":     true, // 1 cargo atual
	"current_employer": true, // explícito
	"current_address":  true, // 1 endereço atual
	"married_to":       true, // 1 cônjuge por vez (na prática)
	"reports_to":       true, // 1 gestor direto
}

// IsOneToOne retorna true se o kind deve causar supersede da edge
// existente (mesmo from_entity_id + kind) quando uma nova chega com
// to_entity_id diferente.
//
// Default false — qualquer kind não listado é tratado como M-to-M
// (coexiste com outras). Isso é seguro: M-to-M nunca causa data loss.
func IsOneToOne(kind string) bool {
	return oneToOneKinds[kind]
}
