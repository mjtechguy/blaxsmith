ALTER TABLE bootstrap_challenges
    ADD COLUMN release_attempted_at timestamptz,
    ADD COLUMN released_at timestamptz,
    ADD CONSTRAINT bootstrap_release_order
        CHECK (released_at IS NULL OR release_attempted_at IS NOT NULL);
