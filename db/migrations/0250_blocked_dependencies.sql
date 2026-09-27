-- Failed dependencies produce a durable, tenant-scoped explanation.
ALTER TABLE workflow_events DROP CONSTRAINT workflow_events_kind_check;
ALTER TABLE workflow_events ADD CONSTRAINT workflow_events_kind_check
    CHECK (kind IN ('run.created','run.graph_sealed','run.succeeded','run.failed',
                   'run.cancel_requested','run.cancelled','run.reopened','task.created',
                   'task.ready','task.blocked','task.correction','task.escalated','task.escalation_resolved',
                   'attempt.reserved','attempt.starting','attempt.started','attempt.unknown',
                   'attempt.runtime_bound','attempt.command_exited','attempt.progress',
                   'attempt.result','attempt.stopped','attempt.control',
                   'review.presented','review.superseded',
                   'review.approved','review.changes_requested',
                   'interaction.opened','interaction.answered','artifact.recorded','gate.recorded','verification.recorded'));
