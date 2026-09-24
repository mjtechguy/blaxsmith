package workflow

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/identity"
)

// ErrAdminDenied means the caller is not an organization owner or admin.
var ErrAdminDenied = errors.New("organization administration denied")

// Admin read models. Every query is scoped to the caller's organization and
// bounded; none selects secret material.
type AdminLiveAttempt struct {
	AttemptID, RunID, ProjectID, ProjectName, LaunchKey, Stage, Kind, Harness, Model, State string
	ControllerID, ControllerUsername                                                        string
	StartedAt                                                                               time.Time
	LastActivity                                                                            *time.Time
}

type AdminInteraction struct {
	ID, RunID, ProjectID, ProjectName, LaunchKey, Stage, Kind, Title string
	Blocking                                                         bool
	CreatedAt                                                        time.Time
}

type AdminCapacity struct{ InFlight, Running, TakenOver int32 }

type AdminConnection struct {
	ID, ProviderKind, Host, Account, OwnerKind, State string
	ActiveGrants, ActiveLeases, ExpiringLeases        int32
	LastUsed                                          *time.Time
	CreatedAt                                         time.Time
}

type AdminGrant struct {
	ID, ConnectionID, ProjectID, ProjectName, Capability, Resource string
	ExpiresAt                                                      *time.Time
	CreatedAt                                                      time.Time
}

type AdminOverview struct {
	LiveAttempts     []AdminLiveAttempt
	OpenInteractions []AdminInteraction
	RunStates        map[string]int32
	Capacity         AdminCapacity
	Connections      []AdminConnection
	Grants           []AdminGrant
}

type AdminAuditEvent struct {
	ID                                                                    int64
	Action, ActorKind, ActorID, ActorUsername, SubjectID, ProjectID, Name string
	OccurredAt                                                            time.Time
}

type AuditFilter struct {
	Action, Actor, ProjectID string
	BeforeID                 int64 // Keyset cursor: events with a smaller id.
	Limit                    int
}

func requireAdmin(caller identity.Caller) error {
	if caller.Role != "owner" && caller.Role != "admin" {
		return ErrAdminDenied
	}
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID) {
		return ErrFenced
	}
	return nil
}

const adminLimit = 200

