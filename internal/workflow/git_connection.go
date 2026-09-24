package workflow

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/identity"
)

var gitUsername = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// GitConnection is an organization Git credential for private sources. The
// token never leaves secret custody except to the platform's own fetch/push
// and the AX Workspace setup phase.
type GitConnection struct {
	ID, Host, Username string
	CreatedAt          time.Time
}

func (s *Store) ListGitConnections(ctx context.Context, orgID string) ([]GitConnection, error) {
	if !ids(orgID) {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT c.id,substr(p.origin,9),c.external_account_id,c.created_at
		FROM access_connections c JOIN access_provider_registrations p
			ON p.organization_id=c.organization_id AND p.id=c.provider_registration_id
		WHERE c.organization_id=$1 AND c.state='active' AND p.state='active' AND p.provider_kind='git'
		ORDER BY c.created_at,c.id`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GitConnection{}
	for rows.Next() {
		var c GitConnection
		if err := rows.Scan(&c.ID, &c.Host, &c.Username, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateGitConnectionAs stores an organization Git token (owner/admin only)
// as a provider registration, connection, and encrypted secret in one
// transaction. It never returns credential material.
func (s *Store) CreateGitConnectionAs(ctx context.Context, caller identity.Caller, host, username string,
	token []byte, secrets *access.SecretStore) (GitConnection, error) {
	if caller.Role != "owner" && caller.Role != "admin" {
		return GitConnection{}, ErrProjectModelAccessDenied
	}
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID) || secrets == nil {
		return GitConnection{}, ErrFenced
	}
	if (host != "github.com" && host != "gitlab.com") || !gitUsername.MatchString(username) ||
		len(token) == 0 || len(token) > 8192 || strings.ContainsAny(string(token), "\r\n\x00 ") {
		return GitConnection{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return GitConnection{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockProjectModelAccessAdmin(ctx, tx, caller); err != nil {
		return GitConnection{}, err
	}
	var providerID string
	if err := tx.QueryRow(ctx, `INSERT INTO access_provider_registrations
		(organization_id,id,provider_kind,origin,delivery_modes,state)
		VALUES ($1,gen_random_uuid()::text,'git',$2,ARRAY['native_raw'],'active') RETURNING id`,
		caller.OrganizationID, "https://"+host).Scan(&providerID); err != nil {
		return GitConnection{}, err
	}
	c := GitConnection{Host: host, Username: username}
	if err := tx.QueryRow(ctx, `INSERT INTO access_connections
		(organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state)
		VALUES ($1,gen_random_uuid()::text,'organization',$1,$2,$3,'token','active') RETURNING id,created_at`,
		caller.OrganizationID, providerID, username).Scan(&c.ID, &c.CreatedAt); err != nil {
		return GitConnection{}, err
	}
	if _, err := secrets.RotateTx(ctx, tx, caller.OrganizationID, c.ID, 0, token, nil); err != nil {
		return GitConnection{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events
		(organization_id,actor_kind,actor_id,action,subject_id)
		VALUES ($1,'principal',$2,'access.git_connection.created',$3)`,
		caller.OrganizationID, caller.PrincipalID, c.ID); err != nil {
		return GitConnection{}, err
	}
	return c, tx.Commit(ctx)
}
