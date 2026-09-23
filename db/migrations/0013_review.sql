-- Review is a separate human decision over a completed execution. Packages
-- snapshot the exact revision, evidence, and frozen run inputs being reviewed.
CREATE TABLE workflow_review_packages (
    organization_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    run_id uuid NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    source_commit text NOT NULL CHECK (source_commit ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    bundle_sha256 text NOT NULL CHECK (bundle_sha256 ~ '^[0-9a-f]{64}$'),
    verification_sha256 text NOT NULL CHECK (verification_sha256 ~ '^[0-9a-f]{64}$'),
    integrated_commit text NOT NULL CHECK (integrated_commit ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    evidence_sha256 text NOT NULL CHECK (evidence_sha256 ~ '^[0-9a-f]{64}$'),
    presented_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, run_id, id),
    UNIQUE (organization_id, run_id, revision),
    FOREIGN KEY (organization_id, run_id) REFERENCES workflow_runs (organization_id, id)
);

ALTER TABLE workflow_runs ADD COLUMN review_package_id uuid;
ALTER TABLE workflow_runs ADD CONSTRAINT workflow_runs_current_review
    FOREIGN KEY (organization_id, id, review_package_id)
    REFERENCES workflow_review_packages (organization_id, run_id, id);

-- A later package may supersede an approval; an old head may never be
-- restored and accidentally make that approval effective again.
CREATE FUNCTION workflow_review_head_forward() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    old_revision bigint;
    new_revision bigint;
BEGIN
    IF NEW.review_package_id IS NOT DISTINCT FROM OLD.review_package_id OR OLD.review_package_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT revision INTO old_revision FROM workflow_review_packages
        WHERE organization_id=OLD.organization_id AND run_id=OLD.id AND id=OLD.review_package_id;
    SELECT revision INTO new_revision FROM workflow_review_packages
        WHERE organization_id=NEW.organization_id AND run_id=NEW.id AND id=NEW.review_package_id;
    IF new_revision IS NULL OR new_revision <= old_revision THEN
        RAISE EXCEPTION 'current review package must advance';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER workflow_review_head_forward BEFORE UPDATE OF review_package_id ON workflow_runs
    FOR EACH ROW EXECUTE FUNCTION workflow_review_head_forward();

CREATE TABLE workflow_review_decisions (
    organization_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    run_id uuid NOT NULL,
    package_id uuid NOT NULL,
    idempotency_key text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 128),
    principal_id uuid NOT NULL REFERENCES identity_principals (id),
    session_id uuid NOT NULL,
    action text NOT NULL CHECK (action IN ('approve','request_changes')),
    decided_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, run_id, package_id),
    UNIQUE (organization_id, run_id, idempotency_key),
    FOREIGN KEY (organization_id, run_id, package_id)
        REFERENCES workflow_review_packages (organization_id, run_id, id)
);

CREATE FUNCTION workflow_review_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'workflow review records are append-only';
END $$;
CREATE TRIGGER workflow_review_package_immutable BEFORE UPDATE OR DELETE ON workflow_review_packages
    FOR EACH ROW EXECUTE FUNCTION workflow_review_append_only();
CREATE TRIGGER workflow_review_decision_immutable BEFORE UPDATE OR DELETE ON workflow_review_decisions
    FOR EACH ROW EXECUTE FUNCTION workflow_review_append_only();
