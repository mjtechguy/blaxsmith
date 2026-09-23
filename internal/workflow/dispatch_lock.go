package workflow

import "context"

// WithAttemptDispatchLock serializes external AX operations for one attempt
// across scheduler processes. A transaction-scoped PostgreSQL advisory lock
// is released even when a process loses its database connection.
func (s *Store) WithAttemptDispatchLock(ctx context.Context, a Attempt, fn func(context.Context) error) error {
	if !validAttempt(a) || fn == nil {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, a.ID); err != nil {
		return err
	}
	return fn(ctx)
}
