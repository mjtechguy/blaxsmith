-- Whitelisted server sorting keeps paging bounded within each tenant/project.
CREATE INDEX workflow_projects_name ON workflow_projects (organization_id, name COLLATE "C", id);
CREATE INDEX workflow_runs_launch_key ON workflow_runs (organization_id, project_id, launch_key COLLATE "C", id);
CREATE INDEX workflow_runs_state ON workflow_runs (organization_id, project_id, state, id);
