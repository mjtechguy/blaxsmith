-- Remove the model gateway (0100, 0160, 0170, 0200, 0210). Proxying, routes,
-- pools, pacing, usage metering, costs, and budgets move to a separate service
-- (docs/archive/model-gateway-plan.md); Blaxsmith reaches it as an ordinary
-- API-key connection with a base URL, added here. Every statement is
-- idempotent. Migrations run with blaxsmith.system on, so row-level security
-- does not hide any organization's rows.

-- Tables, in dependency order. Dropping gateway_usage_events drops its monthly
-- and default partitions; triggers go with their tables.
DROP TABLE IF EXISTS
    gateway_alerts, gateway_budgets, gateway_budget_settings,
    gateway_route_inflight, gateway_pool_routes, gateway_pools, gateway_routes,
    gateway_route_state, gateway_paced_tasks, gateway_subscription_limits,
    gateway_price_overrides, gateway_run_usage, gateway_usage_daily, gateway_usage_events,
    gateway_tokens, gateway_attempt_delivery, gateway_project_settings, gateway_org_settings;

-- A partition detached by hand would survive its parent.
DO $$
DECLARE
    t text;
BEGIN
    FOR t IN SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = current_schema() AND c.relkind IN ('r', 'p')
        AND c.relname LIKE 'gateway\_usage\_events\_%' LOOP
        EXECUTE format('DROP TABLE IF EXISTS %I', t);
    END LOOP;
END
$$;

DROP FUNCTION IF EXISTS gateway_ensure_usage_partition(date);
DROP FUNCTION IF EXISTS gateway_route_connection_check();
DROP FUNCTION IF EXISTS gateway_pool_route_check();

-- Pools were granted to projects as resource kind gateway_pool.
DELETE FROM access_resource_grants WHERE resource_kind = 'gateway_pool';
ALTER TABLE access_resource_grants DROP CONSTRAINT IF EXISTS access_resource_grants_resource_kind_check;
ALTER TABLE access_resource_grants ADD CONSTRAINT access_resource_grants_resource_kind_check
    CHECK (resource_kind IN ('connection', 'recipe', 'extension'));

-- brokered_gateway is no longer a delivery mode. Any grant still using it is
-- revoked with its project model selection and leases, like a connection
-- revocation; revoked rows keep their history.
WITH revoked AS (
    UPDATE access_grants SET revoked_at = clock_timestamp(), version = version + 1
    WHERE delivery_mode = 'brokered_gateway' AND revoked_at IS NULL
    RETURNING organization_id, id
), selections AS (
    UPDATE workflow_project_model_grants s SET revoked_at = clock_timestamp()
    FROM revoked r WHERE s.organization_id::text = r.organization_id AND s.grant_id = r.id AND s.revoked_at IS NULL
)
UPDATE access_leases l SET revoked_at = clock_timestamp()
FROM access_bindings b, revoked r
WHERE l.organization_id = b.organization_id AND l.binding_id = b.id AND l.revoked_at IS NULL
AND b.organization_id = r.organization_id AND b.grant_id = r.id;

ALTER TABLE access_grants DROP CONSTRAINT IF EXISTS access_grants_delivery_mode_check;
ALTER TABLE access_grants ADD CONSTRAINT access_grants_delivery_mode_check
    CHECK (delivery_mode IN ('brokered', 'short_lived', 'native_raw', 'oauth_access')
        OR (delivery_mode = 'brokered_gateway' AND revoked_at IS NOT NULL));

UPDATE access_project_policies
SET delivery_modes = array_remove(delivery_modes, 'brokered_gateway'), version = version + 1
WHERE 'brokered_gateway' = ANY (delivery_modes) AND cardinality(array_remove(delivery_modes, 'brokered_gateway')) > 0;

-- Cloud route credentials (Bedrock, Vertex, Azure OpenAI) were usable only as
-- gateway routes. They are revoked, like any revoked connection; their
-- provider registrations, which allowed only brokered_gateway, are disabled.
UPDATE access_leases l SET revoked_at = clock_timestamp()
FROM access_connections c
WHERE l.organization_id = c.organization_id AND l.connection_id = c.id AND l.revoked_at IS NULL
AND c.auth_method IN ('aws_sigv4', 'gcp_service_account', 'azure_api_key');
UPDATE access_connections SET state = 'revoked'
WHERE auth_method IN ('aws_sigv4', 'gcp_service_account', 'azure_api_key') AND state <> 'revoked';
UPDATE access_provider_registrations SET state = 'disabled'
WHERE delivery_modes = ARRAY['brokered_gateway'] AND state <> 'disabled';
UPDATE access_provider_registrations
SET delivery_modes = array_remove(delivery_modes, 'brokered_gateway')
WHERE 'brokered_gateway' = ANY (delivery_modes) AND cardinality(array_remove(delivery_modes, 'brokered_gateway')) > 0;

-- Base URL on API-key connections: an OpenAI- or Anthropic-compatible
-- endpoint (LiteLLM, a company gateway) used in place of the provider's own.
-- Not secret. The application normalizes it (https, no userinfo, query, or
-- fragment, no trailing slash); here it is bounded and kept off subscriptions
-- and every other connection kind.
ALTER TABLE access_connections ADD COLUMN IF NOT EXISTS base_url text NOT NULL DEFAULT '';
ALTER TABLE access_connections DROP CONSTRAINT IF EXISTS access_connections_base_url_check;
ALTER TABLE access_connections ADD CONSTRAINT access_connections_base_url_check
    CHECK (base_url = '' OR (auth_method = 'api_key' AND length(base_url) <= 512 AND base_url ~ '^https?://[^/?#@[:space:]]+(/[^?#[:space:]]*[^/?#[:space:]])?$'));

-- The base URL an attempt was dispatched with. The public tool request (and
-- so the AX command digest) derives from it, and credential release and lease
-- renewal require the connection to still have it.
ALTER TABLE access_bindings ADD COLUMN IF NOT EXISTS model_base_url text NOT NULL DEFAULT '';
ALTER TABLE access_bindings DROP CONSTRAINT IF EXISTS access_bindings_model_base_url_check;
ALTER TABLE access_bindings ADD CONSTRAINT access_bindings_model_base_url_check
    CHECK (length(model_base_url) <= 512 AND (model_base_url = '' OR capability = 'model.invoke'));
