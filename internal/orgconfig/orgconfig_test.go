package orgconfig

import "testing"

func TestValidStage(t *testing.T) {
	valid := []string{"generation", "extraction", "embed", "rerank"}
	for _, s := range valid {
		if !ValidStage(s) {
			t.Errorf("ValidStage(%q) = false, quer true", s)
		}
	}
	invalid := []string{"", "gen", "query", "GENERATION", "llm"}
	for _, s := range invalid {
		if ValidStage(s) {
			t.Errorf("ValidStage(%q) = true, quer false", s)
		}
	}
}

func TestModelChoice_Validate(t *testing.T) {
	cases := []struct {
		name string
		c    ModelChoice
		ok   bool
	}{
		{"ok generation", ModelChoice{Stage: "generation", Provider: "gemini", Model: "gemini-3.5-flash"}, true},
		{"ok ollama base_url", ModelChoice{Stage: "extraction", Provider: "ollama-turbo", Model: "gpt-oss:120b", BaseURL: "https://ollama.com"}, true},
		{"stage inválido", ModelChoice{Stage: "foo", Provider: "gemini"}, false},
		{"stage vazio", ModelChoice{Stage: "", Provider: "gemini"}, false},
		{"provider vazio", ModelChoice{Stage: "generation", Provider: ""}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.Validate()
			if tc.ok && err != nil {
				t.Errorf("Validate() = %v, quer nil", err)
			}
			if !tc.ok && err == nil {
				t.Errorf("Validate() = nil, quer erro")
			}
		})
	}
}
