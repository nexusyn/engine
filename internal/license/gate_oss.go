//go:build !enterprise

// Package license — OSS build. O núcleo open-source do Nexusyn NÃO tem trava de
// licença: este arquivo fornece um gate no-op e entitlements "tudo liberado".
// A implementação real (verificação Ed25519 + heartbeat + trava por assinatura)
// vive nos arquivos *_enterprise.go, que só compilam com -tags=enterprise e são
// removidos do export OSS (ver nexusyn-oss-prep/export-oss.sh).
package license

import (
	"context"
	"log/slog"
	"net/http"
)

// Install é no-op no build OSS: devolve um middleware pass-through. Assinatura
// idêntica à do build enterprise, pra o main.go chamar sem #ifdef.
func Install(ctx context.Context, logger *slog.Logger) func(http.Handler) http.Handler {
	logger.Info("license gate desativado (build OSS / sem edição enterprise)")
	return func(next http.Handler) http.Handler { return next }
}

// AIIntegrated no OSS é sempre true (o core aberto não restringe provider de IA;
// quem liga/desliga IA é a config de env do operador).
func AIIntegrated() bool { return true }

// Edition no OSS é vazio ("core").
func Edition() string { return "" }
