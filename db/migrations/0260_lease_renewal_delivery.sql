-- Keep a renewal pending until its guest-file delivery succeeds. Retries reuse
-- the same generation and expiry, including after a dispatch leader restart.
ALTER TABLE access_leases
    ADD COLUMN renewal_generation bigint NOT NULL DEFAULT 0 CHECK (renewal_generation >= 0),
    ADD COLUMN renewal_pending boolean NOT NULL DEFAULT false;
