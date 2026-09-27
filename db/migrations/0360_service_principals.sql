ALTER TABLE identity_principals ADD COLUMN kind text NOT NULL DEFAULT 'user' CHECK (kind IN ('user','service'));
ALTER TABLE identity_principals ADD CONSTRAINT service_no_login CHECK (kind<>'service' OR (password_hash IS NULL AND email IS NULL));
CREATE FUNCTION identity_principal_kind_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.kind<>OLD.kind THEN RAISE EXCEPTION 'principal kind is immutable'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER identity_principal_kind_immutable BEFORE UPDATE ON identity_principals FOR EACH ROW EXECUTE FUNCTION identity_principal_kind_immutable();
CREATE TABLE identity_service_principals (
 organization_id uuid NOT NULL,
 principal_id uuid NOT NULL,
 project_id uuid NOT NULL,
 label text NOT NULL CHECK (length(label) BETWEEN 1 AND 100),
 request_key text NOT NULL CHECK (length(request_key) BETWEEN 1 AND 128),
 created_by uuid NOT NULL REFERENCES identity_principals(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (organization_id,principal_id),
 UNIQUE (organization_id,project_id,created_by,request_key),
 FOREIGN KEY (organization_id,principal_id) REFERENCES identity_memberships(organization_id,principal_id),
 FOREIGN KEY (organization_id,project_id) REFERENCES workflow_projects(organization_id,id)
);
CREATE TRIGGER identity_service_principals_immutable BEFORE UPDATE OR DELETE ON identity_service_principals
 FOR EACH ROW EXECUTE FUNCTION workflow_events_append_only();
ALTER TABLE identity_service_principals ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity_service_principals FORCE ROW LEVEL SECURITY;
CREATE POLICY blaxsmith_tenant ON identity_service_principals
 USING (current_setting('blaxsmith.system',true)='on' OR organization_id::text=current_setting('blaxsmith.org_id',true))
 WITH CHECK (current_setting('blaxsmith.system',true)='on' OR organization_id::text=current_setting('blaxsmith.org_id',true));
ALTER TABLE identity_sessions DROP CONSTRAINT identity_sessions_auth_method_check;
ALTER TABLE identity_sessions ADD CONSTRAINT identity_sessions_auth_method_check CHECK (auth_method IN ('local','oidc','service'));
ALTER TABLE identity_sessions DROP CONSTRAINT identity_sessions_credential_kind_check;
ALTER TABLE identity_sessions ADD CONSTRAINT identity_sessions_credential_kind_check CHECK (credential_kind IN ('browser','api','service'));
ALTER TABLE identity_sessions ADD CONSTRAINT service_session_kind CHECK ((auth_method='service')=(credential_kind='service'));
CREATE FUNCTION identity_service_session_valid() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.credential_kind='service' AND NOT EXISTS (SELECT 1 FROM identity_service_principals sp JOIN identity_principals p ON p.id=sp.principal_id WHERE sp.organization_id=NEW.organization_id AND sp.principal_id=NEW.principal_id AND p.kind='service') THEN
  RAISE EXCEPTION 'service session requires a registered service identity';
 END IF;
 IF NEW.credential_kind<>'service' AND EXISTS (SELECT 1 FROM identity_principals WHERE id=NEW.principal_id AND kind='service') THEN
  RAISE EXCEPTION 'service identities cannot have human sessions';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER identity_service_session_valid BEFORE INSERT OR UPDATE ON identity_sessions FOR EACH ROW EXECUTE FUNCTION identity_service_session_valid();
