-- Admin dashboard: audit events carry the project their subject belongs to,
-- resolved once at insert so the audit table filters by project with an index.
ALTER TABLE identity_audit_events ADD COLUMN project_id uuid;

CREATE FUNCTION identity_audit_project(org uuid, subject uuid) RETURNS uuid LANGUAGE sql STABLE AS $$
    SELECT COALESCE(
        (SELECT p.id FROM workflow_projects p WHERE p.organization_id=org AND p.id=subject),
        (SELECT r.project_id FROM workflow_runs r WHERE r.organization_id=org AND r.id=subject),
        (SELECT r.project_id FROM workflow_attempts a JOIN workflow_runs r
            ON r.organization_id=a.organization_id AND r.id=a.run_id WHERE a.organization_id=org AND a.id=subject),
        (SELECT r.project_id FROM workflow_interactions i JOIN workflow_runs r
            ON r.organization_id=i.organization_id AND r.id=i.run_id WHERE i.organization_id=org AND i.id=subject),
        (SELECT m.project_id FROM workflow_project_model_grants m WHERE m.organization_id=org AND m.id=subject),
        (SELECT g.project_id::uuid FROM access_grants g WHERE g.organization_id=org::text AND g.id=subject::text
            AND g.project_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'))
$$;

CREATE FUNCTION identity_audit_set_project() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.project_id IS NULL AND NEW.subject_id IS NOT NULL THEN
        NEW.project_id := identity_audit_project(NEW.organization_id, NEW.subject_id);
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER identity_audit_project BEFORE INSERT ON identity_audit_events
    FOR EACH ROW EXECUTE FUNCTION identity_audit_set_project();

UPDATE identity_audit_events SET project_id=identity_audit_project(organization_id, subject_id)
    WHERE subject_id IS NOT NULL;

CREATE INDEX identity_audit_by_action ON identity_audit_events (organization_id, action, id DESC);
CREATE INDEX identity_audit_by_actor ON identity_audit_events (organization_id, actor_id, id DESC);
CREATE INDEX identity_audit_by_project ON identity_audit_events (organization_id, project_id, id DESC);

-- Bounded operator reads: current owners, open interactions, recent runs,
-- latest attempt activity, and per-connection grant and lease health.
CREATE INDEX workflow_attempts_live ON workflow_attempts (organization_id, created_at)
    WHERE state IN ('reserved','starting','running','reconciling');
CREATE INDEX workflow_interactions_open ON workflow_interactions (organization_id, created_at, id)
    WHERE state='open';
CREATE INDEX workflow_runs_org_recent ON workflow_runs (organization_id, created_at DESC);
CREATE INDEX workflow_events_by_attempt ON workflow_events (organization_id, attempt_id, id DESC)
    WHERE attempt_id IS NOT NULL;
CREATE INDEX access_grants_by_connection ON access_grants (organization_id, connection_id)
    WHERE revoked_at IS NULL;
CREATE INDEX access_leases_by_connection ON access_leases (organization_id, connection_id, expires_at);
