-- A reserved AX actor is still initializing until AX confirms Workspace setup.
ALTER TABLE workflow_tasks DROP CONSTRAINT workflow_tasks_state_check;
ALTER TABLE workflow_tasks ADD CONSTRAINT workflow_tasks_state_check
    CHECK (state IN ('pending','reserved','starting','running','reconciling','succeeded','blocked','cancelled'));
ALTER TABLE workflow_tasks DROP CONSTRAINT workflow_tasks_check;
ALTER TABLE workflow_tasks ADD CONSTRAINT workflow_tasks_check
    CHECK ((state IN ('reserved','starting','running','reconciling')) = (active_attempt_id IS NOT NULL));

ALTER TABLE workflow_attempts DROP CONSTRAINT workflow_attempts_state_check;
ALTER TABLE workflow_attempts ADD CONSTRAINT workflow_attempts_state_check
    CHECK (state IN ('reserved','starting','running','reconciling','succeeded','failed','stopped'));

ALTER TABLE workflow_events DROP CONSTRAINT workflow_events_kind_check;
ALTER TABLE workflow_events ADD CONSTRAINT workflow_events_kind_check
    CHECK (kind IN ('run.created','run.graph_sealed','run.succeeded','run.failed',
                   'run.cancel_requested','run.cancelled','task.created',
                   'attempt.reserved','attempt.starting','attempt.started','attempt.unknown',
                   'attempt.runtime_bound','attempt.command_exited',
                   'attempt.result','attempt.stopped','review.presented','review.superseded',
                   'review.approved','review.changes_requested'));
