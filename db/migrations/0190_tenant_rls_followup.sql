-- Row-level security for the organization tables added after 0140_tenant_rls.sql
-- (pending sign-ins, organization data keys, gateway budgets and alerts), with
-- the same policy: visible and writable only for the connection's
-- blaxsmith.org_id, or in system mode.
--
-- Every future table with an organization_id column needs the same ENABLE,
-- FORCE and blaxsmith_tenant policy in its own migration, and every code path
-- that reaches it must scope its context with internal/tenant (Org or System).
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['access_pending_sign_ins', 'access_organization_keys',
        'gateway_budget_settings', 'gateway_budgets', 'gateway_alerts'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS blaxsmith_tenant ON %I', t);
        EXECUTE format($p$CREATE POLICY blaxsmith_tenant ON %I
            USING (current_setting('blaxsmith.system', true) = 'on'
                OR organization_id::text = current_setting('blaxsmith.org_id', true))
            WITH CHECK (current_setting('blaxsmith.system', true) = 'on'
                OR organization_id::text = current_setting('blaxsmith.org_id', true))$p$, t);
    END LOOP;
END
$$;
