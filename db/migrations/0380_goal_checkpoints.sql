-- A checkpoint preserves an accepted run contract. It never certifies individual
-- requirements or marks a later plan version complete.
CREATE TABLE workflow_goal_checkpoints (
 organization_id uuid NOT NULL,
 goal_id uuid NOT NULL,
 id uuid NOT NULL DEFAULT gen_random_uuid(),
 sequence bigint NOT NULL CHECK(sequence>0),
 run_id uuid NOT NULL,
 package_id uuid NOT NULL,
 plan_version bigint NOT NULL CHECK(plan_version>0),
 goal_revision bigint NOT NULL CHECK(goal_revision>0),
 expected_goal_revision bigint NOT NULL CHECK(expected_goal_revision>0),
 candidate_revision text NOT NULL CHECK(candidate_revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
 title text NOT NULL CHECK(length(title) BETWEEN 1 AND 300),
 request_key text NOT NULL CHECK(length(request_key) BETWEEN 1 AND 128),
 principal_id uuid NOT NULL REFERENCES identity_principals(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(organization_id,id),
 UNIQUE(organization_id,goal_id,sequence),
 UNIQUE(organization_id,goal_id,principal_id,request_key),
 UNIQUE(organization_id,goal_id,package_id),
 FOREIGN KEY(organization_id,goal_id) REFERENCES workflow_goals(organization_id,id),
 FOREIGN KEY(organization_id,run_id) REFERENCES workflow_goal_runs(organization_id,run_id),
 FOREIGN KEY(organization_id,goal_id,plan_version) REFERENCES workflow_goal_plans(organization_id,goal_id,version),
 FOREIGN KEY(organization_id,package_id) REFERENCES workflow_review_packages(organization_id,id)
);
CREATE TRIGGER workflow_goal_checkpoints_immutable BEFORE UPDATE OR DELETE ON workflow_goal_checkpoints
 FOR EACH ROW EXECUTE FUNCTION workflow_review_append_only();
ALTER TABLE workflow_goal_checkpoints ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_goal_checkpoints FORCE ROW LEVEL SECURITY;
CREATE POLICY blaxsmith_tenant ON workflow_goal_checkpoints
 USING (current_setting('blaxsmith.system',true)='on' OR organization_id::text=current_setting('blaxsmith.org_id',true))
 WITH CHECK (current_setting('blaxsmith.system',true)='on' OR organization_id::text=current_setting('blaxsmith.org_id',true));
