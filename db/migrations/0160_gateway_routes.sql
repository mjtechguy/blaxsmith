-- Model gateway G2 and G4 (docs/model-gateway-plan.md §4, §5, §6, §10, §15.1):
-- routes, pools of organization-owned API-key and cloud connections, route
-- state, dispatcher pacing, and personal subscription limit meters.
--
-- Policy, enforced here as well as in code: a route is always an
-- organization-owned API-key or cloud connection. Personal subscriptions
-- (Codex ChatGPT sign-ins, Claude setup-tokens) can never be a route, so no
-- pool can contain one and nothing ever fails over to someone else's account.

-- §15.1 switches this phase makes real, plus §16 raw event retention.
ALTER TABLE gateway_org_settings
    ADD COLUMN pools_enabled boolean NOT NULL DEFAULT false,
    ADD COLUMN pacing_enabled boolean NOT NULL DEFAULT false,
    ADD COLUMN personal_routes_enabled boolean NOT NULL DEFAULT false,
    ADD COLUMN event_retention_days integer NOT NULL DEFAULT 90 CHECK (event_retention_days BETWEEN 7 AND 400);

-- §4 route: one connection plus an endpoint kind. Bedrock and Vertex routes
-- serve the Anthropic Messages API through their own transport and auth;
-- their credentials are organization connections (auth_method aws_sigv4 or
-- gcp_service_account) that are never granted to projects directly.
CREATE TABLE gateway_routes (
    organization_id uuid NOT NULL REFERENCES identity_organizations (id),
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    name text NOT NULL CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$'),
    connection_id text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('anthropic', 'bedrock', 'vertex', 'openai', 'opencode_zen', 'opencode_go')),
    region text NOT NULL DEFAULT '' CHECK (region ~ '^[a-z0-9-]{0,32}$'),
    cloud_project text NOT NULL DEFAULT '' CHECK (cloud_project ~ '^([a-z][a-z0-9-]{4,62})?$'),
    -- Our model id -> the route's model id (Bedrock model or inference
    -- profile ids, Vertex model names). Empty passes ids through unchanged.
    model_map jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(model_map) = 'object'),
    weight integer NOT NULL DEFAULT 1 CHECK (weight BETWEEN 1 AND 1000),
    priority integer NOT NULL DEFAULT 100 CHECK (priority BETWEEN 0 AND 1000),
    concurrency_cap integer NOT NULL DEFAULT 0 CHECK (concurrency_cap BETWEEN 0 AND 10000),
    -- §5 configured quota for routes whose provider sends no remaining-quota
    -- headers (Bedrock, Vertex). 0 = not configured.
    requests_per_minute integer NOT NULL DEFAULT 0 CHECK (requests_per_minute BETWEEN 0 AND 1000000),
    tokens_per_minute bigint NOT NULL DEFAULT 0 CHECK (tokens_per_minute BETWEEN 0 AND 1000000000),
    state text NOT NULL DEFAULT 'enabled' CHECK (state IN ('enabled', 'draining', 'disabled')),
    created_by uuid NOT NULL REFERENCES identity_principals (id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, connection_id),
    UNIQUE (organization_id, name),
    CHECK ((kind IN ('bedrock', 'vertex')) = (region <> '')),
    CHECK ((kind = 'vertex') = (cloud_project <> ''))
);

CREATE FUNCTION gateway_route_connection_check() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM access_connections c
        WHERE c.organization_id = NEW.organization_id::text AND c.id = NEW.connection_id
        AND c.owner_kind = 'organization'
        AND c.auth_method IN ('api_key', 'aws_sigv4', 'gcp_service_account')
        AND (c.auth_method = 'aws_sigv4') = (NEW.kind = 'bedrock')
        AND (c.auth_method = 'gcp_service_account') = (NEW.kind = 'vertex')) THEN
        RAISE EXCEPTION 'gateway routes use organization-owned API-key or cloud connections only'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER gateway_route_connection_check BEFORE INSERT OR UPDATE OF connection_id, kind ON gateway_routes
    FOR EACH ROW EXECUTE FUNCTION gateway_route_connection_check();

-- §4 pool: a named set of routes serving one model family, granted to
-- projects through access_resource_grants (resource kind gateway_pool).
CREATE TABLE gateway_pools (
    organization_id uuid NOT NULL REFERENCES identity_organizations (id),
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    name text NOT NULL CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$'),
    family text NOT NULL CHECK (family IN ('anthropic', 'openai', 'opencode', 'opencode-go')),
    strategy text NOT NULL DEFAULT 'priority_headroom' CHECK (strategy IN ('priority_headroom', 'weighted', 'fill_first')),
    concurrency_cap integer NOT NULL DEFAULT 0 CHECK (concurrency_cap BETWEEN 0 AND 100000),
    affinity boolean NOT NULL DEFAULT true,
    state text NOT NULL DEFAULT 'enabled' CHECK (state IN ('enabled', 'disabled')),
    created_by uuid NOT NULL REFERENCES identity_principals (id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, name)
);

