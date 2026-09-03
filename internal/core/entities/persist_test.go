package entities

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// parseValidTime aceita ISO date-only e RFC3339; vazio/inválido → nil (não chuta).
func TestParseValidTime(t *testing.T) {
	assert.Nil(t, parseValidTime(""), "vazio -> nil")
	assert.Nil(t, parseValidTime("   "), "whitespace -> nil")
	assert.Nil(t, parseValidTime("marco de 2020"), "nao-ISO -> nil (nunca chuta data)")

	d := parseValidTime("2020-03-15")
	if assert.NotNil(t, d, "date-only ISO parseia") {
		assert.Equal(t, 2020, d.Year())
		assert.Equal(t, 3, int(d.Month()))
		assert.Equal(t, 15, d.Day())
	}

	r := parseValidTime("2021-06-15T10:30:00Z")
	if assert.NotNil(t, r, "RFC3339 parseia") {
		assert.Equal(t, 2021, r.Year())
	}
}

// temporal grounding (Módulo D): a data de observação é injetada no prompt pra o
// LLM resolver datas relativas; sem data (zero) não injeta.
func TestBuildExtractUserPrompt_ObservationDate(t *testing.T) {
	obs := time.Date(2025, 6, 20, 0, 0, 0, 0, time.UTC)
	p := buildExtractUserPrompt("Luciano mudou ano passado", obs)
	assert.Contains(t, p, "Observation date: 2025-06-20", "injeta a data de observacao")
	assert.Contains(t, p, "Luciano mudou ano passado", "mantem o texto da passagem")
	assert.NotContains(t, buildExtractUserPrompt("x", time.Time{}), "Observation date:", "sem data -> nao injeta")
}
