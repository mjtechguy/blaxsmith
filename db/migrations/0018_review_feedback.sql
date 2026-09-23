-- Correction requests carry actionable feedback in the immutable decision.
-- Legacy requests had no feedback; enforce this for new rows without inventing
-- words on behalf of past reviewers.
ALTER TABLE workflow_review_decisions ADD COLUMN feedback text NOT NULL DEFAULT '';
ALTER TABLE workflow_review_decisions ADD CONSTRAINT workflow_review_feedback_check
    CHECK ((action='approve' AND feedback='') OR
           (action='request_changes' AND length(btrim(feedback)) BETWEEN 10 AND 4000)) NOT VALID;
