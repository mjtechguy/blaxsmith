-- Evidence is immutable and attributable to one attempt and exact revision.
CREATE TABLE workflow_evidence (
 organization_id uuid NOT NULL,
 id uuid NOT NULL DEFAULT gen_random_uuid(),
 run_id uuid NOT NULL,
 task_id uuid NOT NULL,
 attempt_id uuid NOT NULL,
 kind text NOT NULL CHECK (kind IN ('artifact','gate','verification')),
 origin_key text NOT NULL CHECK (length(origin_key) BETWEEN 1 AND 64),
 revision text NOT NULL CHECK (revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
 policy_sha256 text NOT NULL CHECK (policy_sha256 ~ '^[0-9a-f]{64}$'),
 metadata jsonb NOT NULL CHECK (jsonb_typeof(metadata)='object' AND octet_length(metadata::text)<=65536),
 content bytea NOT NULL DEFAULT '',
 sha256 text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (organization_id,id),
 UNIQUE (organization_id,attempt_id,kind,origin_key),
 FOREIGN KEY (organization_id,run_id,task_id,attempt_id) REFERENCES workflow_attempts(organization_id,run_id,task_id,id),
 CHECK (octet_length(content)<=4194304)
);
CREATE INDEX workflow_evidence_run ON workflow_evidence(organization_id,run_id,id);
CREATE TRIGGER workflow_evidence_immutable BEFORE UPDATE OR DELETE ON workflow_evidence
 FOR EACH ROW EXECUTE FUNCTION workflow_review_append_only();
ALTER TABLE workflow_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_evidence FORCE ROW LEVEL SECURITY;
CREATE POLICY blaxsmith_tenant ON workflow_evidence
 USING (current_setting('blaxsmith.system',true)='on' OR organization_id::text=current_setting('blaxsmith.org_id',true))
 WITH CHECK (current_setting('blaxsmith.system',true)='on' OR organization_id::text=current_setting('blaxsmith.org_id',true));
-- An interrupted verifier is never resumed in a possibly contaminated guest.
ALTER TABLE workflow_attempts ADD COLUMN verification_started boolean NOT NULL DEFAULT false;

ALTER TABLE workflow_events DROP CONSTRAINT workflow_events_kind_check;
ALTER TABLE workflow_events ADD CONSTRAINT workflow_events_kind_check
    CHECK (kind IN ('run.created','run.graph_sealed','run.succeeded','run.failed',
                   'run.cancel_requested','run.cancelled','run.reopened','task.created',
                   'task.ready','task.correction','task.escalated','task.escalation_resolved',
                   'attempt.reserved','attempt.starting','attempt.started','attempt.unknown',
                   'attempt.runtime_bound','attempt.command_exited','attempt.progress',
                   'attempt.result','attempt.stopped','attempt.control',
                   'review.presented','review.superseded',
                   'review.approved','review.changes_requested',
                   'interaction.opened','interaction.answered','artifact.recorded','gate.recorded','verification.recorded'));
