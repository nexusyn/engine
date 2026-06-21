package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nexusyn/engine/internal/config"
	"github.com/nexusyn/engine/internal/platformconfig"
	"github.com/nexusyn/engine/internal/provider/embed"
	"github.com/nexusyn/engine/internal/provider/llm"
	"github.com/nexusyn/engine/internal/provider/rerank"
	"github.com/nexusyn/engine/internal/secret"
)

// modelCfgBaseURLAllow — sufixos de host confiáveis pro base_url do model-config.
// Provedores conhecidos por default; o operador pode somar via env (CSV).
var modelCfgBaseURLAllow = func() []string {
	base := []string{
		"minimax.io", "googleapis.com", "anthropic.com", "openrouter.ai",
		"ollama.com", "jina.ai", "together.ai", "together.xyz",
	}
	if extra := os.Getenv("NEXUS_MODELCFG_BASEURL_ALLOWLIST"); extra != "" {
		for _, h := range strings.Split(extra, ",") {
			if h = strings.TrimSpace(strings.ToLower(h)); h != "" {
				base = append(base, h)
			}
		}
	}
	return base
}()

// validateBaseURL bloqueia base_url apontando p/ host PÚBLICO arbitrário — defesa contra
// MITM/exfil da geração via model-config (um admin/MASTER comprometido redirecionaria TODO
// o tráfego LLM pro servidor do atacante). PERMITE: vazio (default do provider); host
// interno/self-hosted (IP privado/loopback, nome de serviço sem ponto, .internal/.local/.svc);
// e os sufixos de provedores conhecidos (+ NEXUS_MODELCFG_BASEURL_ALLOWLIST). Resto = bloqueado.
func validateBaseURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("base_url inválida")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("base_url: scheme deve ser http ou https")
	}
	host := strings.ToLower(u.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
			return nil // self-hosted interno
		}
		return fmt.Errorf("base_url: IP público não permitido (%s)", host)
	}
	if !strings.Contains(host, ".") || strings.HasSuffix(host, ".internal") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".svc") {
		return nil // nome de serviço docker / domínio interno
	}
	for _, allow := range modelCfgBaseURLAllow {
		if host == allow || strings.HasSuffix(host, "."+allow) {
			return nil
		}
	}
	return fmt.Errorf("base_url: host não permitido (%s) — se for confiável, adicione em NEXUS_MODELCFG_BASEURL_ALLOWLIST", host)
}

// Handlers da config de modelo GLOBAL do operador (admin-only). Endpoints:
//   GET  /v1/admin/model-config         → etapas com a config EFETIVA (platform OU .env)
//   PUT  /v1/admin/model-config         → grava uma etapa (cifra a key)
//   POST /v1/admin/model-config/test    → testa a conexão (NÃO salva); todas as etapas
//
// Gated por RequireAbility("admin"). NÃO mexe em org_model_config.

// llmModelKey: modelo default + se há key no .env pro provider LLM.
func llmModelKey(c config.LLMConfig, provider string) (string, bool) {
	switch provider {
	case "gemini":
		return c.Gemini.Model, c.Gemini.APIKey != ""
	case "anthropic":
		return c.Anthropic.Model, c.Anthropic.APIKey != ""
	case "minimax":
		return c.MiniMax.Model, c.MiniMax.APIKey != ""
	case "ollama-turbo", "ollama", "together":
		return c.OllamaTurbo.Model, c.OllamaTurbo.APIKey != ""
	case "openrouter":
		return c.OpenRouter.Model, c.OpenRouter.APIKey != ""
	}
	return "", false
}

func embedModelKey(c config.EmbedConfig, provider string) (string, bool) {
	switch provider {
	case "jina", "":
		return c.Jina.EmbedModel, c.Jina.APIKey != ""
	}
	return "", false
}

func rerankModelKey(c config.RerankConfig, e config.EmbedConfig, driver string) (string, bool) {
	switch driver {
	case "jina", "":
		return c.JinaModel, e.Jina.APIKey != ""
	}
	return "", false
}

// envDefaultView deriva a config EFETIVA de uma etapa a partir do .env (quando
// não há linha em platform_model_config). key_mask="(.env)" sinaliza origem.
func envDefaultView(stage string, cfg config.Config) platformconfig.View {
	v := platformconfig.View{Stage: stage}
	switch stage {
	case "generation":
		v.Provider = cfg.LLM.Primary
		v.Model, v.HasKey = llmModelKey(cfg.LLM, v.Provider)
	case "extraction":
		v.Provider = cfg.LLM.ExtractProvider
		if v.Provider == "" {
			v.Provider = cfg.LLM.Primary
		}
		m, hasKey := llmModelKey(cfg.LLM, v.Provider)
		v.Model = cfg.LLM.ExtractModel
		if v.Model == "" {
			v.Model = m
		}
		v.HasKey = hasKey
	case "embed":
		v.Provider = cfg.Embed.Provider
		if v.Provider == "" {
			v.Provider = "jina"
		}
		v.Model, v.HasKey = embedModelKey(cfg.Embed, v.Provider)
	case "rerank":
		v.Provider = cfg.Rerank.Driver
		if v.Provider == "" {
			v.Provider = "jina"
		}
		v.Model, v.HasKey = rerankModelKey(cfg.Rerank, cfg.Embed, v.Provider)
	}
	if v.HasKey {
		v.KeyMask = "(.env)"
	}
	return v
}

