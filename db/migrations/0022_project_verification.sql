-- Project administrators own check commands. Each run freezes the selected
-- version; repository recipes can require check IDs but cannot define commands.
CREATE TABLE workflow_project_verification (
    organization_id uuid NOT NULL,
    project_id uuid NOT NULL,
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    policy_json jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, project_id),
    FOREIGN KEY (organization_id, project_id)
        REFERENCES workflow_projects (organization_id, id)
);
