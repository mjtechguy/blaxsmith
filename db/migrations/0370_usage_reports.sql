-- Untrusted harness counters, immutable and independent of acceptance evidence.
CREATE TABLE workflow_usage_reports (
 organization_id uuid NOT NULL,
 run_id uuid NOT NULL,
 task_id uuid NOT NULL,
 attempt_id uuid NOT NULL,
 report_key text NOT NULL CHECK (report_key ~ '^[a-zA-Z0-9_-]{1,64}$'),
 harness text NOT NULL CHECK (harness IN ('codex','claude-code','opencode')),
 input_tokens bigint NOT NULL CHECK (input_tokens BETWEEN 0 AND 1000000000000),
 output_tokens bigint NOT NULL CHECK (output_tokens BETWEEN 0 AND 1000000000000),
 cost_micros_usd bigint CHECK (cost_micros_usd BETWEEN 0 AND 1000000000000),
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (organization_id,attempt_id,report_key),
 FOREIGN KEY (organization_id,run_id,task_id,attempt_id) REFERENCES workflow_attempts(organization_id,run_id,task_id,id),
 CHECK (harness='opencode' OR report_key='session')
);
CREATE INDEX workflow_usage_run ON workflow_usage_reports(organization_id,run_id,attempt_id);
CREATE TRIGGER workflow_usage_immutable BEFORE UPDATE OR DELETE ON workflow_usage_reports
 FOR EACH ROW EXECUTE FUNCTION workflow_review_append_only();
ALTER TABLE workflow_usage_reports ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_usage_reports FORCE ROW LEVEL SECURITY;
CREATE POLICY blaxsmith_tenant ON workflow_usage_reports
 USING (current_setting('blaxsmith.system',true)='on' OR organization_id::text=current_setting('blaxsmith.org_id',true))
 WITH CHECK (current_setting('blaxsmith.system',true)='on' OR organization_id::text=current_setting('blaxsmith.org_id',true));
