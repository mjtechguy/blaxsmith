-- Connections hub: organization, project, and personal connections. Secrets stay in
-- access_secret_versions; nothing here holds credential material.
ALTER TABLE access_connections DROP CONSTRAINT access_connections_owner_kind_check;
ALTER TABLE access_connections ADD CONSTRAINT access_connections_owner_kind_check
    CHECK (owner_kind IN ('user', 'organization', 'project'));
ALTER TABLE access_connections
    ADD COLUMN label text CHECK (label IS NULL OR length(label) BETWEEN 1 AND 120),
    ADD COLUMN models_checked_at timestamptz,
    ADD COLUMN models_error text CHECK (models_error IS NULL OR length(models_error) <= 500);

ALTER TABLE workflow_project_model_grants DROP CONSTRAINT workflow_project_model_grants_provider_check;
ALTER TABLE workflow_project_model_grants ADD CONSTRAINT workflow_project_model_grants_provider_check
    CHECK (provider IN ('openai', 'anthropic', 'opencode', 'opencode-go'));

-- Provider model catalog per connection, refreshed from the provider's list
-- endpoint on create, daily, and on demand.
CREATE TABLE access_connection_models (
    organization_id text NOT NULL,
    connection_id text NOT NULL,
    model_id text NOT NULL CHECK (model_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'),
    display_name text NOT NULL CHECK (length(display_name) <= 200),
    released_at timestamptz,
    context_tokens integer CHECK (context_tokens IS NULL OR context_tokens > 0),
    capabilities jsonb,
    PRIMARY KEY (organization_id, connection_id, model_id),
    FOREIGN KEY (organization_id, connection_id) REFERENCES access_connections (organization_id, id)
);

-- The principal who launched a run. A personal (user-owned) model grant is
-- honoured only for runs whose initiator owns that connection.
ALTER TABLE workflow_runs ADD COLUMN initiator_principal_id text
    CHECK (initiator_principal_id IS NULL OR length(initiator_principal_id) BETWEEN 1 AND 160);

-- Project owner/admin assignments for the connections hub until project roles
-- exist; the project creator is recorded as owner. Organization resource
-- grants themselves are lane U's access_resource_grants.
CREATE TABLE workflow_project_admins (
    organization_id uuid NOT NULL,
    project_id uuid NOT NULL,
    principal_id text NOT NULL CHECK (length(principal_id) BETWEEN 1 AND 160),
    role text NOT NULL CHECK (role IN ('owner', 'admin')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, project_id, principal_id),
    FOREIGN KEY (organization_id, project_id) REFERENCES workflow_projects (organization_id, id)
);
