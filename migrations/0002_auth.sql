-- 0002_auth.sql — API tokens (Sanctum-equivalent)
--
-- Tokens são SHA-256 hashes (não bcrypt — tokens são alta entropia, hash rápido ok).
-- Formato do token plain: "{id}|{random_32_bytes_hex}" — comparamos hash(random_part).
-- O id ajuda lookup eficiente sem precisar full-table scan.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE api_tokens (
    id              BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    token_hash      TEXT NOT NULL UNIQUE,           -- SHA-256 do random_part
    abilities       TEXT[] NOT NULL DEFAULT '{*}',   -- scopes (admin, read, write, bench, etc.)
    last_used_at    TIMESTAMPTZ,
    expires_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX api_tokens_hash_idx ON api_tokens(token_hash);
CREATE INDEX api_tokens_org_idx ON api_tokens(organization_id);
CREATE INDEX api_tokens_expires_idx ON api_tokens(expires_at) WHERE expires_at IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS api_tokens;
-- +goose StatementEnd
