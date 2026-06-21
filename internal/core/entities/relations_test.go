package entities

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// TestGenEdgeBackfillSQL regenera engine/scripts/backfill_edge_kinds.sql a partir
// do mapa canônico (fonte única). Rodar: NEXUS_GEN_BACKFILL=1 go test -run GenEdge
// ./internal/core/entities/. O SQL é N2 (muta todos os edges) — backup antes.
func TestGenEdgeBackfillSQL(t *testing.T) {
	if os.Getenv("NEXUS_GEN_BACKFILL") != "1" {
		t.Skip("set NEXUS_GEN_BACKFILL=1 para regenerar o backfill SQL")
	}
	// sinônimos ordenados (determinístico)
	pairs := make([][2]string, 0, len(edgeSynonyms))
	for raw, canon := range edgeSynonyms {
		pairs = append(pairs, [2]string{raw, canon})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i][1] != pairs[j][1] {
			return pairs[i][1] < pairs[j][1]
		}
		return pairs[i][0] < pairs[j][0]
	})
	canon := make([]string, 0, len(canonicalEdgeKinds))
	for k := range canonicalEdgeKinds {
		canon = append(canon, k)
	}
	sort.Strings(canon)

	var b strings.Builder
	b.WriteString("-- backfill_edge_kinds.sql — GERADO de relations.go (NÃO editar à mão).\n")
	b.WriteString("-- Regerar: NEXUS_GEN_BACKFILL=1 go test -run GenEdge ./internal/core/entities/\n")
	b.WriteString("-- Fase 0.5 GraphRAG: normaliza edges.kind legados no vocabulário canônico.\n")
	b.WriteString("-- ⚠️ N2 — muta TODOS os edges. BACKUP antes. Rodar em transação e conferir.\n\n")
	b.WriteString("BEGIN;\n\n-- 1) sinônimos de mesma direção → canônico\n")
	b.WriteString("UPDATE edges e SET kind = m.canon FROM (VALUES\n")
	for i, p := range pairs {
		sep := ","
		if i == len(pairs)-1 {
			sep = ""
		}
		b.WriteString(fmt.Sprintf("  ('%s','%s')%s\n", p[0], p[1], sep))
	}
	b.WriteString(") AS m(raw, canon) WHERE e.kind = m.raw;\n\n")
	b.WriteString("-- 2) qualquer kind restante fora do conjunto canônico → relates_to\n")
	quoted := make([]string, len(canon))
	for i, k := range canon {
		quoted[i] = "'" + k + "'"
	}
	b.WriteString("UPDATE edges SET kind = 'relates_to' WHERE kind NOT IN (\n  " +
		strings.Join(quoted, ", ") + "\n);\n\n")
	b.WriteString("-- Conferir ANTES de commitar:\n")
	b.WriteString("--   SELECT kind, count(*) FROM edges GROUP BY kind ORDER BY 2 DESC;\n")
	b.WriteString("COMMIT;\n")

	if err := os.WriteFile("../../../scripts/backfill_edge_kinds.sql", []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("gerado: engine/scripts/backfill_edge_kinds.sql (%d sinônimos, %d canônicos)", len(pairs), len(canon))
}

func TestNormalizeEdgeKind(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// canônico passa direto
		{"caused", "caused"},
		{"located_in", "located_in"},
		{"relates_to", "relates_to"},
		// case-insensitive + trim
		{"  Caused ", "caused"},
		{"USES", "uses"},
		// sinônimos de mesma direção colapsam
		{"used_in", "uses"},
		{"uses_provider", "uses"},
		{"includes", "contains"},
		{"requires", "depends_on"},
		{"related_to", "relates_to"},
		{"creates", "produces"},
		{"redirects_to", "routes_to"},
		// inversos NÃO colapsam pro ativo — caem em relates_to (direção sagrada)
		{"caused_by", "relates_to"},
		{"used_by", "relates_to"},
		{"owns", "relates_to"},
		{"hosts", "relates_to"},
		{"serves", "relates_to"},
		// desconhecido / cauda longa → relates_to
		{"frobnicates", "relates_to"},
		{"", "relates_to"},
	}
	for _, c := range cases {
		if got := NormalizeEdgeKind(c.in); got != c.want {
			t.Errorf("NormalizeEdgeKind(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeEdgeKindSynonymsTargetCanonical(t *testing.T) {
	// todo destino de sinônimo precisa ser um kind canônico (senão vira relates_to silencioso)
	for raw, canon := range edgeSynonyms {
		if !canonicalEdgeKinds[canon] {
			t.Errorf("edgeSynonyms[%q] = %q não é canônico", raw, canon)
		}
		if canonicalEdgeKinds[raw] {
			t.Errorf("edgeSynonyms tem chave %q que JÁ é canônica (redundante)", raw)
		}
	}
}

func TestNormalizeConfidence(t *testing.T) {
	cases := []struct {
		inState string
		inScore float64
		wState  string
		wScore  float64
	}{
		{"extracted", 1.0, "extracted", 1.0},
		{"inferred", 0, "inferred", 0.7},   // score ausente → default por estado
		{"ambiguous", 0, "ambiguous", 0.4}, // idem
		{"", 0, "extracted", 1.0},          // estado ausente → extracted
		{"garbage", 0, "extracted", 1.0},   // estado inválido → extracted
		{"AMBIGUOUS", 0.3, "ambiguous", 0.3},
		{"extracted", 2.5, "extracted", 1.0}, // clamp acima de 1
	}
	for _, c := range cases {
		gs, gsc := normalizeConfidence(c.inState, c.inScore)
		if gs != c.wState || gsc != c.wScore {
			t.Errorf("normalizeConfidence(%q,%v) = (%q,%v), want (%q,%v)",
				c.inState, c.inScore, gs, gsc, c.wState, c.wScore)
		}
	}
}
