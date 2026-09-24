package workflow

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/identity"
)

var ErrAttemptControlDenied = errors.New("attempt control denied")

// AttemptTerminal is what the terminal gateway needs to reach and authorize
// one attempt. Atespace/Actor are the pinned AX readback, empty until bound.
type AttemptTerminal struct {
	OrganizationID, RunID, TaskID, AttemptID string
	Stage, State                             string
	Atespace, Actor                          string
	HolderPrincipalID, HolderSessionID       string
	ControlGeneration                        int64
}

// Human reports whether a person currently controls the running attempt.
func (a AttemptTerminal) Human() bool { return a.HolderPrincipalID != "" && a.State == "running" }

const attemptTerminalColumns = `a.run_id,a.task_id,t.task_key,a.state,COALESCE(r.ax_atespace,''),COALESCE(r.ax_task,''),
	COALESCE(a.control_holder_principal_id::text,''),COALESCE(a.control_holder_session_id::text,''),a.control_generation
	FROM workflow_attempts a JOIN workflow_tasks t ON t.organization_id=a.organization_id AND t.id=a.task_id
	LEFT JOIN workflow_attempt_runtime r ON r.organization_id=a.organization_id AND r.attempt_id=a.id
	WHERE a.organization_id=$1 AND a.id=$2`

func scanAttemptTerminal(row pgx.Row, orgID, attemptID string) (AttemptTerminal, error) {
	t := AttemptTerminal{OrganizationID: orgID, AttemptID: attemptID}
	err := row.Scan(&t.RunID, &t.TaskID, &t.Stage, &t.State, &t.Atespace, &t.Actor,
		&t.HolderPrincipalID, &t.HolderSessionID, &t.ControlGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return AttemptTerminal{}, ErrNotFound
	}
	return t, err
}

// GetAttemptTerminal is tenant-scoped; any live member may view.
func (s *Store) GetAttemptTerminal(ctx context.Context, orgID, attemptID string) (AttemptTerminal, error) {
	if !ids(orgID, attemptID) {
		return AttemptTerminal{}, ErrInvalid
	}
	return scanAttemptTerminal(s.pool.QueryRow(ctx, `SELECT `+attemptTerminalColumns, orgID, attemptID), orgID, attemptID)
}

// takeOverAllowed is the key-disclosure rule: a takeover shows the model API
// key in the session, so only organization owners/admins, or the principal who
// owns every personal model connection the attempt is bound to, may take over.
// The role must already be verified against the live membership.
const takeOverAllowed = `SELECT $4 IN ('owner','admin') OR (
	EXISTS (SELECT 1 FROM access_bindings b WHERE b.organization_id=$1::text AND b.attempt_id=$2::text
		AND b.capability='model.invoke')
	AND NOT EXISTS (SELECT 1 FROM access_bindings b
		JOIN access_grants g ON g.organization_id=b.organization_id AND g.id=b.grant_id
		JOIN access_connections c ON c.organization_id=g.organization_id AND c.id=g.connection_id
		WHERE b.organization_id=$1::text AND b.attempt_id=$2::text AND b.capability='model.invoke'
		AND NOT (c.owner_kind='user' AND c.owner_id=$3::text)))`

// CanTakeOverAttempt reports whether the caller's role and model connection
// ownership allow a takeover. It does not check that the attempt is running.
func (s *Store) CanTakeOverAttempt(ctx context.Context, caller identity.Caller, attemptID string) (bool, error) {
	if !ids(caller.OrganizationID, caller.PrincipalID, attemptID) ||
		(caller.Role != "owner" && caller.Role != "admin" && caller.Role != "member") {
		return false, nil
	}
	var ok bool
	err := s.pool.QueryRow(ctx, takeOverAllowed, caller.OrganizationID, attemptID, caller.PrincipalID, caller.Role).Scan(&ok)
	return ok, err
}

// TakeOverAttempt makes the caller's session the single control holder of a
// running attempt. changed is false when this session already holds it; any
// other holder is ErrConflict. The caller must hold run-control (not viewer)
// and pass the key-disclosure rule (takeOverAllowed).
func (s *Store) TakeOverAttempt(ctx context.Context, caller identity.Caller, attemptID string) (t AttemptTerminal, changed bool, err error) {
	if caller.Role != "owner" && caller.Role != "admin" && caller.Role != "member" {
		return AttemptTerminal{}, false, ErrAttemptControlDenied
	}
	tx, t, err := s.lockAttemptControl(ctx, caller, attemptID, true)
	if err != nil {
		return AttemptTerminal{}, false, err
	}
	defer tx.Rollback(ctx)
	var allowed bool
	if err := tx.QueryRow(ctx, takeOverAllowed, caller.OrganizationID, attemptID, caller.PrincipalID, caller.Role).Scan(&allowed); err != nil {
		return AttemptTerminal{}, false, err
	}
	if !allowed {
		return AttemptTerminal{}, false, ErrAttemptControlDenied
	}
	if t.State != "running" || t.Atespace == "" || t.Actor == "" {
		return AttemptTerminal{}, false, ErrConflict
	}
	if t.HolderPrincipalID != "" {
		if t.HolderSessionID == caller.SessionID {
			return t, false, tx.Commit(ctx)
		}
		return AttemptTerminal{}, false, ErrConflict
	}
	if err := setAttemptControl(ctx, tx, &t, caller.PrincipalID, caller.SessionID, "workflow.attempt.control_taken"); err != nil {
		return AttemptTerminal{}, false, err
	}
	return t, true, tx.Commit(ctx)
}

