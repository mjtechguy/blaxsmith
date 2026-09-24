ALTER TABLE bootstrap_challenges
    ADD COLUMN phase text NOT NULL DEFAULT 'setup'
        CHECK (phase IN ('setup', 'model'));
