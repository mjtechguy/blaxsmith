-- Tenant isolation backstop. Every query already filters organization_id; row
-- level security makes a query that forgets to unable to read or write another
-- organization's rows. The application connects as the table owner, so each
-- table is FORCEd. A connection is scoped when acquired (internal/tenant):
--   blaxsmith.org_id  the one organization the request may touch
--   blaxsmith.system  'on' only for cross-organization system work (dispatch,
--                     lease renewal, rollups, pruning, login and token lookups
--                     before an organization is known, migrations)
-- With neither set, organization-owned rows are invisible and unwritable.
-- Superusers and BYPASSRLS roles skip these policies; the application role
-- must be neither.
--
-- Not covered, because they have no organization_id column: identity_principals
-- and identity_organizations (shared across organizations; reached by login
-- before an organization is known), identity_installation, identity_login_limits,
-- bootstrap_challenges, bootstrap_owners (keyed by cluster/attempt, reached
-- only by the dispatcher), and blaxsmith_schema_migrations. The monthly
-- gateway_usage_events partitions are covered through their parent; only
-- partition maintenance names a partition directly.
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'access_bindings', 'access_connection_models', 'access_connection_recommended_models',
        'access_connections', 'access_grants', 'access_leases', 'access_oauth_sessions',
        'access_project_policies', 'access_provider_registrations', 'access_resource_grants',
        'access_secret_versions',
        'gateway_attempt_delivery', 'gateway_org_settings', 'gateway_price_overrides',
        'gateway_project_settings', 'gateway_run_usage', 'gateway_tokens', 'gateway_usage_daily',
        'gateway_usage_events',
        'identity_account_links', 'identity_audit_events', 'identity_memberships',
        'identity_refresh_tokens', 'identity_sessions',
        'workflow_attempt_handoffs', 'workflow_attempt_results', 'workflow_attempt_runtime',
        'workflow_attempt_steers', 'workflow_attempts', 'workflow_command_exits',
        'workflow_corrections', 'workflow_escalations', 'workflow_events',
        'workflow_extension_versions', 'workflow_extensions', 'workflow_interaction_cursors',
        'workflow_interactions', 'workflow_project_admins', 'workflow_project_model_grants',
        'workflow_project_sources', 'workflow_project_verification', 'workflow_projects',
        'workflow_recipe_versions', 'workflow_recipes', 'workflow_review_decisions',
        'workflow_review_packages', 'workflow_run_bundles', 'workflow_run_extensions',
        'workflow_runs', 'workflow_task_dependencies', 'workflow_tasks',
        'workflow_tool_runtime_approvals'
    ] LOOP
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
