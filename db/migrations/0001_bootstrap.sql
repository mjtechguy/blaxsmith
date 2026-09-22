-- The scheduler must update bootstrap_owners in the same durable authority
-- domain as execution ownership. These tables confer no access by themselves.
CREATE TABLE bootstrap_owners (
    cluster_id text NOT NULL CHECK (cluster_id <> ''),
    attempt_id text NOT NULL CHECK (attempt_id <> ''),
    owner_generation bigint NOT NULL CHECK (owner_generation > 0),
    actor_atespace text NOT NULL CHECK (actor_atespace <> ''),
    actor_name text NOT NULL CHECK (actor_name <> ''),
    actor_uid text NOT NULL CHECK (actor_uid <> ''),
    active boolean NOT NULL,
    PRIMARY KEY (cluster_id, attempt_id)
);

CREATE TABLE bootstrap_challenges (
    id text PRIMARY KEY,
    cluster_id text NOT NULL,
    attempt_id text NOT NULL,
    owner_generation bigint NOT NULL CHECK (owner_generation > 0),
    actor_atespace text NOT NULL,
    actor_name text NOT NULL,
    actor_uid text NOT NULL,
    nonce_sha256 bytea NOT NULL UNIQUE CHECK (octet_length(nonce_sha256) = 32),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL,
    cancelled_at timestamptz,
    consumed_at timestamptz,
    FOREIGN KEY (cluster_id, attempt_id) REFERENCES bootstrap_owners (cluster_id, attempt_id),
    CHECK (cancelled_at IS NULL OR consumed_at IS NULL)
);

CREATE UNIQUE INDEX bootstrap_one_pending_per_attempt
    ON bootstrap_challenges (cluster_id, attempt_id)
    WHERE cancelled_at IS NULL AND consumed_at IS NULL;
