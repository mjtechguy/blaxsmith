CREATE INDEX workflow_running_attempt_cursor ON workflow_attempts (organization_id, id)
    WHERE state = 'running';
