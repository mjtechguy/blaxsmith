-- Shared login limits must apply across API replicas before password hashing.
CREATE TABLE identity_login_limits (
    scope text NOT NULL CHECK (scope IN ('source', 'account_source')),
    key_hash bytea NOT NULL CHECK (octet_length(key_hash) = 32),
    window_start timestamptz NOT NULL,
    attempts integer NOT NULL CHECK (attempts > 0),
    PRIMARY KEY (scope, key_hash)
);

CREATE INDEX identity_login_limits_by_window ON identity_login_limits (window_start);
