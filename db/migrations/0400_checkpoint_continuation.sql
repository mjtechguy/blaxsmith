-- A continuation records lineage without changing the earlier acceptance contract.
ALTER TABLE workflow_goal_checkpoints ADD UNIQUE(organization_id,goal_id,id);
ALTER TABLE workflow_goal_runs ADD COLUMN checkpoint_id uuid;
ALTER TABLE workflow_goal_runs ADD CONSTRAINT workflow_goal_runs_checkpoint
 FOREIGN KEY(organization_id,goal_id,checkpoint_id) REFERENCES workflow_goal_checkpoints(organization_id,goal_id,id);
ALTER TABLE workflow_run_bundles ADD COLUMN configured_git_ref text;
