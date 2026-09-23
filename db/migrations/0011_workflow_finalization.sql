-- Execution completion is distinct from the final human approval decision.
ALTER TABLE workflow_runs DROP CONSTRAINT workflow_runs_state_check;
ALTER TABLE workflow_runs ADD CONSTRAINT workflow_runs_state_check
    CHECK (state IN ('queued','active','succeeded','failed','cancel_requested','cancelled'));

ALTER TABLE workflow_events DROP CONSTRAINT workflow_events_kind_check;
ALTER TABLE workflow_events ADD CONSTRAINT workflow_events_kind_check
    CHECK (kind IN ('run.created','run.succeeded','run.failed','run.cancel_requested','run.cancelled',
                   'task.created','attempt.reserved','attempt.started','attempt.unknown',
                   'attempt.result','attempt.stopped'));
