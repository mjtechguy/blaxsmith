-- Both live limits and immutable policy history carry the same tunable fields.
ALTER TABLE workflow_goal_allowances
 ADD COLUMN max_repeated_check_failures integer NOT NULL DEFAULT 0 CHECK(max_repeated_check_failures BETWEEN 0 AND 100),
 ADD COLUMN no_progress_seconds integer NOT NULL DEFAULT 0 CHECK(no_progress_seconds=0 OR no_progress_seconds BETWEEN 60 AND 2592000),
 ADD COLUMN stall_reset_at timestamptz;
ALTER TABLE workflow_goal_allowance_history
 ADD COLUMN max_repeated_check_failures integer NOT NULL DEFAULT 0 CHECK(max_repeated_check_failures BETWEEN 0 AND 100),
 ADD COLUMN no_progress_seconds integer NOT NULL DEFAULT 0 CHECK(no_progress_seconds=0 OR no_progress_seconds BETWEEN 60 AND 2592000),
 ADD COLUMN stall_reset_at timestamptz;
