-- One public Git source per project. Runs pin a commit at admission, so edits
-- here affect only future runs.
CREATE TABLE workflow_project_sources (
    organization_id uuid NOT NULL,
    project_id uuid NOT NULL,
    repository_url text NOT NULL CHECK (length(repository_url) BETWEEN 1 AND 2048 AND repository_url LIKE 'https://%'),
    git_ref text NOT NULL DEFAULT '' CHECK (length(git_ref) <= 128),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, project_id),
    FOREIGN KEY (organization_id, project_id) REFERENCES workflow_projects (organization_id, id) ON DELETE CASCADE
);