// ReleaseAttemptControl returns control to the agent. Only the holder
// principal may release, and only at the generation it observed.
func (s *Store) ReleaseAttemptControl(ctx context.Context, caller identity.Caller, attemptID string, generation int64) (AttemptTerminal, error) {
	tx, t, err := s.lockAttemptControl(ctx, caller, attemptID, false)
	if err != nil {
		return AttemptTerminal{}, err
	}
	defer tx.Rollback(ctx)
	if t.HolderPrincipalID != caller.PrincipalID {
		return AttemptTerminal{}, ErrAttemptControlDenied
	}
	if t.ControlGeneration != generation {
		return AttemptTerminal{}, ErrConflict
	}
	if err := setAttemptControl(ctx, tx, &t, "", "", "workflow.attempt.control_returned"); err != nil {
		return AttemptTerminal{}, err
	}
	return t, tx.Commit(ctx)
}

// lockAttemptControl locks run then attempt (the order other writers use)
// under a live-session check. control requires a run-control role.
func (s *Store) lockAttemptControl(ctx context.Context, caller identity.Caller, attemptID string, control bool) (pgx.Tx, AttemptTerminal, error) {
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID, attemptID) {
		return nil, AttemptTerminal{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, AttemptTerminal{}, err
	}
	fail := func(err error) (pgx.Tx, AttemptTerminal, error) {
		_ = tx.Rollback(ctx)
		return nil, AttemptTerminal{}, err
	}
	var role string
	err = tx.QueryRow(ctx, `SELECT m.role FROM identity_sessions s
		JOIN identity_memberships m ON m.organization_id=s.organization_id AND m.principal_id=s.principal_id
		JOIN identity_principals p ON p.id=s.principal_id
		JOIN identity_organizations o ON o.id=s.organization_id
		WHERE s.organization_id=$1 AND s.id=$2 AND s.principal_id=$3
		AND m.role=$4 AND (NOT $6 OR m.role IN ('owner','admin','member')) AND $5::timestamptz>clock_timestamp()
		AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
		AND m.state='active' AND p.state='active'
		AND (s.auth_method<>'local' OR o.login_policy IN ('local','mixed'))
		AND (o.mfa_policy<>'required' OR s.mfa_level='totp')
		FOR SHARE OF s,m,p,o`, caller.OrganizationID, caller.SessionID, caller.PrincipalID,
		caller.Role, caller.AccessExpires, control).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return fail(ErrAttemptControlDenied)
	}
	if err != nil {
		return fail(err)
	}
	var runID string
	err = tx.QueryRow(ctx, `SELECT r.id FROM workflow_attempts a JOIN workflow_runs r
		ON r.organization_id=a.organization_id AND r.id=a.run_id
		WHERE a.organization_id=$1 AND a.id=$2 FOR UPDATE OF r`, caller.OrganizationID, attemptID).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fail(ErrNotFound)
	}
	if err != nil {
		return fail(err)
	}
	t, err := scanAttemptTerminal(tx.QueryRow(ctx, `SELECT `+attemptTerminalColumns+` FOR UPDATE OF a`,
		caller.OrganizationID, attemptID), caller.OrganizationID, attemptID)
	if err != nil {
		return fail(err)
	}
	return tx, t, nil
}

func setAttemptControl(ctx context.Context, tx pgx.Tx, t *AttemptTerminal, principalID, sessionID, action string) error {
	var principal, session any
	if principalID != "" {
		principal, session = principalID, sessionID
	}
	tag, err := tx.Exec(ctx, `UPDATE workflow_attempts SET control_holder_principal_id=$3,
		control_holder_session_id=$4, control_generation=control_generation+1, control_changed_at=clock_timestamp()
		WHERE organization_id=$1 AND id=$2 AND control_generation=$5`,
		t.OrganizationID, t.AttemptID, principal, session, t.ControlGeneration)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	actor := principalID
	if actor == "" {
		actor = t.HolderPrincipalID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events
		(organization_id,actor_kind,actor_id,action,subject_id)
		VALUES ($1,'principal',$2,$3,$4)`, t.OrganizationID, actor, action, t.AttemptID); err != nil {
		return err
	}
	if err := event(ctx, tx, t.OrganizationID, t.RunID, t.TaskID, t.AttemptID, "attempt.control"); err != nil {
		return err
	}
	t.HolderPrincipalID, t.HolderSessionID = principalID, sessionID
	t.ControlGeneration++
	return nil
}