// AdminOverview reads the operator dashboard for the caller's organization.
func (s *Store) AdminOverview(ctx context.Context, caller identity.Caller) (AdminOverview, error) {
	if err := requireAdmin(caller); err != nil {
		return AdminOverview{}, err
	}
	org := caller.OrganizationID
	out := AdminOverview{RunStates: map[string]int32{}}
	var err error
	if out.LiveAttempts, err = s.liveAttempts(ctx, org, adminLimit); err != nil {
		return AdminOverview{}, err
	}
	rows, err := s.pool.Query(ctx, `SELECT i.id,i.run_id,r.project_id,p.name,r.launch_key,t.task_key,i.kind,
		left(COALESCE(i.payload->>'title',''),300),COALESCE((i.payload->>'blocking')::boolean,false),i.created_at
		FROM workflow_interactions i
		JOIN workflow_tasks t ON t.organization_id=i.organization_id AND t.id=i.task_id
		JOIN workflow_runs r ON r.organization_id=i.organization_id AND r.id=i.run_id
		JOIN workflow_projects p ON p.organization_id=r.organization_id AND p.id=r.project_id
		WHERE i.organization_id=$1 AND i.state='open' AND r.state IN ('queued','active')
		ORDER BY i.created_at,i.id LIMIT $2`, org, adminLimit)
	if err != nil {
		return AdminOverview{}, err
	}
	out.OpenInteractions, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (AdminInteraction, error) {
		var i AdminInteraction
		err := row.Scan(&i.ID, &i.RunID, &i.ProjectID, &i.ProjectName, &i.LaunchKey, &i.Stage, &i.Kind,
			&i.Title, &i.Blocking, &i.CreatedAt)
		return i, err
	})
	if err != nil {
		return AdminOverview{}, err
	}
	// Buckets: escalated > waiting_on_human > running for open runs; a
	// succeeded run with an undecided review package is waiting on a human.
	rows, err = s.pool.Query(ctx, `SELECT bucket,count(*)::integer FROM (SELECT CASE
		WHEN r.state IN ('cancel_requested','cancelled') THEN 'halted'
		WHEN r.state='failed' THEN 'failed'
		WHEN r.state='succeeded' AND r.review_package_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM workflow_review_decisions d
			WHERE d.organization_id=r.organization_id AND d.run_id=r.id AND d.package_id=r.review_package_id) THEN 'waiting_on_human'
		WHEN r.state='succeeded' THEN 'succeeded'
		WHEN EXISTS (SELECT 1 FROM workflow_tasks t WHERE t.organization_id=r.organization_id AND t.run_id=r.id
			AND t.state='escalated') THEN 'escalated'
		WHEN EXISTS (SELECT 1 FROM workflow_interactions i WHERE i.organization_id=r.organization_id AND i.run_id=r.id
			AND i.state='open' AND COALESCE((i.payload->>'blocking')::boolean,false)) THEN 'waiting_on_human'
		ELSE 'running' END AS bucket
		FROM workflow_runs r WHERE r.organization_id=$1
		AND (r.created_at>clock_timestamp()-interval '24 hours' OR r.state IN ('queued','active','cancel_requested'))
		LIMIT 5000) runs GROUP BY bucket`, org)
	if err != nil {
		return AdminOverview{}, err
	}
	var bucket string
	var count int32
	if _, err := pgx.ForEachRow(rows, []any{&bucket, &count}, func() error {
		out.RunStates[bucket] = count
		return nil
	}); err != nil {
		return AdminOverview{}, err
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*)::integer,count(*) FILTER (WHERE state='running')::integer,
		count(*) FILTER (WHERE control_holder_principal_id IS NOT NULL)::integer
		FROM workflow_attempts WHERE organization_id=$1 AND state IN ('reserved','starting','running','reconciling')`, org).
		Scan(&out.Capacity.InFlight, &out.Capacity.Running, &out.Capacity.TakenOver); err != nil {
		return AdminOverview{}, err
	}
	// Connection health: grant/lease counts and last use only; never secrets.
	rows, err = s.pool.Query(ctx, `SELECT c.id,p.provider_kind,regexp_replace(p.origin,'^https://',''),c.external_account_id,
		c.owner_kind,c.state,
		(SELECT count(*)::integer FROM access_grants g WHERE g.organization_id=c.organization_id AND g.connection_id=c.id
			AND g.revoked_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>clock_timestamp())),
		(SELECT count(*)::integer FROM access_leases l WHERE l.organization_id=c.organization_id AND l.connection_id=c.id
			AND l.revoked_at IS NULL AND l.expires_at>clock_timestamp()),
		(SELECT count(*)::integer FROM access_leases l WHERE l.organization_id=c.organization_id AND l.connection_id=c.id
			AND l.revoked_at IS NULL AND l.expires_at>clock_timestamp() AND l.expires_at<=clock_timestamp()+interval '15 minutes'),
		(SELECT max(l.reserved_at) FROM access_leases l WHERE l.organization_id=c.organization_id AND l.connection_id=c.id),
		c.created_at
		FROM access_connections c JOIN access_provider_registrations p
			ON p.organization_id=c.organization_id AND p.id=c.provider_registration_id
		WHERE c.organization_id=$1::text
		ORDER BY c.state='active' DESC,c.created_at DESC,c.id LIMIT $2`, org, adminLimit)
	if err != nil {
		return AdminOverview{}, err
	}
	out.Connections, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (AdminConnection, error) {
		var c AdminConnection
		err := row.Scan(&c.ID, &c.ProviderKind, &c.Host, &c.Account, &c.OwnerKind, &c.State, &c.ActiveGrants,
			&c.ActiveLeases, &c.ExpiringLeases, &c.LastUsed, &c.CreatedAt)
		return c, err
	})
	if err != nil {
		return AdminOverview{}, err
	}
	rows, err = s.pool.Query(ctx, `SELECT g.id,g.connection_id,g.project_id,COALESCE(p.name,''),g.capability,g.resource,
		g.expires_at,g.created_at
		FROM access_grants g
		LEFT JOIN workflow_projects p ON p.organization_id=$1 AND p.id::text=g.project_id
		WHERE g.organization_id=$1::text AND g.revoked_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>clock_timestamp())
		ORDER BY g.created_at DESC,g.id LIMIT $2`, org, adminLimit)
	if err != nil {
		return AdminOverview{}, err
	}
	out.Grants, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (AdminGrant, error) {
		var g AdminGrant
		err := row.Scan(&g.ID, &g.ConnectionID, &g.ProjectID, &g.ProjectName, &g.Capability, &g.Resource,
			&g.ExpiresAt, &g.CreatedAt)
		return g, err
	})
	if err != nil {
		return AdminOverview{}, err
	}
	return out, nil
}

// liveAttempts reads current attempt owners, oldest first. Stage kind,
// harness, and model come from the run's frozen recipe.
func (s *Store) liveAttempts(ctx context.Context, org string, limit int) ([]AdminLiveAttempt, error) {
	rows, err := s.pool.Query(ctx, `SELECT a.id,a.run_id,r.project_id,p.name,r.launch_key,t.task_key,
		COALESCE(st.s->>'kind',''),COALESCE(b.bundle_json->'recipe'->'profiles'->(st.s->>'profile')->>'harness',''),
		COALESCE(b.bundle_json->'recipe'->'profiles'->(st.s->>'profile')->>'model',''),a.state,
		COALESCE(a.control_holder_principal_id::text,''),COALESCE(identity_principal_label(ip.display_name,ip.email,ip.username),''),a.created_at,
		(SELECT e.occurred_at FROM workflow_events e WHERE e.organization_id=a.organization_id
			AND e.attempt_id=a.id ORDER BY e.id DESC LIMIT 1)
		FROM workflow_attempts a
		JOIN workflow_tasks t ON t.organization_id=a.organization_id AND t.id=a.task_id AND t.active_attempt_id=a.id
		JOIN workflow_runs r ON r.organization_id=a.organization_id AND r.id=a.run_id
		JOIN workflow_projects p ON p.organization_id=r.organization_id AND p.id=r.project_id
		LEFT JOIN workflow_run_bundles b ON b.organization_id=r.organization_id AND b.run_id=r.id
		LEFT JOIN LATERAL (SELECT s FROM jsonb_array_elements(b.bundle_json->'recipe'->'stages') s
			WHERE s->>'id'=t.task_key LIMIT 1) st ON true
		LEFT JOIN identity_principals ip ON ip.id=a.control_holder_principal_id
		WHERE a.organization_id=$1 AND a.state IN ('reserved','starting','running','reconciling')
		ORDER BY a.created_at,a.id LIMIT $2`, org, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (AdminLiveAttempt, error) {
		var a AdminLiveAttempt
		err := row.Scan(&a.AttemptID, &a.RunID, &a.ProjectID, &a.ProjectName, &a.LaunchKey, &a.Stage, &a.Kind,
			&a.Harness, &a.Model, &a.State, &a.ControllerID, &a.ControllerUsername, &a.StartedAt, &a.LastActivity)
		return a, err
	})
}

// ListAuditEvents pages the organization's audit log newest first.
func (s *Store) ListAuditEvents(ctx context.Context, caller identity.Caller, f AuditFilter) ([]AdminAuditEvent, error) {
	if err := requireAdmin(caller); err != nil {
		return nil, err
	}
	f.Action, f.Actor = strings.TrimSpace(f.Action), strings.ToLower(strings.TrimSpace(f.Actor))
	if f.Limit < 1 || f.Limit > 101 || f.BeforeID < 0 || len(f.Action) > 128 || len(f.Actor) > 254 ||
		(f.ProjectID != "" && !ids(f.ProjectID)) {
		return nil, ErrInvalid
	}
	actorID := ""
	if f.Actor != "" {
		if ids(f.Actor) {
			actorID = f.Actor
		} else if err := s.pool.QueryRow(ctx, `SELECT p.id FROM identity_principals p JOIN identity_memberships m
			ON m.principal_id=p.id WHERE m.organization_id=$1 AND (lower(p.email)=$2 OR p.username=$2)`, caller.OrganizationID, f.Actor).
			Scan(&actorID); errors.Is(err, pgx.ErrNoRows) {
			return []AdminAuditEvent{}, nil
		} else if err != nil {
			return nil, err
		}
	}
	before := f.BeforeID
	if before == 0 {
		before = 1<<63 - 1
	}
	rows, err := s.pool.Query(ctx, `SELECT e.id,e.action,e.actor_kind,COALESCE(e.actor_id::text,''),COALESCE(identity_principal_label(ip.display_name,ip.email,ip.username),''),
		COALESCE(e.subject_id::text,''),COALESCE(e.project_id::text,''),COALESCE(p.name,''),e.occurred_at
		FROM identity_audit_events e
		LEFT JOIN identity_principals ip ON ip.id=e.actor_id
		LEFT JOIN workflow_projects p ON p.organization_id=e.organization_id AND p.id=e.project_id
		WHERE e.organization_id=$1 AND e.id<$2
		AND ($3='' OR e.action=$3) AND ($4='' OR e.actor_id=$4::uuid) AND ($5='' OR e.project_id=$5::uuid)
		ORDER BY e.id DESC LIMIT $6`, caller.OrganizationID, before, f.Action, actorID, f.ProjectID, f.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (AdminAuditEvent, error) {
		var e AdminAuditEvent
		err := row.Scan(&e.ID, &e.Action, &e.ActorKind, &e.ActorID, &e.ActorUsername, &e.SubjectID,
			&e.ProjectID, &e.Name, &e.OccurredAt)
		return e, err
	})
}

// HaltRunAs requests cancellation of a queued or active run for an owner or
// admin. The completion sweep stops live attempts; Progress then closes it.
func (s *Store) HaltRunAs(ctx context.Context, caller identity.Caller, runID string) (string, error) {
	if err := requireAdmin(caller); err != nil {
		return "", err
	}
	if !ids(runID) {
		return "", ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if err := lockProjectModelAccessAdmin(ctx, tx, caller); err != nil {
		return "", err
	}
	changed, err := requestCancel(ctx, tx, caller.OrganizationID, runID)
	if err != nil {
		return "", err
	}
	if changed {
		if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events (organization_id,actor_kind,actor_id,action,subject_id)
			VALUES ($1,'principal',$2,'workflow.run.halted',$3)`, caller.OrganizationID, caller.PrincipalID, runID); err != nil {
			return "", err
		}
	}
	return "cancel_requested", tx.Commit(ctx)
}

