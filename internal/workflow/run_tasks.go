package workflow

import "context"

// RunTask is the durable stage state shown to a person inspecting a run.
type RunTask struct {
	ID              string
	Key             string
	State           string
	Generation      int64
	MaxAttempts     int32
	ActiveAttemptID *string
	DependsOn       []string
}

// ListRunTasks returns only the selected organization's run graph. The caller
// must authorize the run before using this read model.
func (s *Store) ListRunTasks(ctx context.Context, orgID, runID string) ([]RunTask, error) {
	if !ids(orgID, runID) {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT t.id,t.task_key,t.state,t.generation,t.max_attempts,t.active_attempt_id::text,
		ARRAY(SELECT parent.task_key FROM workflow_task_dependencies d
		JOIN workflow_tasks parent ON parent.organization_id=d.organization_id AND parent.id=d.depends_on_task_id
		WHERE d.organization_id=t.organization_id AND d.run_id=t.run_id AND d.task_id=t.id ORDER BY parent.task_key)
		FROM workflow_tasks t WHERE t.organization_id=$1 AND t.run_id=$2 ORDER BY t.created_at,t.id`, orgID, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := []RunTask{}
	for rows.Next() {
		var task RunTask
		if err := rows.Scan(&task.ID, &task.Key, &task.State, &task.Generation, &task.MaxAttempts,
			&task.ActiveAttemptID, &task.DependsOn); err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}
