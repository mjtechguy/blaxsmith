-- Pending sign-ins (GitHub OAuth state and PKCE verifier, Codex device codes)
-- are shared by every replica. The key is a SHA-256 of the browser-held
-- handle, the row is bound to the session that started the flow, and the
-- payload is AES-GCM ciphertext from the secret store (never plaintext).
CREATE TABLE access_pending_sign_ins (
    key_hash bytea PRIMARY KEY CHECK (octet_length(key_hash) = 32),
    kind text NOT NULL CHECK (kind IN ('github_oauth', 'codex_device')),
    organization_id text NOT NULL,
    principal_id text NOT NULL,
    session_id text NOT NULL,
    key_id text NOT NULL,
    nonce bytea NOT NULL CHECK (octet_length(nonce) = 12),
    ciphertext bytea NOT NULL,
    claimed_until timestamptz,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX access_pending_sign_ins_by_expiry ON access_pending_sign_ins (expires_at);
CREATE INDEX access_pending_sign_ins_by_principal ON access_pending_sign_ins (principal_id);

-- Public account-link endpoints share the login limiter: per source address
-- and per link.
ALTER TABLE identity_login_limits DROP CONSTRAINT identity_login_limits_scope_check;
ALTER TABLE identity_login_limits ADD CONSTRAINT identity_login_limits_scope_check
    CHECK (scope IN ('source', 'account_source', 'account', 'reauth', 'link_source', 'link'));
