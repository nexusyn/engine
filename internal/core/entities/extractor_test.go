package entities

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexusyn/engine/internal/provider/llm"
)

// stubLLM permite ditar a resposta sem subir HTTP.
type stubLLM struct {
	res llm.Result
	err error
}

func (s *stubLLM) Complete(_ context.Context, _ llm.Prompt) (llm.Result, error) {
	if s.err != nil {
		return llm.Result{}, s.err
	}
	return s.res, nil
}
func (*stubLLM) Name() string  { return "stub" }
func (*stubLLM) Model() string { return "stub-model" }

func TestExtract_HappyPath(t *testing.T) {
	stub := &stubLLM{res: llm.Result{Content: `{
		"entities": [
			{"name": "Luna", "kind": "person", "aliases": ["a gata Luna"], "attributes": {"age": 4}},
			{"name": "2020-03-14", "kind": "date", "attributes": {"iso": "2020-03-14"}}
		],
		"edges": [
			{"from_name": "Luna", "to_name": "2020-03-14", "kind": "happened_at", "weight": 1.0}
		]
	}`}}

	got, err := Extract(context.Background(), stub, "Luna nasceu em 2020-03-14.")
	require.NoError(t, err)
	require.Len(t, got.Entities, 2)
	assert.Equal(t, "Luna", got.Entities[0].Name)
	assert.Equal(t, "person", got.Entities[0].Kind)
	assert.Equal(t, []string{"a gata Luna"}, got.Entities[0].Aliases)
	assert.Equal(t, "luna", got.Entities[0].Slug())

	require.Len(t, got.Edges, 1)
	assert.Equal(t, "Luna", got.Edges[0].FromName)
	assert.Equal(t, "happened_at", got.Edges[0].Kind)
	assert.Equal(t, 1.0, got.Edges[0].Weight)
}

func TestExtract_StripsCodeFence(t *testing.T) {
	// LLMs costumam ignorar "no code fence" — extractor precisa ser tolerante
	stub := &stubLLM{res: llm.Result{Content: "```json\n{\"entities\":[{\"name\":\"X\",\"kind\":\"concept\"}],\"edges\":[]}\n```"}}
	got, err := Extract(context.Background(), stub, "X é um conceito")
	require.NoError(t, err)
	require.Len(t, got.Entities, 1)
	assert.Equal(t, "X", got.Entities[0].Name)
}

func TestExtract_EmptyText(t *testing.T) {
	stub := &stubLLM{err: errors.New("LLM nunca deveria ser chamado")}
	got, err := Extract(context.Background(), stub, "   ")
	require.NoError(t, err)
	assert.Empty(t, got.Entities)
	assert.Empty(t, got.Edges)
}

func TestExtract_BadJSON_Errors(t *testing.T) {
	stub := &stubLLM{res: llm.Result{Content: "isso não é json"}}
	_, err := Extract(context.Background(), stub, "qualquer texto")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad json")
}

func TestExtract_DropsInvalidKind(t *testing.T) {
	stub := &stubLLM{res: llm.Result{Content: `{
		"entities": [
			{"name": "X", "kind": "PERSON"},
			{"name": "Y", "kind": "monster"},
			{"name": "Z", "kind": ""}
		],
		"edges": []
	}`}}
	got, err := Extract(context.Background(), stub, "Y X Z")
	require.NoError(t, err)
	require.Len(t, got.Entities, 3)
	// PERSON → lowercase = person (válido)
	assert.Equal(t, "person", got.Entities[0].Kind)
	// monster → fallback concept
	assert.Equal(t, "concept", got.Entities[1].Kind)
	// "" → fallback concept
	assert.Equal(t, "concept", got.Entities[2].Kind)
}

func TestExtract_DedupBySlug(t *testing.T) {
	// "Luna" e "luna" geram mesmo slug — só 1 entity
	stub := &stubLLM{res: llm.Result{Content: `{
		"entities": [
			{"name": "Luna", "kind": "person"},
			{"name": "luna", "kind": "person"},
			{"name": "Luna  ", "kind": "person"}
		],
		"edges": []
	}`}}
	got, err := Extract(context.Background(), stub, "Luna")
	require.NoError(t, err)
	assert.Len(t, got.Entities, 1)
}

func TestExtract_DropsOrphanEdges(t *testing.T) {
	stub := &stubLLM{res: llm.Result{Content: `{
		"entities": [
			{"name": "Luna", "kind": "person"}
		],
		"edges": [
			{"from_name": "Luna", "to_name": "Marte", "kind": "located_in"},
			{"from_name": "Luna", "to_name": "Luna", "kind": "self"}
		]
	}`}}
	got, err := Extract(context.Background(), stub, "Luna")
	require.NoError(t, err)
	// Marte não foi extraído → drop. Self-loop → drop.
	assert.Empty(t, got.Edges)
}

func TestExtract_DefaultEdgeKind(t *testing.T) {
	stub := &stubLLM{res: llm.Result{Content: `{
		"entities": [
			{"name": "A", "kind": "concept"},
			{"name": "B", "kind": "concept"}
		],
		"edges": [
			{"from_name": "A", "to_name": "B"}
		]
	}`}}
	got, err := Extract(context.Background(), stub, "A e B")
	require.NoError(t, err)
	require.Len(t, got.Edges, 1)
	assert.Equal(t, "relates_to", got.Edges[0].Kind)
	assert.Equal(t, 1.0, got.Edges[0].Weight)
}

func TestExtract_NilProvider(t *testing.T) {
	_, err := Extract(context.Background(), nil, "texto")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "provider nil")
}

func TestExtract_LLMError_Propagates(t *testing.T) {
	stub := &stubLLM{err: errors.New("quota exceeded")}
	_, err := Extract(context.Background(), stub, "texto")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "quota exceeded")
}

func TestEntityRef_Slug(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"Luna", "luna"},
		{"Luciano Rodrigues", "luciano-rodrigues"},
		{"Açúcar Refinado!", "acucar-refinado"},
		{"São Paulo", "sao-paulo"},
		{"  ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := EntityRef{Name: tc.name}
			assert.Equal(t, tc.want, e.Slug())
		})
	}
}
