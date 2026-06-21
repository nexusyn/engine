# NEXUS — Makefile
#
# Comandos padronizados pro projeto. Cada target documentado em docs/04-makefile.md.

# ───── Variáveis ─────
GO          ?= go
BINARY      := nexus
BINDIR      := bin
GOFLAGS     := -trimpath
LDFLAGS     := -s -w \
               -X main.version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev) \
               -X main.commit=$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown) \
               -X main.buildTime=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# Postgres connection pra migrations / dev local
DB_URL      ?= postgres://nexus:secret@localhost:5432/nexus?sslmode=disable

# Docker image tags
IMAGE_NEXUS     := nexus:latest
IMAGE_DASHBOARD := nexus-dashboard:latest

.DEFAULT_GOAL := help

# ───── Help ─────
.PHONY: help
help: ## Mostra esta ajuda
	@awk 'BEGIN {FS = ":.*##"; printf "\nNEXUS — Makefile targets:\n\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@echo ""

# ───── Build ─────
.PHONY: build
build: ## Compila o binário em bin/nexus
	@mkdir -p $(BINDIR)
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINDIR)/$(BINARY) ./cmd/nexus
	@echo "→ binary: $(BINDIR)/$(BINARY) ($(shell du -h $(BINDIR)/$(BINARY) 2>/dev/null | cut -f1 || echo '?'))"

.PHONY: install
install: ## Instala em $$GOPATH/bin
	$(GO) install $(GOFLAGS) -ldflags "$(LDFLAGS)" ./cmd/nexus

.PHONY: clean
clean: ## Remove artifacts de build
	rm -rf $(BINDIR) dist coverage.out coverage.html

# ───── Test ─────
.PHONY: test
test: ## Roda testes unitários
	$(GO) test -race -count=1 ./...

.PHONY: test-integration
test-integration: ## Roda testes de integração (precisa Docker — usa testcontainers pra Postgres efêmero)
	$(GO) test -race -count=1 -tags=integration ./...

.PHONY: test-rls
test-rls: ## Teste específico de RLS (cross-tenant leak)
	$(GO) test -race -count=1 -tags=integration -run TestRLS ./internal/tenant/...

.PHONY: test-coverage
test-coverage: ## Gera report HTML de coverage
	$(GO) test -race -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "→ coverage report: coverage.html"

# ───── Lint ─────
.PHONY: lint
lint: ## Roda golangci-lint
	golangci-lint run --timeout 5m

.PHONY: fmt
fmt: ## Formata código (gofmt + goimports)
	gofmt -w -s .
	@command -v goimports >/dev/null && goimports -w -local github.com/nexusyn/engine . || echo "goimports não instalado (opcional)"

.PHONY: tidy
tidy: ## go mod tidy
	$(GO) mod tidy

# ───── Migrations ─────
.PHONY: migrate-up
migrate-up: ## Roda migrations pendentes
	$(GO) run ./cmd/nexus migrate up

.PHONY: migrate-down
migrate-down: ## Reverte última migration
	$(GO) run ./cmd/nexus migrate down

.PHONY: migrate-status
migrate-status: ## Lista status das migrations
	$(GO) run ./cmd/nexus migrate status

# ───── Docker ─────
.PHONY: docker-build
docker-build: docker-build-nexus ## Build de todas imagens

.PHONY: docker-build-nexus
docker-build-nexus: ## Build da imagem nexus (Go)
	docker build -f docker/Dockerfile.nexus -t $(IMAGE_NEXUS) .

# ───── Docker Compose (stack 'nexus') ─────
.PHONY: compose-up
compose-up: ## Sobe stack completa (nexus + postgres + dashboard)
	docker compose -p nexus up -d

.PHONY: compose-down
compose-down: ## Derruba stack (preserva volumes)
	docker compose -p nexus down

.PHONY: compose-purge
compose-purge: ## Derruba stack + APAGA volumes (cuidado: perde dados)
	docker compose -p nexus down -v

.PHONY: compose-logs
compose-logs: ## Tail logs do nexus
	docker compose -p nexus logs -f nexus

.PHONY: compose-ps
compose-ps: ## Status dos containers
	docker compose -p nexus ps

# ───── Dev convenience ─────
.PHONY: run
run: ## Roda servidor em modo dev (sem build cached)
	$(GO) run ./cmd/nexus serve

.PHONY: dev
dev: compose-up ## Sobe Postgres e abre logs do nexus
	@sleep 2
	@$(MAKE) compose-logs

# ───── Bench ─────
.PHONY: bench-quick
bench-quick: ## Roda bench LongMemEval N=30
	./bin/$(BINARY) bench longmemeval --sample=30

.PHONY: bench-full
bench-full: ## Roda bench LongMemEval N=400 (full)
	./bin/$(BINARY) bench longmemeval --sample=400

# ───── Setup ─────
.PHONY: setup
setup: ## Instala deps + ferramentas dev (golangci-lint, goimports, gh)
	$(GO) mod download
	@command -v golangci-lint >/dev/null || $(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.62.2
	@command -v goimports >/dev/null || $(GO) install golang.org/x/tools/cmd/goimports@latest
	@command -v river >/dev/null || $(GO) install github.com/riverqueue/river/cmd/river@latest

.PHONY: env-example
env-example: ## Cria .env a partir do .env.example se ainda não existir
	@if [ ! -f .env ]; then cp .env.example .env && echo "→ .env criado a partir do .env.example. Edite com suas credenciais."; else echo "→ .env já existe."; fi
