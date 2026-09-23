-- A run becomes dispatchable only after its complete, immutable recipe graph
-- and verification policy have been committed in the same transaction.
ALTER TABLE workflow_runs ADD COLUMN graph_sealed boolean NOT NULL DEFAULT false;

CREATE FUNCTION workflow_graph_cannot_unseal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.graph_sealed AND NOT NEW.graph_sealed THEN
        RAISE EXCEPTION 'sealed workflow graph cannot be reopened';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER workflow_run_graph_sealed BEFORE UPDATE ON workflow_runs
    FOR EACH ROW EXECUTE FUNCTION workflow_graph_cannot_unseal();

CREATE FUNCTION workflow_graph_open_for_insert() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE sealed boolean;
BEGIN
    SELECT graph_sealed INTO sealed FROM workflow_runs
        WHERE organization_id=NEW.organization_id AND id=NEW.run_id FOR UPDATE;
    IF sealed THEN
        RAISE EXCEPTION 'sealed workflow graph cannot accept new inputs';
    END IF;
    RETURN NEW;
END $$;

CREATE TABLE workflow_run_bundles (
    organization_id uuid NOT NULL,
    run_id uuid NOT NULL,
    bundle_json jsonb NOT NULL,
    verification_json jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, run_id),
    FOREIGN KEY (organization_id, run_id) REFERENCES workflow_runs (organization_id, id)
);

CREATE FUNCTION workflow_bundle_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'frozen workflow bundle cannot change';
END $$;
CREATE TRIGGER workflow_bundle_immutable BEFORE UPDATE OR DELETE ON workflow_run_bundles
    FOR EACH ROW EXECUTE FUNCTION workflow_bundle_append_only();
CREATE TRIGGER workflow_bundle_open BEFORE INSERT ON workflow_run_bundles
    FOR EACH ROW EXECUTE FUNCTION workflow_graph_open_for_insert();

CREATE TRIGGER workflow_task_open BEFORE INSERT ON workflow_tasks
    FOR EACH ROW EXECUTE FUNCTION workflow_graph_open_for_insert();

CREATE TABLE workflow_task_dependencies (
    organization_id uuid NOT NULL,
    run_id uuid NOT NULL,
    task_id uuid NOT NULL,
    depends_on_task_id uuid NOT NULL,
    PRIMARY KEY (organization_id, run_id, task_id, depends_on_task_id),
    FOREIGN KEY (organization_id, run_id, task_id)
        REFERENCES workflow_tasks (organization_id, run_id, id),
    FOREIGN KEY (organization_id, run_id, depends_on_task_id)
        REFERENCES workflow_tasks (organization_id, run_id, id),
    CHECK (task_id <> depends_on_task_id)
);

CREATE FUNCTION workflow_dependency_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'frozen task dependency cannot change';
END $$;
CREATE TRIGGER workflow_dependency_immutable BEFORE UPDATE OR DELETE ON workflow_task_dependencies
    FOR EACH ROW EXECUTE FUNCTION workflow_dependency_append_only();
CREATE TRIGGER workflow_dependency_open BEFORE INSERT ON workflow_task_dependencies
    FOR EACH ROW EXECUTE FUNCTION workflow_graph_open_for_insert();

ALTER TABLE workflow_events DROP CONSTRAINT workflow_events_kind_check;
ALTER TABLE workflow_events ADD CONSTRAINT workflow_events_kind_check
    CHECK (kind IN ('run.created','run.graph_sealed','run.succeeded','run.failed',
                   'run.cancel_requested','run.cancelled','task.created',
                   'attempt.reserved','attempt.started','attempt.unknown',
                   'attempt.result','attempt.stopped'));
