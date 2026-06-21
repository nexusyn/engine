package query

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexusyn/engine/internal/provider/llm"
)

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

func TestSubQueryGen_DecomposesMultiHop(t *testing.T) {
	stub := &stubLLM{res: llm.Result{Content: `{"sub_queries": ["my age", "grandma age"]}`}}
	gen := NewSubQueryGen(stub)
	out, err := gen.Generate(context.Background(), "How many years older is my grandma than me?")
	require.NoError(t, err)
	assert.Equal(t, []string{"my age", "grandma age"}, out)
}

func TestSubQueryGen_EmptyForAtomic(t *testing.T) {
	stub := &stubLLM{res: llm.Result{Content: `{"sub_queries": []}`}}
	gen := NewSubQueryGen(stub)
	out, err := gen.Generate(context.Background(), "what's the weather?")
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestSubQueryGen_StripsCodeFence(t *testing.T) {
	stub := &stubLLM{res: llm.Result{
		Content: "```json\n{\"sub_queries\": [\"x\", \"y\"]}\n```",
	}}
	gen := NewSubQueryGen(stub)
	out, err := gen.Generate(context.Background(), "two-part question")
	require.NoError(t, err)
	assert.Equal(t, []string{"x", "y"}, out)
}

func TestSubQueryGen_DropsEmptyEntries(t *testing.T) {
	stub := &stubLLM{res: llm.Result{Content: `{"sub_queries": ["a", "", "  ", "b"]}`}}
	gen := NewSubQueryGen(stub)
	out, err := gen.Generate(context.Background(), "anything")
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, out)
}

func TestSubQueryGen_BadJSON_Errors(t *testing.T) {
	stub := &stubLLM{res: llm.Result{Content: "not json"}}
	gen := NewSubQueryGen(stub)
	_, err := gen.Generate(context.Background(), "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad json")
}

func TestSubQueryGen_LLMError_Propagates(t *testing.T) {
	stub := &stubLLM{err: errors.New("quota")}
	gen := NewSubQueryGen(stub)
	_, err := gen.Generate(context.Background(), "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "quota")
}

func TestSubQueryGen_NilProvider(t *testing.T) {
	gen := NewSubQueryGen(nil)
	_, err := gen.Generate(context.Background(), "x")
	require.Error(t, err)
}
