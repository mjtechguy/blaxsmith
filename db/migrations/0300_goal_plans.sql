-- Run association and artifact versions are platform records, not factory authority.
CREATE TABLE workflow_goal_runs (
 organization_id uuid NOT NULL,
 goal_id uuid NOT NULL,
 run_id uuid NOT NULL,
 goal_revision bigint NOT NULL CHECK (goal_revision>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(organization_id,run_id),
 FOREIGN KEY(organization_id,goal_id) REFERENCES workflow_goals(organization_id,id),
 FOREIGN KEY(organization_id,run_id) REFERENCES workflow_runs(organization_id,id)
);
CREATE INDEX workflow_goal_runs_goal ON workflow_goal_runs(organization_id,goal_id,created_at DESC);
CREATE TABLE workflow_goal_plans (
 organization_id uuid NOT NULL,
 goal_id uuid NOT NULL,
 version bigint NOT NULL CHECK(version>0),
 goal_revision bigint NOT NULL CHECK(goal_revision>0),
 request_key text NOT NULL CHECK(length(request_key) BETWEEN 1 AND 128),
 content bytea NOT NULL CHECK(octet_length(content) BETWEEN 1 AND 262144),
 sha256 text NOT NULL CHECK(sha256 ~ '^[0-9a-f]{64}$'),
 run_id uuid,
 evidence_id uuid,
 principal_id uuid NOT NULL REFERENCES identity_principals(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(organization_id,goal_id,version),
 UNIQUE(organization_id,goal_id,principal_id,request_key),
 FOREIGN KEY(organization_id,goal_id) REFERENCES workflow_goals(organization_id,id),
 FOREIGN KEY(organization_id,run_id) REFERENCES workflow_goal_runs(organization_id,run_id),
 FOREIGN KEY(organization_id,evidence_id) REFERENCES workflow_evidence(organization_id,id),
 CHECK((run_id IS NULL)=(evidence_id IS NULL))
);
CREATE TRIGGER workflow_goal_runs_immutable BEFORE UPDATE OR DELETE ON workflow_goal_runs FOR EACH ROW EXECUTE FUNCTION workflow_events_append_only();
CREATE TRIGGER workflow_goal_plans_immutable BEFORE UPDATE OR DELETE ON workflow_goal_plans FOR EACH ROW EXECUTE FUNCTION workflow_events_append_only();
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['workflow_goal_runs','workflow_goal_plans'] LOOP
  EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format($p$CREATE POLICY blaxsmith_tenant ON %I
   USING(current_setting('blaxsmith.system',true)='on' OR organization_id::text=current_setting('blaxsmith.org_id',true))
   WITH CHECK(current_setting('blaxsmith.system',true)='on' OR organization_id::text=current_setting('blaxsmith.org_id',true))$p$,t);
 END LOOP;
END $$;
