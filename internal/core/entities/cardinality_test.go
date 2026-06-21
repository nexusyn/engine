package entities

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsOneToOne(t *testing.T) {
	cases := []struct {
		kind string
		want bool
	}{
		// 1-to-1 (causam supersede)
		{"located_in", true},
		{"lives_in", true},
		{"works_for", true},
		{"current_role", true},
		{"current_employer", true},
		{"current_address", true},
		{"married_to", true},
		{"reports_to", true},

		// M-to-M (coexistem)
		{"mentions", false},
		{"relates_to", false},
		{"happened_at", false},
		{"caused", false},
		{"born_in", false}, // imutável, não vale supersede

		// Edge cases
		{"", false},
		{"UNKNOWN_KIND", false},
		{"LOCATED_IN", false}, // lowercase obrigatório
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			assert.Equal(t, tc.want, IsOneToOne(tc.kind))
		})
	}
}
