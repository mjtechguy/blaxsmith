-- Policy acceptance belongs to the immutable evidence package. It is never
-- represented as a human decision or granted the authority to merge/deploy.
ALTER TABLE workflow_review_packages ADD COLUMN acceptance_mode text NOT NULL DEFAULT 'manual'
    CHECK (acceptance_mode IN ('manual', 'policy'));
