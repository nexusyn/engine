package query

import (
	"regexp"
	"strings"
)

// scratchpadInstruction é anexado ao system prompt SÓ para queries que precisam
// ENUMERAR (count/aggregation/ordering/knowledge-update). O M2.7 (gerador de prod)
// não consome thinking budget — sem um rascunho explícito ele subconta e pega
// valor stale (os #1 erros do bench). Damos o rascunho num bloco <scratch> e o
// DESCARTAMOS server-side, devolvendo só <final>: o modelo raciocina sem vazar o
// raciocínio (que foi o motivo de impor "ANSWER ONLY"). Custo-zero — mesmo modelo.
const scratchpadInstruction = `
REASONING MODE — this question requires enumeration (counting, aggregation, ordering, or a value that changed over time). Use this TWO-PART format, which OVERRIDES the "ANSWER ONLY" rule above:
1. <scratch>
   List EVERY relevant mention of the subject, each with its [Session date]. Then do the operation explicitly: sort by date, count the distinct instances, or pick the value carrying the LATEST [Session date]. Reason freely here.
   </scratch>
2. <final>
   ONLY the answer, obeying every OUTPUT FORMAT rule above (terse — just the number/name/value; no preamble; no [Session date] tags).
   </final>
The <scratch> block is DISCARDED by the server; only <final> reaches the user. NEVER leave <final> empty, and NEVER put the answer only inside <scratch>.`

var (
	finalTagRe   = regexp.MustCompile(`(?is)<final>(.*?)</final>`)
	scratchTagRe = regexp.MustCompile(`(?is)<scratch>.*?</scratch>`)
	strayTagsRe  = regexp.MustCompile(`(?i)</?(final|scratch)>`)
)

// extractFinal recupera o conteúdo de <final>, com fallback robusto pra NUNCA
// devolver vazio nem vazar o <scratch>:
//  1. há <final>…</final> não-vazio → esse conteúdo;
//  2. senão, remove <scratch>…</scratch> e usa o resto (modelo esqueceu o <final>);
//  3. senão, devolve a string original limpa de tags órfãs.
func extractFinal(s string) string {
	if m := finalTagRe.FindStringSubmatch(s); m != nil {
		if out := strings.TrimSpace(m[1]); out != "" {
			return out
		}
	}
	stripped := strings.TrimSpace(scratchTagRe.ReplaceAllString(s, ""))
	stripped = strings.TrimSpace(strayTagsRe.ReplaceAllString(stripped, ""))
	if stripped != "" {
		return stripped
	}
	if cleaned := strings.TrimSpace(strayTagsRe.ReplaceAllString(s, "")); cleaned != "" {
		return cleaned
	}
	return strings.TrimSpace(s) // último recurso — nunca devolver vazio
}
