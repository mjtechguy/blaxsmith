-- Structured human interactions (questions, approvals, escalations, interview
-- rounds) and steering messages. Progress rides the durable workflow_events log.
ALTER TABLE workflow_events ADD COLUMN payload jsonb
    CHECK (payload IS NULL OR (jsonb_typeof(payload)='object' AND octet_length(payload::text) <= 8192));
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
                   'interaction.opened','interaction.answered'));

-- A guest interaction belongs to one attempt and carries the guest's own id.
-- A platform interaction (escalation) belongs to a stage with no live guest.
CREATE TABLE workflow_interactions (
    organization_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    run_id uuid NOT NULL,
    task_id uuid NOT NULL,
    attempt_id uuid,
    origin text NOT NULL CHECK (origin IN ('guest','platform')),
    origin_key text NOT NULL CHECK (origin_key ~ '^[0-9A-Za-z_-]{1,64}$'),
    kind text NOT NULL CHECK (kind IN ('question','approval','escalation','interview_round')),
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload)='object' AND octet_length(payload::text) <= 65536),
    state text NOT NULL DEFAULT 'open' CHECK (state IN ('open','answered','cancelled','superseded')),
    answer jsonb CHECK (answer IS NULL OR jsonb_typeof(answer)='object'),
    answered_by uuid REFERENCES identity_principals (id),
    answered_session_id uuid,
    answered_at timestamptz,
    delivered_at timestamptz,
    closed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, id),
    FOREIGN KEY (organization_id, run_id, task_id) REFERENCES workflow_tasks (organization_id, run_id, id),
    FOREIGN KEY (organization_id, run_id, task_id, attempt_id)
        REFERENCES workflow_attempts (organization_id, run_id, task_id, id),
    CHECK ((origin='guest') = (attempt_id IS NOT NULL)),
    CHECK ((state='answered') = (answer IS NOT NULL AND answered_by IS NOT NULL AND answered_at IS NOT NULL)),
    CHECK ((state='open') = (closed_at IS NULL)),
    CHECK (delivered_at IS NULL OR state='answered')
);
CREATE UNIQUE INDEX workflow_interactions_guest_key ON workflow_interactions (organization_id, attempt_id, origin_key)
    WHERE origin='guest';
CREATE UNIQUE INDEX workflow_interactions_platform_key ON workflow_interactions (organization_id, run_id, task_id, origin_key)
    WHERE origin='platform';
CREATE INDEX workflow_interactions_by_run ON workflow_interactions (organization_id, run_id, created_at, id);
CREATE INDEX workflow_interactions_undelivered ON workflow_interactions (organization_id, attempt_id)
    WHERE state='answered' AND delivered_at IS NULL;

-- The first answer is final; only delivery and closure may change afterwards.
CREATE FUNCTION workflow_interaction_answer_final() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.organization_id, NEW.run_id, NEW.task_id, NEW.attempt_id, NEW.origin, NEW.origin_key,
        NEW.kind, NEW.payload, NEW.created_at) IS DISTINCT FROM
       (OLD.organization_id, OLD.run_id, OLD.task_id, OLD.attempt_id, OLD.origin, OLD.origin_key,
        OLD.kind, OLD.payload, OLD.created_at) THEN
        RAISE EXCEPTION 'workflow interaction identity cannot change';
    END IF;
    IF OLD.state <> 'open' AND (NEW.state, NEW.answer, NEW.answered_by, NEW.answered_session_id, NEW.answered_at)
        IS DISTINCT FROM (OLD.state, OLD.answer, OLD.answered_by, OLD.answered_session_id, OLD.answered_at) THEN
        RAISE EXCEPTION 'closed workflow interaction cannot change';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER workflow_interaction_final BEFORE UPDATE ON workflow_interactions
    FOR EACH ROW EXECUTE FUNCTION workflow_interaction_answer_final();
CREATE TRIGGER workflow_interaction_no_delete BEFORE DELETE ON workflow_interactions
    FOR EACH ROW EXECUTE FUNCTION workflow_events_append_only();

-- Steering messages persist before delivery and are redelivered until the
-- guest acknowledges them; the guest dedupes on id.
CREATE TABLE workflow_attempt_steers (
    organization_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    run_id uuid NOT NULL,
    task_id uuid NOT NULL,
    attempt_id uuid NOT NULL,
    body jsonb NOT NULL CHECK (jsonb_typeof(body)='object' AND octet_length(body::text) <= 8192),
    principal_id uuid NOT NULL REFERENCES identity_principals (id),
    session_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    delivered_at timestamptz,
    PRIMARY KEY (organization_id, id),
    FOREIGN KEY (organization_id, run_id, task_id, attempt_id)
        REFERENCES workflow_attempts (organization_id, run_id, task_id, id)
);
CREATE INDEX workflow_attempt_steers_undelivered ON workflow_attempt_steers (organization_id, attempt_id, created_at)
    WHERE delivered_at IS NULL;

-- The last guest watch sequence persisted for an attempt. Lines at or below
-- it are replays and are skipped.
CREATE TABLE workflow_interaction_cursors (
    organization_id uuid NOT NULL,
    attempt_id uuid NOT NULL,
    run_id uuid NOT NULL,
    task_id uuid NOT NULL,
    seq bigint NOT NULL DEFAULT 0 CHECK (seq >= 0),
    PRIMARY KEY (organization_id, attempt_id),
    FOREIGN KEY (organization_id, run_id, task_id, attempt_id)
        REFERENCES workflow_attempts (organization_id, run_id, task_id, id)
);
