-- Admin-managed local accounts. Setup and reset links are stored only as a
-- SHA-256 digest; the raw token is shown once to the issuing admin. Audit
-- detail carries non-secret context such as a role change's from/to.
ALTER TABLE identity_principals ADD COLUMN display_name text NOT NULL DEFAULT ''
    CHECK (length(display_name) <= 160);
ALTER TABLE identity_audit_events ADD COLUMN detail jsonb;

CREATE TABLE identity_account_links (
    token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    organization_id uuid NOT NULL,
    principal_id uuid NOT NULL,
    purpose text NOT NULL CHECK (purpose IN ('setup', 'reset')),
    issued_by uuid NOT NULL REFERENCES identity_principals (id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    revoked_at timestamptz,
    FOREIGN KEY (organization_id, principal_id)
        REFERENCES identity_memberships (organization_id, principal_id),
    CHECK (expires_at > created_at)
);

-- At most one usable link per member; issuing a new one revokes the old.
CREATE UNIQUE INDEX identity_account_links_open ON identity_account_links (organization_id, principal_id)
    WHERE consumed_at IS NULL AND revoked_at IS NULL;

CREATE INDEX identity_sessions_last_login ON identity_sessions (organization_id, principal_id, created_at DESC);
