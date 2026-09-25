-- Row-level security for the model gateway G2/G4 tables
-- (0160_gateway_routes.sql), with the tenant policy of 0140_tenant_rls.sql:
-- visible and writable only for the connection's blaxsmith.org_id, or in
-- system mode. Cross-organization gateway work (token authorization, route
-- state persistence, dispatcher pacing, retention) runs with tenant.System.
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['gateway_routes', 'gateway_pools', 'gateway_pool_routes',
        'gateway_route_state', 'gateway_paced_tasks', 'gateway_subscription_limits'] LOOP
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
