-- Interactive takeover: at most one human controls an attempt's terminal.
-- control_generation advances on every holder change and fences stale
-- handbacks and reconnects. Terminal output is never stored.
ALTER TABLE workflow_attempts
    ADD COLUMN control_holder_principal_id uuid,
    ADD COLUMN control_holder_session_id uuid,
    ADD COLUMN control_generation bigint NOT NULL DEFAULT 0 CHECK (control_generation >= 0),
    ADD COLUMN control_changed_at timestamptz,
    ADD CONSTRAINT workflow_attempts_control_holder
        CHECK ((control_holder_principal_id IS NULL) = (control_holder_session_id IS NULL));

-- 0029's kinds plus attempt.control; 0031 holds the full union.
ALTER TABLE workflow_events DROP CONSTRAINT workflow_events_kind_check;
ALTER TABLE workflow_events ADD CONSTRAINT workflow_events_kind_check
    CHECK (kind IN ('run.created','run.graph_sealed','run.succeeded','run.failed',
                   'run.cancel_requested','run.cancelled','run.reopened','task.created',
                   'task.ready','task.correction','task.escalated','task.escalation_resolved',
                   'attempt.reserved','attempt.starting','attempt.started','attempt.unknown',
                   'attempt.runtime_bound','attempt.command_exited','attempt.progress',
                   'attempt.result','attempt.stopped','attempt.control',
                   'review.presented','review.superseded',
                   'review.approved','review.changes_requested'));
