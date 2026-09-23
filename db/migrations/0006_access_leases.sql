-- A lease records platform-authorized delivery, not provider-side token
-- revocation. An attempted bootstrap send with no delivered_at is unknown.
CREATE TABLE access_leases (
    organization_id text NOT NULL CHECK (organization_id <> ''),
    id text NOT NULL CHECK (id <> ''),
    binding_id text NOT NULL,
    connection_id text NOT NULL,
    bootstrap_challenge_id text NOT NULL UNIQUE,
    cluster_id text NOT NULL CHECK (cluster_id <> ''),
    attempt_id text NOT NULL CHECK (attempt_id <> ''),
    owner_generation bigint NOT NULL CHECK (owner_generation > 0),
    actor_uid text NOT NULL CHECK (actor_uid <> ''),
    capability text NOT NULL CHECK (capability <> ''),
    resource text NOT NULL CHECK (resource <> ''),
    audience text NOT NULL CHECK (audience <> ''),
    secret_version bigint,
    reserved_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL,
    delivery_attempted_at timestamptz,
    delivered_at timestamptz,
    revoked_at timestamptz,
    PRIMARY KEY (organization_id, id),
    FOREIGN KEY (organization_id, binding_id)
        REFERENCES access_bindings (organization_id, id),
    FOREIGN KEY (organization_id, connection_id)
        REFERENCES access_connections (organization_id, id),
    FOREIGN KEY (organization_id, connection_id, secret_version)
        REFERENCES access_secret_versions (organization_id, connection_id, version),
    FOREIGN KEY (bootstrap_challenge_id) REFERENCES bootstrap_challenges (id),
    CHECK (delivered_at IS NULL OR delivery_attempted_at IS NOT NULL)
);

CREATE INDEX access_leases_binding ON access_leases (organization_id, binding_id);
