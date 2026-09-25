-- Envelope encryption for access secrets. Each organization gets its own
-- random 256-bit data-encryption key (DEK); only its wrapped form is stored,
-- sealed by an app master key (configured outside PostgreSQL) with AAD that
-- binds organization, DEK version and master key ID. Rotating the master key
-- re-wraps these rows; secret ciphertext is untouched.
CREATE TABLE access_organization_keys (
    organization_id text NOT NULL CHECK (organization_id <> ''),
    version integer NOT NULL CHECK (version > 0),
    master_key_id text NOT NULL CHECK (master_key_id <> ''),
    algorithm text NOT NULL CHECK (algorithm = 'AES-256-GCM'),
    nonce bytea NOT NULL CHECK (octet_length(nonce) = 12),
    wrapped_key bytea NOT NULL CHECK (octet_length(wrapped_key) = 48),
    state text NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'retired')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    rewrapped_at timestamptz,
    PRIMARY KEY (organization_id, version)
);

CREATE UNIQUE INDEX access_organization_keys_one_active
    ON access_organization_keys (organization_id) WHERE state = 'active';

-- NULL: legacy row sealed directly by the master key named in key_id.
-- Set: sealed by this organization's DEK of that version.
ALTER TABLE access_secret_versions
    ADD COLUMN data_key_version integer,
    ADD FOREIGN KEY (organization_id, data_key_version)
        REFERENCES access_organization_keys (organization_id, version);

CREATE INDEX access_secret_versions_legacy
    ON access_secret_versions (organization_id, connection_id, version)
    WHERE data_key_version IS NULL;
