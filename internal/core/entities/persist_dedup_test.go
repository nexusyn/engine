package entities

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDedupOneToOneBatch_KeepsLastOnly(t *testing.T) {
	edges := []EdgeRef{
		{FromName: "Luna", ToName: "Lavras-MG", Kind: "located_in"},     // duplicate (early)
		{FromName: "Luna", ToName: "Florianópolis", Kind: "located_in"}, // duplicate (late — sobrevive)
		{FromName: "Luna", ToName: "Marte", Kind: "mentions"},           // M-to-M — sobrevive
		{FromName: "Luna", ToName: "Vênus", Kind: "mentions"},           // M-to-M — sobrevive
	}

	out := dedupOneToOneBatch(edges)
	assert.Len(t, out, 3, "removeu lavras (1-to-1 substituída por florianopolis)")

	// Garante que a ÚLTIMA located_in sobreviveu
	var locatedToNames []string
	var mentionsCount int
	for _, e := range out {
		if e.Kind == "located_in" {
			locatedToNames = append(locatedToNames, e.ToName)
		}
		if e.Kind == "mentions" {
			mentionsCount++
		}
	}
	assert.Equal(t, []string{"Florianópolis"}, locatedToNames)
	assert.Equal(t, 2, mentionsCount, "ambas mentions preservadas (M-to-M)")
}

func TestDedupOneToOneBatch_DifferentFroms_NaoInterfere(t *testing.T) {
	// 2 located_in com FROMS diferentes não conflitam — ambas sobrevivem
	edges := []EdgeRef{
		{FromName: "Luna", ToName: "Lavras-MG", Kind: "located_in"},
		{FromName: "Luciano", ToName: "Belo Horizonte", Kind: "located_in"},
	}
	out := dedupOneToOneBatch(edges)
	assert.Len(t, out, 2)
}

func TestDedupOneToOneBatch_DifferentKinds_NaoInterfere(t *testing.T) {
	// Mesmo FROM mas kinds diferentes (located_in vs works_for) — ambas sobrevivem
	edges := []EdgeRef{
		{FromName: "Luna", ToName: "Lavras-MG", Kind: "located_in"},
		{FromName: "Luna", ToName: "TechCo", Kind: "works_for"},
	}
	out := dedupOneToOneBatch(edges)
	assert.Len(t, out, 2)
}

func TestDedupOneToOneBatch_OnlyMtoM_NoFilter(t *testing.T) {
	// Sem 1-to-1 no batch — retorna tudo intacto
	edges := []EdgeRef{
		{FromName: "A", ToName: "B", Kind: "mentions"},
		{FromName: "A", ToName: "B", Kind: "mentions"},
		{FromName: "A", ToName: "C", Kind: "relates_to"},
	}
	out := dedupOneToOneBatch(edges)
	assert.Len(t, out, 3, "M-to-M nunca é filtrado")
}

func TestDedupOneToOneBatch_TripleContradiction(t *testing.T) {
	// 3 located_in seguidas: só a última vence
	edges := []EdgeRef{
		{FromName: "Luna", ToName: "A", Kind: "located_in"},
		{FromName: "Luna", ToName: "B", Kind: "located_in"},
		{FromName: "Luna", ToName: "C", Kind: "located_in"}, // sobrevive
	}
	out := dedupOneToOneBatch(edges)
	assert.Len(t, out, 1)
	assert.Equal(t, "C", out[0].ToName)
}

func TestDedupOneToOneBatch_Empty(t *testing.T) {
	assert.Empty(t, dedupOneToOneBatch(nil))
	assert.Empty(t, dedupOneToOneBatch([]EdgeRef{}))
}
