-- Model gateway across replicas (docs/model-gateway-plan.md §4, §5), and
-- Azure OpenAI routes.
--
-- Admission-critical route state is shared through PostgreSQL: a 429's
-- cooldown and a known reset, the circuit breaker (open, and the single
-- half-open probe), and concurrency slots. Each replica keeps a fast
-- in-memory view and reads this state with a short TTL before choosing.

ALTER TABLE gateway_route_state
    ADD COLUMN opened_at timestamptz,
    ADD COLUMN probe_until timestamptz,
    ADD COLUMN consecutive_failures integer NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0);

-- Concurrency slots per replica. A slot is a route id, or 'pool:<id>' for a
-- pool's cap. Rows whose replica stopped heartbeating are ignored, so a
-- crashed replica cannot hold slots forever.
CREATE TABLE gateway_route_inflight (
    organization_id uuid NOT NULL,
    slot text NOT NULL CHECK (length(slot) BETWEEN 1 AND 80),
    replica text NOT NULL CHECK (replica ~ '^[a-f0-9]{16}$'),
    inflight integer NOT NULL DEFAULT 0 CHECK (inflight >= 0),
    heartbeat_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, slot, replica)
);

DO $$
BEGIN
    ALTER TABLE gateway_route_inflight ENABLE ROW LEVEL SECURITY;
    ALTER TABLE gateway_route_inflight FORCE ROW LEVEL SECURITY;
    CREATE POLICY blaxsmith_tenant ON gateway_route_inflight
        USING (current_setting('blaxsmith.system', true) = 'on'
            OR organization_id::text = current_setting('blaxsmith.org_id', true))
        WITH CHECK (current_setting('blaxsmith.system', true) = 'on'
            OR organization_id::text = current_setting('blaxsmith.org_id', true));
END
$$;

-- Azure OpenAI: an organization's Azure resource, its deployments as the
-- model map, and the API version. The key is an organization connection
-- (auth_method azure_api_key) used only as a route.
ALTER TABLE gateway_routes
    ADD COLUMN azure_resource text NOT NULL DEFAULT '' CHECK (azure_resource ~ '^([a-z0-9][a-z0-9-]{1,62})?$'),
    ADD COLUMN api_version text NOT NULL DEFAULT '' CHECK (api_version ~ '^([0-9]{4}-[0-9]{2}-[0-9]{2}(-preview)?)?$');
ALTER TABLE gateway_routes DROP CONSTRAINT gateway_routes_kind_check;
ALTER TABLE gateway_routes ADD CONSTRAINT gateway_routes_kind_check
    CHECK (kind IN ('anthropic', 'bedrock', 'vertex', 'openai', 'azure_openai', 'opencode_zen', 'opencode_go'));
ALTER TABLE gateway_routes ADD CONSTRAINT gateway_routes_azure_check
    CHECK ((kind = 'azure_openai') = (azure_resource <> '' AND api_version <> ''));

CREATE OR REPLACE FUNCTION gateway_route_connection_check() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM access_connections c
        WHERE c.organization_id = NEW.organization_id::text AND c.id = NEW.connection_id
        AND c.owner_kind = 'organization'
        AND c.auth_method IN ('api_key', 'aws_sigv4', 'gcp_service_account', 'azure_api_key')
        AND (c.auth_method = 'aws_sigv4') = (NEW.kind = 'bedrock')
        AND (c.auth_method = 'gcp_service_account') = (NEW.kind = 'vertex')
        AND (c.auth_method = 'azure_api_key') = (NEW.kind = 'azure_openai')) THEN
        RAISE EXCEPTION 'gateway routes use organization-owned API-key or cloud connections only'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION gateway_pool_route_check() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM gateway_pools p JOIN gateway_routes r
            ON r.organization_id = p.organization_id AND r.id = NEW.route_id
        WHERE p.organization_id = NEW.organization_id AND p.id = NEW.pool_id
        AND CASE p.family
            WHEN 'anthropic' THEN r.kind IN ('anthropic', 'bedrock', 'vertex')
            WHEN 'openai' THEN r.kind IN ('openai', 'azure_openai')
            WHEN 'opencode' THEN r.kind = 'opencode_zen'
            WHEN 'opencode-go' THEN r.kind = 'opencode_go'
        END) THEN
        RAISE EXCEPTION 'route kind does not serve this pool''s model family' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;
