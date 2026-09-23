-- Browser transport is added only after cookie, CSRF, and login throttling gates.
CREATE TABLE identity_sessions (
    organization_id uuid NOT NULL,
    id uuid NOT NULL,
    principal_id uuid NOT NULL,
    auth_method text NOT NULL CHECK (auth_method IN ('local', 'oidc')),
    mfa_level text NOT NULL CHECK (mfa_level IN ('none', 'totp')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    PRIMARY KEY (organization_id, id),
    FOREIGN KEY (organization_id, principal_id)
        REFERENCES identity_memberships (organization_id, principal_id)
);

CREATE INDEX identity_sessions_by_principal
    ON identity_sessions (organization_id, principal_id, expires_at DESC);

CREATE TABLE identity_refresh_tokens (
    token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    organization_id uuid NOT NULL,
    session_id uuid NOT NULL,
    issued_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    consumed_at timestamptz,
    FOREIGN KEY (organization_id, session_id)
        REFERENCES identity_sessions (organization_id, id)
);

CREATE INDEX identity_refresh_by_session
    ON identity_refresh_tokens (organization_id, session_id, issued_at DESC);
