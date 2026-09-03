package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/nexusyn/engine/internal/provider/llm"
)

// genResolver expõe o provider de geração ao vivo (config global). Implementado
// pelo modelresolver — o mesmo injetado no serviço de query.
type genResolver interface {
	Generation(ctx context.Context) llm.Provider
}

// AdminTranslateHandler — POST /v1/admin/translate {"text":"...","target":"en"|"pt"}:
// traduz texto via o LLM de geração já configurado. Usado pelo console (Comunicados)
// pra o operador digitar só em português e a IA preencher a versão em inglês.
// O provider é interno (não vai na resposta) — white-label. Admin-only (RequireAbility).
func AdminTranslateHandler(resolver genResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Text   string `json:"text"`
			Target string `json:"target"`
		}
		if !decodeJSONBody(w, r, &req) {
			return
		}

		req.Text = strings.TrimSpace(req.Text)
		if req.Text == "" {
			writeError(w, http.StatusBadRequest, "text vazio")
			return
		}

		// Default PT→EN; aceita target=pt pra o caminho inverso.
		target := "English (US)"
		if strings.HasPrefix(strings.ToLower(req.Target), "pt") {
			target = "Brazilian Portuguese"
		}

		gen := resolver.Generation(r.Context())
		if gen == nil {
			writeInternalError(w, "translate", fmt.Errorf("nenhum provider de geração disponível"))
			return
		}

		res, err := gen.Complete(r.Context(), llm.Prompt{
			System: "You are a professional translator. Translate the user's message into " + target +
				". Preserve the meaning, tone, line breaks and any {placeholders}. " +
				"Do not add notes, explanations, headers or quotes — output ONLY the translated text.",
			User:        req.Text,
			Temperature: 0.2,
			MaxTokens:   2000,
		})
		if err != nil {
			writeInternalError(w, "translate", err)
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"translation": strings.TrimSpace(res.Content),
		})
	}
}
