package access

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// Private Git source: a project references one organization Git connection.
// The dispatcher workload holds git.read (AX Workspace setup phase only) and
// git.write (the platform's run-branch push) grants on it for the project's
// repository. Neither credential ever reaches an agent's model phase.

// DispatcherGrantee is the workload that reads and pushes project Git.
const DispatcherGrantee = "blaxsmith-dispatcher"

func gitOrigin(repoURL string) (string, error) {
	u, err := url.Parse(repoURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", ErrDenied
	}
	return "https://" + strings.ToLower(u.Host), nil
}

// SetProjectGit points the project's dispatcher Git grants at connectionID
// for repoURL: grants on any other connection or repository are revoked, and
// an empty connectionID (a public source) leaves none. Otherwise it grants
// git.read and git.write and enables Git delivery in the project policy. The
// connection must be an active Git connection for the repository's host.
// Existing matching grants are kept, so reselecting the source is idempotent.
func SetProjectGit(ctx context.Context, tx pgx.Tx, organizationID, projectID, connectionID, repoURL, issuerID string) error {
	origin, err := gitOrigin(repoURL)
	if tx == nil || err != nil || organizationID == "" || projectID == "" || issuerID == "" {
		return ErrDenied
	}
	if _, err := tx.Exec(ctx, `WITH revoked AS (UPDATE access_grants SET revoked_at=clock_timestamp(),version=version+1
		WHERE organization_id=$1 AND project_id=$2 AND grantee_kind='workload' AND grantee_id=$3
		AND capability IN ('git.read','git.write') AND revoked_at IS NULL AND (connection_id<>$4 OR resource<>$5)
		RETURNING id)
		UPDATE access_leases SET revoked_at=clock_timestamp() WHERE organization_id=$1 AND revoked_at IS NULL
		AND binding_id IN (SELECT b.id FROM access_bindings b JOIN revoked r ON b.grant_id=r.id WHERE b.organization_id=$1)`,
		organizationID, projectID, DispatcherGrantee, connectionID, repoURL); err != nil {
		return fmt.Errorf("revoke previous project Git grants: %w", err)
	}
	if connectionID == "" {
		return nil
	}
	var ok bool
	err = tx.QueryRow(ctx, `SELECT true FROM access_connections c
		JOIN access_provider_registrations p ON p.organization_id=c.organization_id AND p.id=c.provider_registration_id
		WHERE c.organization_id=$1 AND c.id=$2 AND c.state='active' AND c.active_secret_version IS NOT NULL
		AND (c.owner_kind='organization' OR (c.owner_kind='project' AND c.owner_id=$4))
		AND p.state='active' AND p.provider_kind='git' AND p.origin=$3 FOR SHARE OF c,p`,
		organizationID, connectionID, origin, projectID).Scan(&ok)
	if err != nil {
		return deniedOrError("git connection", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO access_project_policies
		(organization_id,project_id,version,git_read_enabled,delivery_modes)
		VALUES ($1,$2,1,true,ARRAY['native_raw'])
		ON CONFLICT (organization_id,project_id) DO UPDATE SET
		version=CASE WHEN access_project_policies.git_read_enabled AND 'native_raw'=ANY(access_project_policies.delivery_modes)
			THEN access_project_policies.version ELSE access_project_policies.version+1 END,
		git_read_enabled=true,
		delivery_modes=CASE WHEN 'native_raw'=ANY(access_project_policies.delivery_modes)
			THEN access_project_policies.delivery_modes
			ELSE array_append(access_project_policies.delivery_modes,'native_raw') END`,
		organizationID, projectID); err != nil {
		return fmt.Errorf("enable project Git policy: %w", err)
	}
	for _, capability := range []string{"git.read", "git.write"} {
		if _, err := tx.Exec(ctx, `INSERT INTO access_grants
			(organization_id,id,connection_id,project_id,grantee_kind,grantee_id,capability,resource,delivery_mode,issuer_id)
			SELECT $1,gen_random_uuid()::text,$2,$3,'workload',$4,$5,$6,'native_raw',$7
			WHERE NOT EXISTS (SELECT 1 FROM access_grants WHERE organization_id=$1 AND connection_id=$2 AND project_id=$3
				AND grantee_kind='workload' AND grantee_id=$4 AND capability=$5 AND resource=$6 AND revoked_at IS NULL
				AND (expires_at IS NULL OR expires_at>clock_timestamp()))`,
			organizationID, connectionID, projectID, DispatcherGrantee, capability, repoURL, issuerID); err != nil {
			return fmt.Errorf("grant project %s: %w", capability, err)
		}
	}
	return nil
}

// BindGitRead freezes the project's current git.read grant on connectionID
// for one reserved attempt at its input commit. ErrDenied means the private
// source has no usable grant.
func BindGitRead(ctx context.Context, tx pgx.Tx, organizationID, projectID, connectionID, repoURL, attemptID, commit string) (string, error) {
	if tx == nil || attemptID == "" || !validCommit(commit) {
		return "", ErrDenied
	}
	grantID, grantVersion, policyVersion, err := gitReadGrant(ctx, tx, organizationID, projectID, connectionID, repoURL)
	if err != nil {
		return "", err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	bindingID := base64.RawURLEncoding.EncodeToString(id[:])
	if _, err := tx.Exec(ctx, `INSERT INTO access_bindings
		(organization_id,id,attempt_id,project_id,grant_id,grant_version,capability,resource,input_commit,policy_version)
		VALUES ($1,$2,$3,$4,$5,$6,'git.read',$7,$8,$9)`, organizationID, bindingID, attemptID, projectID,
		grantID, grantVersion, repoURL, commit, policyVersion); err != nil {
		return "", fmt.Errorf("bind git read: %w", err)
	}
	return bindingID, nil
}

// PreflightGitRead reports ErrDenied when a private source's connection has
// no active git.read grant, before any attempt is reserved.
func PreflightGitRead(ctx context.Context, db *pgxpool.Pool, organizationID, projectID, connectionID, repoURL string) error {
	ctx = tenant.Org(ctx, organizationID)
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, _, _, err = gitReadGrant(ctx, tx, organizationID, projectID, connectionID, repoURL)
	return err
}

func gitReadGrant(ctx context.Context, tx pgx.Tx, organizationID, projectID, connectionID, repoURL string) (string, int64, int64, error) {
	origin, err := gitOrigin(repoURL)
	if err != nil || organizationID == "" || projectID == "" || connectionID == "" {
		return "", 0, 0, ErrDenied
	}
	var grantID string
	var grantVersion, policyVersion int64
	err = tx.QueryRow(ctx, `SELECT g.id,g.version,pp.version FROM access_grants g
		JOIN access_project_policies pp ON pp.organization_id=g.organization_id AND pp.project_id=g.project_id
		JOIN access_connections c ON c.organization_id=g.organization_id AND c.id=g.connection_id
		JOIN access_provider_registrations p ON p.organization_id=c.organization_id AND p.id=c.provider_registration_id
		WHERE g.organization_id=$1 AND g.project_id=$2 AND g.connection_id=$3 AND g.capability='git.read'
		AND g.resource=$4 AND g.grantee_kind='workload' AND g.grantee_id=$5 AND g.revoked_at IS NULL
		AND (g.expires_at IS NULL OR g.expires_at>clock_timestamp()) AND pp.git_read_enabled
		AND c.state='active' AND c.active_secret_version IS NOT NULL AND p.state='active' AND p.provider_kind='git' AND p.origin=$6
		ORDER BY g.created_at DESC,g.id LIMIT 1 FOR SHARE OF g,pp,c,p`,
		organizationID, projectID, connectionID, repoURL, DispatcherGrantee, origin).Scan(&grantID, &grantVersion, &policyVersion)
	if err != nil {
		return "", 0, 0, deniedOrError("git read grant", err)
	}
	return grantID, grantVersion, policyVersion, nil
}

// AttemptGitRead returns an attempt's frozen git.read binding for the setup
// phase, with the connection account as the Git username. ok is false for
// public sources (no binding).
func AttemptGitRead(ctx context.Context, db *pgxpool.Pool, organizationID, attemptID string) (GitRead, string, bool, error) {
	ctx = tenant.Org(ctx, organizationID)
	var r GitRead
	var username string
	err := db.QueryRow(ctx, `SELECT b.id,b.project_id,g.grantee_kind,g.grantee_id,b.resource,b.input_commit,b.policy_version,
		c.external_account_id FROM access_bindings b
		JOIN access_grants g ON g.organization_id=b.organization_id AND g.id=b.grant_id
		JOIN access_connections c ON c.organization_id=g.organization_id AND c.id=g.connection_id
		WHERE b.organization_id=$1 AND b.attempt_id=$2 AND b.capability='git.read'
		ORDER BY b.created_at DESC LIMIT 1`, organizationID, attemptID).
		Scan(&r.BindingID, &r.ProjectID, &r.GranteeKind, &r.GranteeID, &r.RepoURL, &r.Commit, &r.PolicyVersion, &username)
	if errors.Is(err, pgx.ErrNoRows) {
		return GitRead{}, "", false, nil
	}
	if err != nil {
		return GitRead{}, "", false, err
	}
	r.OrganizationID, r.AttemptID = organizationID, attemptID
	return r, username, true, nil
}
