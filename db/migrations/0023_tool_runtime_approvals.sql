-- Operator-seeded, tenant-scoped runtime approvals. Old revisions remain for
-- audit after revocation; a recipe cannot create or edit these records.
CREATE TABLE workflow_tool_runtime_approvals (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES identity_organizations (id),
    harness text NOT NULL CHECK (harness IN ('codex','claude-code','opencode')),
    model text NOT NULL CHECK (model ~ '^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$'),
    effort text NOT NULL CHECK (effort ~ '^[a-z][a-z0-9_-]{0,31}$'),
    image text NOT NULL CHECK (image ~ '^.+@sha256:[0-9a-f]{64}$'),
    binary_path text NOT NULL CHECK (left(binary_path,1)='/' AND length(binary_path) BETWEEN 2 AND 512),
    binary_sha256 text NOT NULL CHECK (binary_sha256 ~ '^[0-9a-f]{64}$'),
    version text NOT NULL CHECK (version ~ '^[0-9]+\.[0-9]+\.[0-9]+$'),
    worker_pool text NOT NULL CHECK (length(worker_pool) BETWEEN 1 AND 128),
    max_timeout_seconds integer NOT NULL CHECK (max_timeout_seconds BETWEEN 1 AND 86400),
    max_output_bytes integer NOT NULL CHECK (max_output_bytes BETWEEN 1 AND 16777216),
    approved_by text NOT NULL CHECK (length(approved_by) BETWEEN 1 AND 160),
    approved_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    revoked_at timestamptz,
    CHECK (revoked_at IS NULL OR revoked_at >= approved_at)
);
CREATE UNIQUE INDEX workflow_tool_runtime_active_selection
    ON workflow_tool_runtime_approvals (organization_id,harness,model,effort)
    WHERE revoked_at IS NULL;

-- First proof uses an organization-owned workload grant selected explicitly
-- for each project/provider/model. Future user-supplied grants need a frozen
-- launch-principal/billing selection contract, not an implicit fallback.
CREATE TABLE workflow_project_model_grants (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL,
    project_id uuid NOT NULL,
    provider text NOT NULL CHECK (provider IN ('openai','anthropic')),
    model text NOT NULL CHECK (model ~ '^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$'),
    grant_id text NOT NULL CHECK (length(grant_id) BETWEEN 1 AND 160),
    grantee_id text NOT NULL CHECK (length(grantee_id) BETWEEN 1 AND 160),
    approved_by text NOT NULL CHECK (length(approved_by) BETWEEN 1 AND 160),
    approved_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    revoked_at timestamptz,
    FOREIGN KEY (organization_id,project_id) REFERENCES workflow_projects (organization_id,id),
    CHECK (revoked_at IS NULL OR revoked_at >= approved_at)
);
CREATE UNIQUE INDEX workflow_project_model_grant_active_selection
    ON workflow_project_model_grants (organization_id,project_id,provider,model)
    WHERE revoked_at IS NULL;
