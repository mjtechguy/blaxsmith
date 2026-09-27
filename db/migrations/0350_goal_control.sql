ALTER TABLE workflow_goals
 ADD COLUMN control_state text NOT NULL DEFAULT 'active' CHECK (control_state IN ('active','paused','cancel_requested')),
 ADD COLUMN control_version bigint NOT NULL DEFAULT 0 CHECK (control_version>=0);
CREATE TABLE workflow_goal_control_history (
 organization_id uuid NOT NULL,
 goal_id uuid NOT NULL,
 version bigint NOT NULL CHECK (version>0),
 expected_version bigint NOT NULL CHECK (expected_version>=0),
 action text NOT NULL CHECK (action IN ('pause','resume','cancel')),
 request_key text NOT NULL CHECK (length(request_key) BETWEEN 1 AND 128),
 principal_id uuid NOT NULL REFERENCES identity_principals(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (organization_id,goal_id,version),
 UNIQUE (organization_id,goal_id,principal_id,request_key),
 FOREIGN KEY (organization_id,goal_id) REFERENCES workflow_goals(organization_id,id)
);
CREATE TRIGGER workflow_goal_control_history_immutable BEFORE UPDATE OR DELETE ON workflow_goal_control_history
 FOR EACH ROW EXECUTE FUNCTION workflow_events_append_only();
ALTER TABLE workflow_goal_control_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_goal_control_history FORCE ROW LEVEL SECURITY;
CREATE POLICY blaxsmith_tenant ON workflow_goal_control_history
 USING (current_setting('blaxsmith.system',true)='on' OR organization_id::text=current_setting('blaxsmith.org_id',true))
 WITH CHECK (current_setting('blaxsmith.system',true)='on' OR organization_id::text=current_setting('blaxsmith.org_id',true));