CREATE TABLE gateway_pool_routes (
    organization_id uuid NOT NULL,
    pool_id uuid NOT NULL,
    route_id uuid NOT NULL,
    PRIMARY KEY (organization_id, pool_id, route_id),
    FOREIGN KEY (organization_id, pool_id) REFERENCES gateway_pools (organization_id, id) ON DELETE CASCADE,
    FOREIGN KEY (organization_id, route_id) REFERENCES gateway_routes (organization_id, id) ON DELETE CASCADE
);

CREATE INDEX gateway_pool_routes_by_route ON gateway_pool_routes (organization_id, route_id);

-- A pool member must serve the pool's family. Personal subscriptions cannot
-- be routes at all (gateway_route_connection_check), so they cannot be here.
CREATE FUNCTION gateway_pool_route_check() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM gateway_pools p JOIN gateway_routes r
            ON r.organization_id = p.organization_id AND r.id = NEW.route_id
        WHERE p.organization_id = NEW.organization_id AND p.id = NEW.pool_id
        AND CASE p.family
            WHEN 'anthropic' THEN r.kind IN ('anthropic', 'bedrock', 'vertex')
            WHEN 'openai' THEN r.kind = 'openai'
            WHEN 'opencode' THEN r.kind = 'opencode_zen'
            WHEN 'opencode-go' THEN r.kind = 'opencode_go'
        END) THEN
        RAISE EXCEPTION 'route kind does not serve this pool''s model family' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER gateway_pool_route_check BEFORE INSERT OR UPDATE ON gateway_pool_routes
    FOR EACH ROW EXECUTE FUNCTION gateway_pool_route_check();

ALTER TABLE access_resource_grants DROP CONSTRAINT access_resource_grants_resource_kind_check;
ALTER TABLE access_resource_grants ADD CONSTRAINT access_resource_grants_resource_kind_check
    CHECK (resource_kind IN ('connection', 'recipe', 'extension', 'gateway_pool'));

-- §5 route state, persisted about every 5 s by the gateway for the UI, the
-- dispatcher's headroom check, and other replicas. route_id is a
-- gateway_routes id, or the connection id of an implicit G1 route.
CREATE TABLE gateway_route_state (
    organization_id uuid NOT NULL,
    route_id text NOT NULL CHECK (length(route_id) BETWEEN 1 AND 64),
    breaker text NOT NULL DEFAULT 'closed' CHECK (breaker IN ('closed', 'open', 'half_open')),
    cooldown_until timestamptz,
    inflight integer NOT NULL DEFAULT 0 CHECK (inflight >= 0),
    -- {"requests":{"limit":n,"remaining":n,"reset_at":"RFC 3339"},"tokens":{...},...}
    metrics jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(metrics) = 'object'),
    requests_15m integer NOT NULL DEFAULT 0,
    errors_15m integer NOT NULL DEFAULT 0,
    last_status integer NOT NULL DEFAULT 0,
    last_429_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, route_id)
);

-- §5 back-pressure: a ready stage whose pool has no headroom stays queued
-- with this visible reason; dispatch skips it until retry_at.
CREATE TABLE gateway_paced_tasks (
    organization_id uuid NOT NULL,
    task_id uuid NOT NULL,
    run_id uuid NOT NULL,
    pool_id uuid NOT NULL,
    reason text NOT NULL CHECK (length(reason) BETWEEN 1 AND 200),
    resets_at timestamptz,
    retry_at timestamptz NOT NULL,
    since timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, task_id),
    FOREIGN KEY (organization_id, task_id) REFERENCES workflow_tasks (organization_id, id)
);

CREATE INDEX gateway_paced_tasks_by_run ON gateway_paced_tasks (organization_id, run_id);

-- §6 personal subscription rate-limit windows as the provider reported them
-- on the owner's own gateway traffic. Shown only to the connection owner.
CREATE TABLE gateway_subscription_limits (
    organization_id uuid NOT NULL,
    connection_id text NOT NULL,
    principal_id uuid NOT NULL,
    window_name text NOT NULL CHECK (window_name ~ '^[a-z0-9_]{1,32}$'),
    used_pct double precision NOT NULL CHECK (used_pct >= 0 AND used_pct <= 1000),
    window_minutes integer CHECK (window_minutes > 0),
    resets_at timestamptz,
    observed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, connection_id, window_name)
);

CREATE INDEX gateway_subscription_limits_by_owner ON gateway_subscription_limits (organization_id, principal_id);
