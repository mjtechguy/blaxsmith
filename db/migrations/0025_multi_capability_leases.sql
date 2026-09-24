ALTER TABLE access_leases
    DROP CONSTRAINT access_leases_bootstrap_challenge_id_key;

CREATE UNIQUE INDEX access_leases_challenge_capability_key
    ON access_leases (bootstrap_challenge_id, capability);
