-- Private Git source: a project may reference one organization Git
-- connection (NULL = public). Each run freezes the connection it was
-- admitted with; dispatch binds git.read for the AX Workspace setup phase and
-- the platform pushes the run branch through the same connection's git.write.
ALTER TABLE workflow_project_sources ADD COLUMN git_connection_id text
    CHECK (git_connection_id IS NULL OR length(git_connection_id) BETWEEN 1 AND 160);
ALTER TABLE workflow_run_bundles ADD COLUMN git_connection_id text
    CHECK (git_connection_id IS NULL OR length(git_connection_id) BETWEEN 1 AND 160);
