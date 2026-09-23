-- Organization-scoped, newest-first workspace lists use keyset cursors.
CREATE INDEX workflow_projects_recent ON workflow_projects (organization_id, created_at DESC, id DESC);
CREATE INDEX workflow_runs_recent ON workflow_runs (organization_id, project_id, created_at DESC, id DESC);
