package workflow

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// requireConnectionManager is the management rule of the connections hub:
// organization owners and admins for organization connections, project
// admins for project ones, and the owner of a personal one.
func (s *Store) requireConnectionManager(ctx context.Context, tx pgx.Tx, caller identity.Caller, r connectionRow) error {
	switch r.OwnerKind {
	case "organization":
		if caller.Role != "owner" && caller.Role != "admin" {
			return ErrConnectionDenied
		}
	case "project":
		return s.requireProjectAdmin(ctx, tx, caller, r.OwnerID)
	default:
		if r.OwnerID != caller.PrincipalID {
			return ErrNotFound
		}
	}
	return nil
}

// SetConnectionBaseURLAs sets or clears (empty) an active API-key
// connection's base URL, already normalized by access.NormalizeBaseURL. The
// stored model list came from the old endpoint, so it is dropped until the
// next refresh. Attempts bound to the old URL are fenced: credential release
// and lease renewal compare the binding's frozen URL with the connection's.
func (s *Store) SetConnectionBaseURLAs(ctx context.Context, caller identity.Caller, connectionID, baseURL string) error {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID) || connectionID == "" || len(connectionID) > 64 {
		return ErrInvalid
	}
	if normalized, err := access.NormalizeBaseURL(baseURL); err != nil || normalized != baseURL {
		return access.ErrBaseURL
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockCallerSession(ctx, tx, caller, true); err != nil {
		return err
	}
	r, err := lockConnection(ctx, tx, caller.OrganizationID, connectionID, true)
	if err != nil {
		return err
	}
	if err := s.requireConnectionManager(ctx, tx, caller, r); err != nil {
		return err
	}
	if r.State != "active" || r.AuthMethod != access.APIKeyAuth || access.ModelOrigin(r.Provider) == "" {
		return ErrInvalid
	}
	var previous string
	if err := tx.QueryRow(ctx, `SELECT base_url FROM access_connections WHERE organization_id=$1 AND id=$2`,
		caller.OrganizationID, connectionID).Scan(&previous); err != nil {
		return err
	}
	if previous == baseURL {
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `UPDATE access_connections SET base_url=$3,models_checked_at=NULL,models_error=NULL
		WHERE organization_id=$1 AND id=$2`, caller.OrganizationID, connectionID, baseURL); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM access_connection_models WHERE organization_id=$1 AND connection_id=$2`,
		caller.OrganizationID, connectionID); err != nil {
		return err
	}
	detail, err := json.Marshal(map[string]string{"from": previous, "to": baseURL})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events (organization_id,actor_kind,actor_id,action,subject_id,detail)
		VALUES ($1,'principal',$2,'access.connection.base_url_changed',$3,$4)`,
		caller.OrganizationID, caller.PrincipalID, connectionID, detail); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ManagedConnectionAs reads one connection for a caller who manages it.
func (s *Store) ManagedConnectionAs(ctx context.Context, caller identity.Caller, connectionID string) (Connection, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if !ids(caller.OrganizationID, caller.PrincipalID) || connectionID == "" || len(connectionID) > 64 {
		return Connection{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Connection{}, err
	}
	defer tx.Rollback(ctx)
	r, err := lockConnection(ctx, tx, caller.OrganizationID, connectionID, false)
	if err != nil {
		return Connection{}, err
	}
	if err := s.requireConnectionManager(ctx, tx, caller, r); err != nil {
		return Connection{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Connection{}, err
	}
	return s.connectionFor(ctx, caller, connectionID)
}
