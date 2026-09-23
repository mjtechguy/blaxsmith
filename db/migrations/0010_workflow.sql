-- Initial durable workflow contract. Authorization is enforced by the caller;
-- every workflow lookup still carries its organization key.
CREATE TABLE workflow_projects (
    organization_id uuid NOT NULL REFERENCES identity_organizations (id),
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    slug text NOT NULL CHECK (slug ~ '^[a-z][a-z0-9-]{2,63}$'),
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 160),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, slug)
);

CREATE TABLE workflow_runs (
    organization_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    project_id uuid NOT NULL,
    launch_key text NOT NULL CHECK (length(launch_key) BETWEEN 1 AND 128),
    source_commit text NOT NULL CHECK (source_commit ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    bundle_sha256 text NOT NULL CHECK (bundle_sha256 ~ '^[0-9a-f]{64}$'),
    verification_sha256 text NOT NULL CHECK (verification_sha256 ~ '^[0-9a-f]{64}$'),
    state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','active','cancel_requested','cancelled')),
    event_seq bigint NOT NULL DEFAULT 0 CHECK (event_seq >= 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, project_id, launch_key),
    FOREIGN KEY (organization_id, project_id) REFERENCES workflow_projects (organization_id, id)
);

-- Immutable source and acceptance inputs cannot change even through ad hoc SQL.
CREATE FUNCTION workflow_frozen_run() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.organization_id, NEW.project_id, NEW.launch_key, NEW.source_commit,
        NEW.bundle_sha256, NEW.verification_sha256) IS DISTINCT FROM
       (OLD.organization_id, OLD.project_id, OLD.launch_key, OLD.source_commit,
        OLD.bundle_sha256, OLD.verification_sha256) THEN
        RAISE EXCEPTION 'frozen workflow run inputs cannot change';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER workflow_run_frozen BEFORE UPDATE ON workflow_runs
    FOR EACH ROW EXECUTE FUNCTION workflow_frozen_run();

CREATE TABLE workflow_tasks (
    organization_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    run_id uuid NOT NULL,
    task_key text NOT NULL CHECK (task_key ~ '^[a-z][a-z0-9_-]{0,63}$'),
    input_sha256 text NOT NULL CHECK (input_sha256 ~ '^[0-9a-f]{64}$'),
    max_attempts integer NOT NULL CHECK (max_attempts BETWEEN 1 AND 20),
    generation bigint NOT NULL DEFAULT 0 CHECK (generation >= 0),
    active_attempt_id uuid,
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','reserved','running','reconciling','succeeded','blocked','cancelled')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, run_id, id),
    UNIQUE (organization_id, run_id, task_key),
    FOREIGN KEY (organization_id, run_id) REFERENCES workflow_runs (organization_id, id),
    CHECK ((state IN ('reserved','running','reconciling')) = (active_attempt_id IS NOT NULL))
);

CREATE FUNCTION workflow_frozen_task() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.organization_id, NEW.run_id, NEW.task_key, NEW.input_sha256, NEW.max_attempts) IS DISTINCT FROM
       (OLD.organization_id, OLD.run_id, OLD.task_key, OLD.input_sha256, OLD.max_attempts) THEN
        RAISE EXCEPTION 'frozen workflow task inputs cannot change';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER workflow_task_frozen BEFORE UPDATE ON workflow_tasks
    FOR EACH ROW EXECUTE FUNCTION workflow_frozen_task();

CREATE TABLE workflow_attempts (
    organization_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    run_id uuid NOT NULL,
    task_id uuid NOT NULL,
    generation bigint NOT NULL CHECK (generation BETWEEN 1 AND 20),
    fence_token uuid NOT NULL DEFAULT gen_random_uuid(),
    state text NOT NULL CHECK (state IN ('reserved','running','reconciling','succeeded','failed','stopped')),
    result_sha256 text CHECK (result_sha256 IS NULL OR result_sha256 ~ '^[0-9a-f]{64}$'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    finished_at timestamptz,
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, run_id, task_id, id),
    UNIQUE (organization_id, task_id, generation),
    UNIQUE (fence_token),
    FOREIGN KEY (organization_id, run_id, task_id) REFERENCES workflow_tasks (organization_id, run_id, id)
);

CREATE INDEX workflow_attempts_by_run ON workflow_attempts (organization_id, run_id, created_at);

ALTER TABLE workflow_tasks ADD CONSTRAINT workflow_tasks_active_attempt
    FOREIGN KEY (organization_id, run_id, id, active_attempt_id)
    REFERENCES workflow_attempts (organization_id, run_id, task_id, id)
    DEFERRABLE INITIALLY DEFERRED;

CREATE FUNCTION workflow_frozen_attempt() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.organization_id, NEW.run_id, NEW.task_id, NEW.generation, NEW.fence_token) IS DISTINCT FROM
       (OLD.organization_id, OLD.run_id, OLD.task_id, OLD.generation, OLD.fence_token) THEN
        RAISE EXCEPTION 'frozen workflow attempt identity cannot change';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER workflow_attempt_frozen BEFORE UPDATE ON workflow_attempts
    FOR EACH ROW EXECUTE FUNCTION workflow_frozen_attempt();

CREATE TABLE workflow_events (
    id bigint NOT NULL CHECK (id > 0),
    organization_id uuid NOT NULL,
    run_id uuid NOT NULL,
    task_id uuid,
    attempt_id uuid,
    kind text NOT NULL CHECK (kind IN ('run.created','run.cancel_requested','run.cancelled','task.created','attempt.reserved','attempt.started','attempt.unknown','attempt.result','attempt.stopped')),
    occurred_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, run_id, id),
    CHECK (attempt_id IS NULL OR task_id IS NOT NULL),
    FOREIGN KEY (organization_id, run_id) REFERENCES workflow_runs (organization_id, id),
    FOREIGN KEY (organization_id, run_id, task_id) REFERENCES workflow_tasks (organization_id, run_id, id),
    FOREIGN KEY (organization_id, run_id, task_id, attempt_id)
        REFERENCES workflow_attempts (organization_id, run_id, task_id, id)
);

CREATE FUNCTION workflow_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'workflow events are append-only';
END $$;
CREATE TRIGGER workflow_event_immutable BEFORE UPDATE OR DELETE ON workflow_events
    FOR EACH ROW EXECUTE FUNCTION workflow_events_append_only();