// PlatformModelConfigGetHandler lista as 4 etapas com a config EFETIVA: linha de
// platform_model_config (key mascarada) se existir, senão o default do .env.
func PlatformModelConfigGetHandler(pool *pgxpool.Pool, cipher *secret.Cipher, cfg config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cipher == nil {
			writeError(w, http.StatusServiceUnavailable, "CONFIG_ENC_KEY não configurada no engine")
			return
		}
		saved := map[string]platformconfig.View{}
		if views, err := platformconfig.Load(r.Context(), pool, cipher); err == nil {
			for _, v := range views {
				saved[v.Stage] = v
			}
		}
		out := make([]platformconfig.View, 0, len(platformconfig.Stages))
		for _, st := range platformconfig.Stages {
			if v, ok := saved[st]; ok && v.Provider != "" {
				out = append(out, v) // configurado via UI (platform)
			} else {
				out = append(out, envDefaultView(st, cfg)) // efetivo do .env
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"stages": out})
	}
}

type platformConfigPutRequest struct {
	Stage    string `json:"stage"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key"`
}

// PlatformModelConfigPutHandler grava/atualiza uma etapa.
func PlatformModelConfigPutHandler(pool *pgxpool.Pool, cipher *secret.Cipher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cipher == nil {
			writeError(w, http.StatusServiceUnavailable, "CONFIG_ENC_KEY não configurada no engine")
			return
		}
		var req platformConfigPutRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
		defer func() { _ = r.Body.Close() }()
		if !platformconfig.ValidStage(req.Stage) {
			writeError(w, http.StatusBadRequest, "stage inválido (generation|extraction|embed|rerank)")
			return
		}
		if req.Provider == "" {
			writeError(w, http.StatusBadRequest, "provider é obrigatório")
			return
		}
		if err := validateBaseURL(req.BaseURL); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// Audit (control-plane): mudança de model-config é alto impacto (redirect do LLM).
		slog.Warn("audit: model-config alterado",
			"stage", req.Stage, "provider", req.Provider, "model", req.Model,
			"base_url", req.BaseURL, "key_changed", req.APIKey != "")
		err := platformconfig.Set(r.Context(), pool, cipher, platformconfig.Config{
			Stage: req.Stage, Provider: req.Provider, Model: req.Model,
			BaseURL: req.BaseURL, APIKey: req.APIKey,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

type platformConfigTestRequest struct {
	Stage    string `json:"stage"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key"`
}

type platformConfigTestResponse struct {
	OK        bool   `json:"ok"`
	LatencyMs int    `json:"latency_ms"`
	Sample    string `json:"sample,omitempty"`
	Error     string `json:"error,omitempty"`
}

// PlatformModelConfigTestHandler testa a conexão SEM salvar, em qualquer etapa.
// Resolve a key: usa a do request; se vazia, a salva em platform_model_config;
// se nenhuma, "" → o builder cai na key do .env do provider.
func PlatformModelConfigTestHandler(pool *pgxpool.Pool, cipher *secret.Cipher, cfg config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req platformConfigTestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
		defer func() { _ = r.Body.Close() }()
		if !platformconfig.ValidStage(req.Stage) {
			writeError(w, http.StatusBadRequest, "stage inválido")
			return
		}
		if req.Provider == "" {
			writeError(w, http.StatusBadRequest, "provider é obrigatório")
			return
		}
		if err := validateBaseURL(req.BaseURL); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		// Resolve a key: request > salva (platform) > "" (.env via builder)
		key := req.APIKey
		if key == "" && cipher != nil {
			if stored, _ := platformconfig.Get(r.Context(), pool, cipher, req.Stage); stored != nil && stored.APIKey != "" {
				key = stored.APIKey
			}
		}

		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		start := time.Now()
		var sample string
		var terr error

		switch req.Stage {
		case "generation", "extraction":
			prov, err := llm.BuildForWithKey(cfg.LLM, req.Provider, req.Model, key, req.BaseURL)
			if err != nil {
				writeJSON(w, http.StatusOK, platformConfigTestResponse{OK: false, Error: err.Error()})
				return
			}
			res, e := prov.Complete(ctx, llm.Prompt{User: "Reply with the single word: pong", MaxTokens: 16, Temperature: 0})
			terr = e
			sample = trunc(res.Content, 80)
		case "embed":
			p, err := embed.BuildForKey(cfg.Embed, req.Provider, req.Model, key, req.BaseURL)
			if err != nil {
				writeJSON(w, http.StatusOK, platformConfigTestResponse{OK: false, Error: err.Error()})
				return
			}
			vecs, e := p.Embed(ctx, []string{"ping"}, embed.InputTypeQuery)
			terr = e
			if e == nil && len(vecs) == 1 {
				sample = fmt.Sprintf("%d dims", len(vecs[0]))
			}
		case "rerank":
			p, err := rerank.BuildForKey(cfg.Rerank, cfg.Embed, req.Provider, req.Model, key, req.BaseURL)
			if err != nil {
				writeJSON(w, http.StatusOK, platformConfigTestResponse{OK: false, Error: err.Error()})
				return
			}
			if p == nil {
				writeJSON(w, http.StatusOK, platformConfigTestResponse{OK: false, Error: "rerank driver 'none' não testável"})
				return
			}
			res, e := p.Rerank(ctx, "ping", []string{"pong", "ping"}, 1)
			terr = e
			if e == nil {
				sample = fmt.Sprintf("%d resultado(s)", len(res))
			}
		}

		lat := int(time.Since(start).Milliseconds())
		if terr != nil {
			writeJSON(w, http.StatusOK, platformConfigTestResponse{OK: false, LatencyMs: lat, Error: terr.Error()})
			return
		}
		writeJSON(w, http.StatusOK, platformConfigTestResponse{OK: true, LatencyMs: lat, Sample: sample})
	}
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
