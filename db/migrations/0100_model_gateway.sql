-- Model gateway G1 (docs/model-gateway-plan.md §3, §7, §10, §15.1): brokered
-- pass-through with run-scoped tokens, one route per connection, metering and
-- per-organization feature flags. No prompt or response content is stored.

-- §3 delivery mode: a grant may require brokering (the key never leaves the
-- platform). A native_raw grant may also be brokered when the project opts in.
ALTER TABLE access_grants DROP CONSTRAINT access_grants_delivery_mode_check;
ALTER TABLE access_grants ADD CONSTRAINT access_grants_delivery_mode_check
    CHECK (delivery_mode IN ('brokered', 'short_lived', 'native_raw', 'oauth_access', 'brokered_gateway'));

-- §15.1 organization switches. Later-phase switches (pools, pacing, budgets,
-- personal routes, members' Claude subscriptions, content capture) are not
-- stored: G1 shows them disabled.
CREATE TABLE gateway_org_settings (
    organization_id uuid PRIMARY KEY REFERENCES identity_organizations (id),
    enabled boolean NOT NULL DEFAULT false,
    default_delivery_mode text NOT NULL DEFAULT 'native_raw'
        CHECK (default_delivery_mode IN ('native_raw', 'brokered_gateway')),
    allow_project_choice boolean NOT NULL DEFAULT true,
    remove_direct_egress boolean NOT NULL DEFAULT true,
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    updated_by uuid REFERENCES identity_principals (id),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- Project → Settings → Model access. Absent means "use the organization default".
CREATE TABLE gateway_project_settings (
    organization_id uuid NOT NULL,
    project_id uuid NOT NULL,
    delivery_mode text NOT NULL CHECK (delivery_mode IN ('native_raw', 'brokered_gateway')),
    updated_by uuid REFERENCES identity_principals (id),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, project_id),
    FOREIGN KEY (organization_id, project_id) REFERENCES workflow_projects (organization_id, id)
);

-- The delivery mode each attempt was dispatched with. The public tool request
-- (and so the AX command digest) derives from this row, so completion and
-- recovery rebuild the same command even if the settings change mid-run.
CREATE TABLE gateway_attempt_delivery (
    organization_id uuid NOT NULL,
    attempt_id uuid NOT NULL,
    delivery_mode text NOT NULL CHECK (delivery_mode IN ('native_raw', 'brokered_gateway')),
    base_url text NOT NULL DEFAULT '' CHECK (length(base_url) <= 512),
    remove_direct_egress boolean NOT NULL DEFAULT false,
    harness text NOT NULL CHECK (harness IN ('codex', 'claude-code', 'opencode')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, attempt_id),
    FOREIGN KEY (organization_id, attempt_id) REFERENCES workflow_attempts (organization_id, id),
    CHECK ((delivery_mode = 'brokered_gateway') = (base_url <> ''))
);

-- §3 gateway tokens: opaque, stored as SHA-256 only, bound to one model lease.
-- Expiry and revocation are the lease's, read live on every request, so the
-- existing renewal loop renews the token and lease revocation revokes it.
CREATE TABLE gateway_tokens (
    token_sha256 bytea PRIMARY KEY CHECK (length(token_sha256) = 32),
    id uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    organization_id uuid NOT NULL,
    project_id uuid NOT NULL,
    run_id uuid NOT NULL,
    attempt_id uuid NOT NULL,
    task_id uuid NOT NULL,
    stage_key text NOT NULL,
    principal_id text NOT NULL DEFAULT '',
    harness text NOT NULL,
    provider text NOT NULL,
    model text NOT NULL,
    allowed_families text[] NOT NULL CHECK (cardinality(allowed_families) > 0),
    binding_id text NOT NULL,
    grantee_kind text NOT NULL,
    grantee_id text NOT NULL,
    policy_version bigint NOT NULL CHECK (policy_version > 0),
    lease_id text NOT NULL,
    lease_generation bigint NOT NULL CHECK (lease_generation > 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    revoked_at timestamptz,
    FOREIGN KEY (organization_id, attempt_id) REFERENCES workflow_attempts (organization_id, id)
);

CREATE INDEX gateway_tokens_by_lease ON gateway_tokens (organization_id, lease_id);
CREATE INDEX gateway_tokens_by_attempt ON gateway_tokens (organization_id, attempt_id);

-- §7 one event per completed or failed upstream request, partitioned by month.
CREATE TABLE gateway_usage_events (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL,
    project_id uuid NOT NULL,
    run_id uuid NOT NULL,
    attempt_id uuid NOT NULL,
    task_id uuid NOT NULL,
    stage_key text NOT NULL,
    principal_id text NOT NULL DEFAULT '',
    harness text NOT NULL,
    pool_id text NOT NULL DEFAULT '',
    route_id text NOT NULL,
    route_kind text NOT NULL,
    api text NOT NULL CHECK (api IN ('anthropic_messages', 'openai_responses', 'openai_chat', 'other')),
    requested_model text NOT NULL DEFAULT '',
    served_model text NOT NULL DEFAULT '',
    status text NOT NULL CHECK (status IN ('ok', 'error', 'cancelled')),
    http_status integer NOT NULL,
    retry_count integer NOT NULL DEFAULT 0,
    streamed boolean NOT NULL DEFAULT false,
    usage_reported boolean NOT NULL DEFAULT false,
    request_id text NOT NULL DEFAULT '' CHECK (length(request_id) <= 200),
    started_at timestamptz NOT NULL,
    ttft_ms integer,
    duration_ms integer NOT NULL DEFAULT 0,
    input_tokens bigint NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens bigint NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    cache_read_tokens bigint NOT NULL DEFAULT 0 CHECK (cache_read_tokens >= 0),
    cache_write_tokens bigint NOT NULL DEFAULT 0 CHECK (cache_write_tokens >= 0),
    reasoning_tokens bigint NOT NULL DEFAULT 0 CHECK (reasoning_tokens >= 0),
    cost_usd_micros bigint NOT NULL DEFAULT 0 CHECK (cost_usd_micros >= 0),
    price_version text NOT NULL DEFAULT '',
    PRIMARY KEY (started_at, id)
) PARTITION BY RANGE (started_at);

-- The default partition only catches rows if the monthly job ever falls behind.
CREATE TABLE gateway_usage_events_default PARTITION OF gateway_usage_events DEFAULT;

CREATE FUNCTION gateway_ensure_usage_partition(month date) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    first date := date_trunc('month', month)::date;
    name text := 'gateway_usage_events_' || to_char(first, 'YYYY_MM');
BEGIN
    IF to_regclass(name) IS NULL THEN
        EXECUTE format('CREATE TABLE %I PARTITION OF gateway_usage_events FOR VALUES FROM (%L) TO (%L)',
            name, first, (first + interval '1 month')::date);
    END IF;
END $$;

SELECT gateway_ensure_usage_partition((date_trunc('month', clock_timestamp()) + make_interval(months => m))::date)
FROM generate_series(-1, 2) AS m;

CREATE INDEX gateway_usage_events_by_run ON gateway_usage_events (organization_id, run_id, started_at);
CREATE INDEX gateway_usage_events_by_org ON gateway_usage_events (organization_id, started_at);

-- §7 rollups by (org, project, principal, pool, route, model, day).
CREATE TABLE gateway_usage_daily (
    organization_id uuid NOT NULL,
    day date NOT NULL,
    project_id uuid NOT NULL,
    principal_id text NOT NULL,
    pool_id text NOT NULL,
    route_id text NOT NULL,
    route_kind text NOT NULL,
    model text NOT NULL,
    requests bigint NOT NULL,
    errors bigint NOT NULL,
    rate_limited bigint NOT NULL,
    input_tokens bigint NOT NULL,
    output_tokens bigint NOT NULL,
    cache_read_tokens bigint NOT NULL,
    cache_write_tokens bigint NOT NULL,
    reasoning_tokens bigint NOT NULL,
    cost_usd_micros bigint NOT NULL,
    ttft_ms_sum bigint NOT NULL,
    ttft_count bigint NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, day, project_id, principal_id, pool_id, route_id, model)
);

CREATE INDEX gateway_usage_daily_by_project ON gateway_usage_daily (organization_id, project_id, day);
CREATE INDEX gateway_usage_daily_by_principal ON gateway_usage_daily (organization_id, principal_id, day);

-- Per-run totals, maintained with each event insert so run pages read one row.
CREATE TABLE gateway_run_usage (
    organization_id uuid NOT NULL,
    run_id uuid NOT NULL,
    project_id uuid NOT NULL,
    principal_id text NOT NULL DEFAULT '',
    requests bigint NOT NULL DEFAULT 0,
    errors bigint NOT NULL DEFAULT 0,
    input_tokens bigint NOT NULL DEFAULT 0,
    output_tokens bigint NOT NULL DEFAULT 0,
    cache_read_tokens bigint NOT NULL DEFAULT 0,
    cache_write_tokens bigint NOT NULL DEFAULT 0,
    reasoning_tokens bigint NOT NULL DEFAULT 0,
    cost_usd_micros bigint NOT NULL DEFAULT 0,
    last_request_at timestamptz,
    PRIMARY KEY (organization_id, run_id),
    FOREIGN KEY (organization_id, run_id) REFERENCES workflow_runs (organization_id, id)
);

CREATE INDEX gateway_run_usage_by_cost ON gateway_run_usage (organization_id, cost_usd_micros DESC);

-- §7 price overrides (contracted rates). Bundled manifest prices apply when no
-- override is effective. Rates are USD micros per million tokens.
CREATE TABLE gateway_price_overrides (
    organization_id uuid NOT NULL REFERENCES identity_organizations (id),
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    provider text NOT NULL CHECK (provider ~ '^[a-z][a-z0-9-]{0,31}$'),
    model text NOT NULL CHECK (model ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'),
    input_micros_per_mtok bigint NOT NULL CHECK (input_micros_per_mtok >= 0),
    output_micros_per_mtok bigint NOT NULL CHECK (output_micros_per_mtok >= 0),
    cache_read_micros_per_mtok bigint NOT NULL CHECK (cache_read_micros_per_mtok >= 0),
    cache_write_micros_per_mtok bigint NOT NULL CHECK (cache_write_micros_per_mtok >= 0),
    effective_from timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by uuid NOT NULL REFERENCES identity_principals (id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, id)
);

CREATE INDEX gateway_price_overrides_lookup ON gateway_price_overrides (organization_id, provider, model, effective_from DESC);
