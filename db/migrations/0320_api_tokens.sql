-- Delegated machine credentials use dedicated live sessions, so the existing
-- transactional session fences also fence revocation of API mutations.
ALTER TABLE identity_sessions ADD COLUMN credential_kind text NOT NULL DEFAULT 'browser'
 CHECK (credential_kind IN ('browser','api'));
CREATE TABLE identity_api_tokens (
 organization_id uuid NOT NULL,
 session_id uuid NOT NULL,
 project_id uuid NOT NULL,
 principal_id uuid NOT NULL,
 token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash)=32),
 label text NOT NULL CHECK (length(label) BETWEEN 1 AND 100),
 scopes jsonb NOT NULL CHECK (jsonb_typeof(scopes)='array'),
 role text NOT NULL CHECK (role IN ('owner','admin','member','viewer')),
 PRIMARY KEY (organization_id,session_id),
 FOREIGN KEY (organization_id,session_id) REFERENCES identity_sessions(organization_id,id),
 FOREIGN KEY (organization_id,principal_id) REFERENCES identity_memberships(organization_id,principal_id),
 FOREIGN KEY (organization_id,project_id) REFERENCES workflow_projects(organization_id,id)
);
CREATE TRIGGER identity_api_tokens_immutable BEFORE UPDATE OR DELETE ON identity_api_tokens
 FOR EACH ROW EXECUTE FUNCTION workflow_events_append_only();
ALTER TABLE identity_api_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity_api_tokens FORCE ROW LEVEL SECURITY;
CREATE POLICY blaxsmith_tenant ON identity_api_tokens
 USING (current_setting('blaxsmith.system',true)='on' OR organization_id::text=current_setting('blaxsmith.org_id',true))
 WITH CHECK (current_setting('blaxsmith.system',true)='on' OR organization_id::text=current_setting('blaxsmith.org_id',true));
