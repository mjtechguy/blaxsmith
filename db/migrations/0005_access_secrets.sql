-- Ciphertext only. The 256-bit encryption keys and key IDs are configured
-- outside PostgreSQL; old keys remain available while retained rows need them.
ALTER TABLE access_connections
    ADD COLUMN active_secret_version bigint CHECK (active_secret_version > 0);

CREATE TABLE access_secret_versions (
    organization_id text NOT NULL CHECK (organization_id <> ''),
    connection_id text NOT NULL,
    version bigint NOT NULL CHECK (version > 0),
    key_id text NOT NULL CHECK (key_id <> ''),
    algorithm text NOT NULL CHECK (algorithm = 'AES-256-GCM'),
    nonce bytea NOT NULL CHECK (octet_length(nonce) = 12),
    ciphertext bytea NOT NULL CHECK (octet_length(ciphertext) BETWEEN 17 AND 16400),
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, connection_id, version),
    FOREIGN KEY (organization_id, connection_id)
        REFERENCES access_connections (organization_id, id)
);
