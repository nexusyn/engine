-- 0028_delete_org.sql — exclusão permanente (purge) de uma org e TODOS os seus
-- dados. Hard delete FÍSICO e IRREVERSÍVEL: DELETE FROM organizations cascateia
-- em todas as tabelas (todas têm organization_id ... ON DELETE CASCADE — pages,
-- chunks, embeddings, entities, edges, sessions, events, usage_records,
-- org_limits, org_usage, webhooks, api_tokens, agents, user_profiles, …).
--
-- Control-plane (DELETE /v1/admin/orgs/{id}), chamado pelo console na execução da
-- exclusão de conta pós-carência (LGPD art. 18 VI — direito à eliminação). Mesmo
-- padrão SECURITY DEFINER de set_org_limits/create_org_with_token (escreve
-- bypassando RLS; caller = nexus_app, sem elevar privilégio).

-- +goose Up
-- +goose StatementBegin

-- A função roda como nexus_service (SECURITY DEFINER). Ele já tem INSERT em
-- organizations (0018) mas não DELETE — concede aqui. O cascade nas tabelas
-- filhas é ação do sistema (não exige privilégio nelas).
GRANT DELETE ON organizations TO nexus_service;

CREATE OR REPLACE FUNCTION delete_org(p_org bigint)
RETURNS bigint
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    v_deleted bigint;
BEGIN
    DELETE FROM organizations WHERE id = p_org;
    GET DIAGNOSTICS v_deleted = ROW_COUNT;
    RETURN v_deleted; -- 1 = apagou (cascade), 0 = org não existia
END;
$$;

ALTER FUNCTION delete_org(bigint) OWNER TO nexus_service;
REVOKE ALL ON FUNCTION delete_org(bigint) FROM PUBLIC;
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_app') THEN
        GRANT EXECUTE ON FUNCTION delete_org(bigint) TO nexus_app;
    END IF;
END $$;

COMMENT ON FUNCTION delete_org(bigint) IS
    'SECURITY DEFINER (owner nexus_service/BYPASSRLS): purge IRREVERSÍVEL de uma '
    'org e TODOS os seus dados (cascade). Control-plane (DELETE /v1/admin/orgs/{id}), '
    'chamado pelo console na exclusão de conta pós-carência (LGPD). Retorna 1 se '
    'apagou, 0 se a org não existia.';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS delete_org(bigint);
-- +goose StatementEnd
