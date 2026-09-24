-- Email is the local sign-in identifier. It is stored lowercased and trimmed,
-- unique across the installation regardless of case, and starts unverified
-- (there is no SMTP; a future OIDC link trusts only a verified email claim).
-- The username stays as an internal handle for code and audit joins.
ALTER TABLE identity_principals
    ADD COLUMN email text CHECK (email IS NULL OR (email = lower(btrim(email))
        AND length(email) BETWEEN 3 AND 254 AND email ~ '^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$')),
    ADD COLUMN email_verified boolean NOT NULL DEFAULT false,
    -- A principal without an email may sign in with its username exactly once;
    -- that session must set an email before it can do anything else.
    ADD COLUMN legacy_login_at timestamptz;

CREATE UNIQUE INDEX identity_principals_email ON identity_principals (lower(email));

UPDATE identity_principals SET email = username
    WHERE username ~ '^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$' AND length(username) <= 254;

ALTER TABLE identity_sessions
    ADD COLUMN email_required boolean NOT NULL DEFAULT false,
    ADD COLUMN source_address text CHECK (length(source_address) <= 64),
    ADD COLUMN user_agent text CHECK (length(user_agent) <= 256),
    ADD COLUMN last_seen_at timestamptz;

-- Re-authentication (current-password checks on account changes) shares the
-- login limiter under its own scope.
ALTER TABLE identity_login_limits DROP CONSTRAINT identity_login_limits_scope_check;
ALTER TABLE identity_login_limits ADD CONSTRAINT identity_login_limits_scope_check
    CHECK (scope IN ('source', 'account_source', 'account', 'reauth'));

-- How a principal is shown to people: display name, then email, then the
-- internal handle for accounts that have neither.
CREATE FUNCTION identity_principal_label(display_name text, email text, username text) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    RETURN COALESCE(NULLIF(display_name, ''), email, username);
