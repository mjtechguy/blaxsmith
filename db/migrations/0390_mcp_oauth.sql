ALTER TABLE identity_api_tokens ADD COLUMN resource text NOT NULL DEFAULT '';
ALTER TABLE identity_login_limits DROP CONSTRAINT identity_login_limits_scope_check;
ALTER TABLE identity_login_limits ADD CONSTRAINT identity_login_limits_scope_check
 CHECK (scope IN ('source','account_source','account','reauth','link_source','link','oauth_source'));
CREATE TABLE identity_oauth_codes (
 organization_id uuid NOT NULL,
 code_hash bytea PRIMARY KEY CHECK (octet_length(code_hash)=32),
 principal_id uuid NOT NULL,
 browser_session_id uuid NOT NULL,
 project_id uuid NOT NULL,
 role text NOT NULL CHECK (role IN ('owner','admin','member','viewer')),
 access_expires_at timestamptz NOT NULL,
 client_id text NOT NULL CHECK (length(client_id) BETWEEN 1 AND 128),
 client_name text NOT NULL CHECK (length(client_name) BETWEEN 1 AND 80),
 redirect_uri text NOT NULL CHECK (length(redirect_uri) BETWEEN 1 AND 2048),
 resource text NOT NULL,
 challenge text NOT NULL CHECK (length(challenge)=43),
 scopes jsonb NOT NULL CHECK (jsonb_typeof(scopes)='array'),
 expires_at timestamptz NOT NULL DEFAULT clock_timestamp()+interval '2 minutes',
 issued_session_id uuid,
 FOREIGN KEY (organization_id,browser_session_id) REFERENCES identity_sessions(organization_id,id),
 FOREIGN KEY (organization_id,issued_session_id) REFERENCES identity_sessions(organization_id,id),
 FOREIGN KEY (organization_id,principal_id) REFERENCES identity_memberships(organization_id,principal_id),
 FOREIGN KEY (organization_id,project_id) REFERENCES workflow_projects(organization_id,id)
);
CREATE INDEX identity_oauth_codes_owner ON identity_oauth_codes(organization_id,principal_id,project_id,expires_at);
ALTER TABLE identity_oauth_codes ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity_oauth_codes FORCE ROW LEVEL SECURITY;
CREATE POLICY blaxsmith_tenant ON identity_oauth_codes
 USING (current_setting('blaxsmith.system',true)='on' OR organization_id::text=current_setting('blaxsmith.org_id',true))
 WITH CHECK (current_setting('blaxsmith.system',true)='on' OR organization_id::text=current_setting('blaxsmith.org_id',true));