// RevokeGrantAs revokes one active connection grant. A project model grant
// takes the existing model-access revocation path; a Git grant is revoked
// with its bindings' leases. The connection itself stays for other projects.
func (s *Store) RevokeGrantAs(ctx context.Context, caller identity.Caller, grantID string) error {
	if err := requireAdmin(caller); err != nil {
		return err
	}
	if !ids(grantID) {
		return ErrInvalid
	}
	var modelAccessID string
	err := s.pool.QueryRow(ctx, `SELECT id::text FROM workflow_project_model_grants
		WHERE organization_id=$1 AND grant_id=$2 AND revoked_at IS NULL`, caller.OrganizationID, grantID).Scan(&modelAccessID)
	if err == nil {
		return s.RevokeProjectModelAccessAs(ctx, caller, modelAccessID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockProjectModelAccessAdmin(ctx, tx, caller); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE access_grants SET revoked_at=clock_timestamp(),version=version+1
		WHERE organization_id=$1::text AND id=$2 AND revoked_at IS NULL AND capability IN ('git.read','git.write')`,
		caller.OrganizationID, grantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `UPDATE access_leases SET revoked_at=clock_timestamp()
		WHERE organization_id=$1::text AND revoked_at IS NULL AND binding_id IN
		(SELECT id FROM access_bindings WHERE organization_id=$1::text AND grant_id=$2)`,
		caller.OrganizationID, grantID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events (organization_id,actor_kind,actor_id,action,subject_id)
		VALUES ($1,'principal',$2,'access.grant.revoked',$3)`, caller.OrganizationID, caller.PrincipalID, grantID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
