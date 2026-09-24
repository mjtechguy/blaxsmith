-- Multi-stage flow: accepted results, frozen handoffs, bounded corrections,
-- and loop/human escalations. Corrections grant extra attempts without
-- changing a task's frozen retry budget.
ALTER TABLE workflow_tasks ADD COLUMN extra_attempts integer NOT NULL DEFAULT 0
    CHECK (extra_attempts BETWEEN 0 AND 19);
ALTER TABLE workflow_tasks DROP CONSTRAINT workflow_tasks_state_check;
ALTER TABLE workflow_tasks ADD CONSTRAINT workflow_tasks_state_check
    CHECK (state IN ('pending','reserved','starting','running','reconciling','succeeded','blocked','cancelled','escalated'));

-- Guest-reported, untrusted handoff text. Only the current owner's clean exit
-- can record it, and it takes effect only after actor stop is proved.
CREATE TABLE workflow_attempt_results (
    organization_id uuid NOT NULL,
    attempt_id uuid NOT NULL,
    run_id uuid NOT NULL,
    task_id uuid NOT NULL,
    summary text NOT NULL CHECK (octet_length(summary) <= 16384),
    revision text CHECK (revision IS NULL OR revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    verdict text NOT NULL CHECK (verdict IN ('','pass','fail')),
    result_sha256 text NOT NULL CHECK (result_sha256 ~ '^[0-9a-f]{64}$'),
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, attempt_id),
    FOREIGN KEY (organization_id, run_id, task_id, attempt_id)
        REFERENCES workflow_attempts (organization_id, run_id, task_id, id)
);

-- The exact handoff text frozen into an attempt's prompt.
CREATE TABLE workflow_attempt_handoffs (
    organization_id uuid NOT NULL,
    attempt_id uuid NOT NULL,
    run_id uuid NOT NULL,
    task_id uuid NOT NULL,
    handoff text NOT NULL CHECK (octet_length(handoff) <= 131072),
    handoff_sha256 text NOT NULL CHECK (handoff_sha256 ~ '^[0-9a-f]{64}$'),
    PRIMARY KEY (organization_id, attempt_id),
    FOREIGN KEY (organization_id, run_id, task_id, attempt_id)
        REFERENCES workflow_attempts (organization_id, run_id, task_id, id)
);

CREATE TABLE workflow_corrections (
    organization_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    run_id uuid NOT NULL,
    task_id uuid NOT NULL,
    source_stage text NOT NULL CHECK (source_stage ~ '^[a-z][a-z0-9_-]{0,63}$'),
    decision_id uuid,
    message text NOT NULL CHECK (octet_length(message) <= 16384),
    consumed_attempt_id uuid,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, decision_id, task_id),
    FOREIGN KEY (organization_id, run_id, task_id) REFERENCES workflow_tasks (organization_id, run_id, id)
);
CREATE INDEX workflow_corrections_by_run ON workflow_corrections (organization_id, run_id, source_stage);

-- An escalation outbox: raised_at is set once the interaction sink accepted it.
CREATE TABLE workflow_escalations (
    organization_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    run_id uuid NOT NULL,
    stage_key text NOT NULL CHECK (stage_key ~ '^[a-z][a-z0-9_-]{0,63}$'),
    escalation_key text NOT NULL CHECK (escalation_key ~ '^[0-9A-Za-z_-]{1,64}$'),
    task_id uuid,
    decision_id uuid,
    findings text NOT NULL CHECK (octet_length(findings) <= 16384),
    granted_cycles integer NOT NULL DEFAULT 0 CHECK (granted_cycles BETWEEN 0 AND 10),
    resolution text CHECK (resolution IN ('raise_cap','accept_with_exceptions','halt')),
    raised_at timestamptz,
    resolved_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, run_id, escalation_key),
    FOREIGN KEY (organization_id, run_id) REFERENCES workflow_runs (organization_id, id)
);
CREATE INDEX workflow_escalations_unraised ON workflow_escalations (organization_id, id) WHERE raised_at IS NULL;

-- 0030 and 0031 extend this list; 0031 holds the full union.
ALTER TABLE workflow_events DROP CONSTRAINT workflow_events_kind_check;
ALTER TABLE workflow_events ADD CONSTRAINT workflow_events_kind_check
    CHECK (kind IN ('run.created','run.graph_sealed','run.succeeded','run.failed',
                   'run.cancel_requested','run.cancelled','run.reopened','task.created',
                   'task.ready','task.correction','task.escalated','task.escalation_resolved',
                   'attempt.reserved','attempt.starting','attempt.started','attempt.unknown',
                   'attempt.runtime_bound','attempt.command_exited','attempt.progress',
                   'attempt.result','attempt.stopped','review.presented','review.superseded',
                   'review.approved','review.changes_requested'));
