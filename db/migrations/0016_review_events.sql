-- Execution and list events precede these review events.
ALTER TABLE workflow_events DROP CONSTRAINT workflow_events_kind_check;
ALTER TABLE workflow_events ADD CONSTRAINT workflow_events_kind_check
    CHECK (kind IN ('run.created','run.graph_sealed','run.succeeded','run.failed',
                   'run.cancel_requested','run.cancelled','task.created',
                   'attempt.reserved','attempt.started','attempt.unknown',
                   'attempt.result','attempt.stopped','review.presented','review.superseded',
                   'review.approved','review.changes_requested'));
