#!/bin/bash
# Postgres init script — roda APENAS no primeiro start do container (PGDATA vazio).
#
# Cria:
#  - Role `nexus_app` (NON-superuser, NON-BYPASSRLS) — usuário de runtime da aplicação
#
# A role admin (POSTGRES_USER) é usada SÓ pra migrations (cria DDL, applica RLS).
# Aplicação em runtime conecta como `nexus_app` — isso garante que RLS é exercitada.
#
# Conforme `docs/07-auth-and-tenancy.md`: BYPASSRLS+SUPERUSER tornam RLS decorativa.

set -e

# Variáveis do compose: APP_USER_PASSWORD precisa estar no .env
: "${APP_USER_PASSWORD:?APP_USER_PASSWORD env var requerida no compose .env}"

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL
    -- Role de runtime — NÃO superuser, NÃO BYPASSRLS
    CREATE ROLE nexus_app LOGIN PASSWORD '$APP_USER_PASSWORD';

    -- Grants serão dados pelas migrations (não cria tabelas ainda)
    GRANT USAGE ON SCHEMA public TO nexus_app;

    -- Default privileges — quando admin criar tabelas (via migrations), nexus_app
    -- ganha automaticamente SELECT/INSERT/UPDATE/DELETE
    ALTER DEFAULT PRIVILEGES IN SCHEMA public
        GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO nexus_app;
    ALTER DEFAULT PRIVILEGES IN SCHEMA public
        GRANT USAGE, SELECT ON SEQUENCES TO nexus_app;
    ALTER DEFAULT PRIVILEGES IN SCHEMA public
        GRANT EXECUTE ON FUNCTIONS TO nexus_app;

    -- Confirma estado
    SELECT rolname, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = 'nexus_app';
EOSQL

echo "→ nexus_app criada (non-superuser, non-bypassrls)"
