-- New revisions retain the acceptance contract and never rewrite run evidence.
CREATE TABLE workflow_verification_history (
 organization_id uuid NOT NULL,
 project_id uuid NOT NULL,
 version bigint NOT NULL CHECK (version>0),
 policy_json jsonb NOT NULL,
 updated_at timestamptz NOT NULL,
 principal_id uuid,
 PRIMARY KEY (organization_id,project_id,version),
 FOREIGN KEY (organization_id,project_id) REFERENCES workflow_projects(organization_id,id),
 FOREIGN KEY (organization_id,principal_id) REFERENCES identity_memberships(organization_id,principal_id)
);
INSERT INTO workflow_verification_history (organization_id,project_id,version,policy_json,updated_at)
 SELECT organization_id,project_id,version,policy_json,updated_at FROM workflow_project_verification;
CREATE TRIGGER workflow_verification_history_immutable BEFORE UPDATE OR DELETE ON workflow_verification_history
 FOR EACH ROW EXECUTE FUNCTION workflow_events_append_only();
ALTER TABLE workflow_verification_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_verification_history FORCE ROW LEVEL SECURITY;
CREATE POLICY blaxsmith_tenant ON workflow_verification_history
 USING (current_setting('blaxsmith.system',true)='on' OR organization_id::text=current_setting('blaxsmith.org_id',true))
 WITH CHECK (current_setting('blaxsmith.system',true)='on' OR organization_id::text=current_setting('blaxsmith.org_id',true));
