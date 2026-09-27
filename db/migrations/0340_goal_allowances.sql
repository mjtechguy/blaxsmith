-- Human-set admission limits shared by all factory runs attached to a goal.
CREATE TABLE workflow_goal_allowances (
 organization_id uuid NOT NULL,
 goal_id uuid NOT NULL,
 version bigint NOT NULL CHECK(version>0),
 max_runs integer NOT NULL CHECK(max_runs BETWEEN 0 AND 10000),
 max_attempts integer NOT NULL CHECK(max_attempts BETWEEN 0 AND 100000),
 admit_until timestamptz,
 principal_id uuid NOT NULL REFERENCES identity_principals(id),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(organization_id,goal_id),
 FOREIGN KEY(organization_id,goal_id) REFERENCES workflow_goals(organization_id,id)
);
CREATE TABLE workflow_goal_allowance_history (LIKE workflow_goal_allowances INCLUDING DEFAULTS INCLUDING CONSTRAINTS);
ALTER TABLE workflow_goal_allowance_history ADD PRIMARY KEY(organization_id,goal_id,version);
ALTER TABLE workflow_goal_allowance_history ADD FOREIGN KEY(organization_id,goal_id) REFERENCES workflow_goals(organization_id,id);
ALTER TABLE workflow_goal_allowance_history ADD FOREIGN KEY(principal_id) REFERENCES identity_principals(id);
CREATE TRIGGER workflow_goal_allowance_history_immutable BEFORE UPDATE OR DELETE ON workflow_goal_allowance_history FOR EACH ROW EXECUTE FUNCTION workflow_events_append_only();
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['workflow_goal_allowances','workflow_goal_allowance_history'] LOOP
  EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format($p$CREATE POLICY blaxsmith_tenant ON %I
   USING (current_setting('blaxsmith.system',true)='on' OR organization_id::text=current_setting('blaxsmith.org_id',true))
   WITH CHECK (current_setting('blaxsmith.system',true)='on' OR organization_id::text=current_setting('blaxsmith.org_id',true))$p$,t);
 END LOOP;
END $$;
