-- Model gateway G3 (docs/model-gateway-plan.md §8, §9, §14 item 3): soft
-- budgets and threshold alerts. Budgets are monthly (UTC calendar month)
-- amounts in estimated USD. Alerts never block anything.

-- §15.1 "Budgets & alerts" switch, kept apart from gateway_org_settings so it
-- can change without touching the gateway master settings. Off by default.
CREATE TABLE gateway_budget_settings (
    organization_id uuid PRIMARY KEY REFERENCES identity_organizations (id),
    enabled boolean NOT NULL DEFAULT false,
    updated_by uuid REFERENCES identity_principals (id),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE gateway_budgets (
    organization_id uuid NOT NULL REFERENCES identity_organizations (id),
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    scope text NOT NULL CHECK (scope IN ('organization', 'project', 'user')),
    project_id uuid,
    principal_id uuid REFERENCES identity_principals (id),
    amount_usd_micros bigint NOT NULL CHECK (amount_usd_micros > 0 AND amount_usd_micros <= 100000000000000),
    -- Percent thresholds, ascending; default 50/80/100.
    thresholds integer[] NOT NULL DEFAULT '{50,80,100}'
        CHECK (cardinality(thresholds) BETWEEN 1 AND 6 AND 1 <= ALL (thresholds) AND 200 >= ALL (thresholds)),
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    created_by uuid NOT NULL REFERENCES identity_principals (id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    archived_at timestamptz,
    PRIMARY KEY (organization_id, id),
    FOREIGN KEY (organization_id, project_id) REFERENCES workflow_projects (organization_id, id),
    CHECK ((scope = 'project') = (project_id IS NOT NULL)),
    CHECK ((scope = 'user') = (principal_id IS NOT NULL))
);

-- One active budget per organization, project or user.
CREATE UNIQUE INDEX gateway_budgets_one_active ON gateway_budgets
    (organization_id, scope, COALESCE(project_id, principal_id, organization_id)) WHERE archived_at IS NULL;

-- One alert per budget, period and threshold: the unique key makes delivery
-- exactly-once however many rollup ticks evaluate the same crossing. Scope,
-- project and principal are copied from the budget for recipient filtering.
CREATE TABLE gateway_alerts (
    organization_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    budget_id uuid NOT NULL,
    period_start date NOT NULL,
    threshold_pct integer NOT NULL CHECK (threshold_pct BETWEEN 1 AND 200),
    scope text NOT NULL CHECK (scope IN ('organization', 'project', 'user')),
    project_id uuid,
    principal_id uuid,
    spend_usd_micros bigint NOT NULL CHECK (spend_usd_micros >= 0),
    amount_usd_micros bigint NOT NULL CHECK (amount_usd_micros > 0),
    forecast_usd_micros bigint NOT NULL CHECK (forecast_usd_micros >= 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    acknowledged_at timestamptz,
    acknowledged_by uuid REFERENCES identity_principals (id),
    snoozed_until timestamptz,
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, budget_id, period_start, threshold_pct),
    FOREIGN KEY (organization_id, budget_id) REFERENCES gateway_budgets (organization_id, id),
    CHECK ((acknowledged_at IS NULL) = (acknowledged_by IS NULL))
);

CREATE INDEX gateway_alerts_open ON gateway_alerts (organization_id, created_at DESC) WHERE acknowledged_at IS NULL;
