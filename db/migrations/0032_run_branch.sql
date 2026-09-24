-- Code flow between stages: an attempt freezes the commit it starts from
-- (NULL = the run's source commit), and the run records the last run-branch
-- tip the platform pushed, which the next push's force-with-lease expects.
ALTER TABLE workflow_attempts ADD COLUMN input_commit text
    CHECK (input_commit IS NULL OR input_commit ~ '^[0-9a-f]{40}([0-9a-f]{24})?$');
ALTER TABLE workflow_runs ADD COLUMN branch_tip text
    CHECK (branch_tip IS NULL OR branch_tip ~ '^[0-9a-f]{40}([0-9a-f]{24})?$');

CREATE OR REPLACE FUNCTION workflow_frozen_attempt() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.organization_id, NEW.run_id, NEW.task_id, NEW.generation, NEW.fence_token) IS DISTINCT FROM
       (OLD.organization_id, OLD.run_id, OLD.task_id, OLD.generation, OLD.fence_token) OR
       (OLD.input_commit IS NOT NULL AND NEW.input_commit IS DISTINCT FROM OLD.input_commit) THEN
        RAISE EXCEPTION 'frozen workflow attempt identity cannot change';
    END IF;
    RETURN NEW;
END $$;
