-- 0036_pages_unique_include_domain.sql — corrige a unique de pages pra incluir domain.
--
-- O modelo LÓGICO identifica uma página por (organization_id, domain, slug): o
-- compile faz UPDATE ... WHERE organization_id=$1 AND slug=$2 AND domain=$5, e o
-- pipeline destila o MESMO assunto em domains distintos (wiki/decision/lesson/
-- error). Mas a unique era (organization_id, slug, transaction_time) — SEM domain.
--
-- Consequência (bug latente): quando duas páginas de domains diferentes acabam com
-- o mesmo slug (ex.: a wiki e a decision do tópico "reachyn-console-nginx-php-fpm"),
-- qualquer UPDATE em massa que iguale transaction_time (o trigger pages_set_
-- transaction_time força now(), constante na transação — ex.: o `nexus reextract`)
-- gera (org, slug, now()) duplicado → viola a unique e aborta a transação.
--
-- Incluir domain na unique alinha a constraint ao modelo lógico, remove a colisão
-- e previne o caso pra qualquer par de domains. Confirmado seguro: 0 violações nos
-- dados de prod e nenhum ON CONFLICT depende desta constraint (os ON CONFLICT por
-- (org, slug) existentes são de entities/agents, não de pages).

-- +goose Up
-- +goose StatementBegin
ALTER TABLE pages DROP CONSTRAINT IF EXISTS pages_organization_id_slug_transaction_time_key;
ALTER TABLE pages DROP CONSTRAINT IF EXISTS pages_org_domain_slug_txtime_key;
ALTER TABLE pages ADD CONSTRAINT pages_org_domain_slug_txtime_key
    UNIQUE (organization_id, domain, slug, transaction_time);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE pages DROP CONSTRAINT IF EXISTS pages_org_domain_slug_txtime_key;
ALTER TABLE pages ADD CONSTRAINT pages_organization_id_slug_transaction_time_key
    UNIQUE (organization_id, slug, transaction_time);
-- +goose StatementEnd
