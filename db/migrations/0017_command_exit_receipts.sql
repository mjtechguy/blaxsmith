-- AX status is workload state, not the supervised command's exit result.
-- Pin the runtime identity before accepting a platform-signed exit receipt.
CREATE TABLE workflow_attempt_runtime (
    organization_id uuid NOT NULL,
    run_id uuid NOT NULL,
    task_id uuid NOT NULL,
    attempt_id uuid NOT NULL,
    ax_atespace text NOT NULL CHECK (ax_atespace <> ''),
    ax_task text NOT NULL CHECK (ax_task <> ''),
    actor_uid text NOT NULL CHECK (actor_uid <> ''),
    template_uid text NOT NULL CHECK (template_uid <> ''),
    image text NOT NULL CHECK (image ~ '@sha256:[0-9a-f]{64}$'),
    worker_pool text NOT NULL CHECK (worker_pool <> ''),
    command_sha256 text NOT NULL CHECK (command_sha256 ~ '^[0-9a-f]{64}$'),
    bound_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, attempt_id),
    FOREIGN KEY (organization_id, run_id, task_id, attempt_id)
        REFERENCES workflow_attempts (organization_id, run_id, task_id, id)
);

CREATE TABLE workflow_command_exits (
    organization_id uuid NOT NULL,
    run_id uuid NOT NULL,
    task_id uuid NOT NULL,
    attempt_id uuid NOT NULL,
    signer_id text NOT NULL CHECK (length(signer_id) BETWEEN 1 AND 128),
    report_json jsonb NOT NULL,
    report_sha256 text NOT NULL CHECK (report_sha256 ~ '^[0-9a-f]{64}$'),
    signature bytea NOT NULL CHECK (octet_length(signature) = 64),
    exit_code integer NOT NULL CHECK (exit_code BETWEEN -1 AND 255),
    evidence_sha256 text NOT NULL CHECK (evidence_sha256 ~ '^[0-9a-f]{64}$'),
    received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, attempt_id),
    FOREIGN KEY (organization_id, attempt_id)
        REFERENCES workflow_attempt_runtime (organization_id, attempt_id),
    FOREIGN KEY (organization_id, run_id, task_id, attempt_id)
        REFERENCES workflow_attempts (organization_id, run_id, task_id, id)
);

CREATE FUNCTION workflow_receipt_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'workflow runtime and command-exit receipts are immutable';
END $$;
CREATE TRIGGER workflow_runtime_immutable BEFORE UPDATE OR DELETE ON workflow_attempt_runtime
    FOR EACH ROW EXECUTE FUNCTION workflow_receipt_append_only();
CREATE TRIGGER workflow_command_exit_immutable BEFORE UPDATE OR DELETE ON workflow_command_exits
    FOR EACH ROW EXECUTE FUNCTION workflow_receipt_append_only();

ALTER TABLE workflow_events DROP CONSTRAINT workflow_events_kind_check;
ALTER TABLE workflow_events ADD CONSTRAINT workflow_events_kind_check
    CHECK (kind IN ('run.created','run.graph_sealed','run.succeeded','run.failed',
                   'run.cancel_requested','run.cancelled','task.created',
                   'attempt.reserved','attempt.started','attempt.unknown',
                   'attempt.runtime_bound','attempt.command_exited',
                   'attempt.result','attempt.stopped','review.presented','review.superseded',
                   'review.approved','review.changes_requested'));
