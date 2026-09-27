-- A run may execute an immutable plan version; NULL denotes goal planning.
ALTER TABLE workflow_goal_runs ADD COLUMN plan_version bigint;
ALTER TABLE workflow_goal_runs ADD CONSTRAINT workflow_goal_runs_plan
 FOREIGN KEY(organization_id,goal_id,plan_version) REFERENCES workflow_goal_plans(organization_id,goal_id,version);
