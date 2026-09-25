package workflow

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

var (
	ErrProjectSourceDenied = errors.New("project source change denied")
	ErrSourceRoute         = errors.New("repository host has no verified public route")
	// ErrGitConnection: a private source names no active Git connection for
	// its host, or the connection has no git.read grant for the project.
	ErrGitConnection     = errors.New("private Git source has no granted Git connection")
	sourceSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	sourceRefPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)
	reservedIPv4         = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	}
	publicIPv6   = netip.MustParsePrefix("2000::/3")
	reservedIPv6 = []netip.Prefix{
		netip.MustParsePrefix("2001::/32"), netip.MustParsePrefix("2001:db8::/32"),
		netip.MustParsePrefix("2002::/16"),
	}
)

type ProjectSource struct {
	ProjectID     string
	RepositoryURL string
	Ref           string
	// GitConnectionID names the organization Git connection for a private
	// repository; empty means the source is fetched anonymously.
	GitConnectionID string
	UpdatedAt       time.Time
}

// ValidatePublicGitSource is also required immediately before any server-side
// fetch. A DNS precheck does not pin Git's later connection: the fetcher must
// additionally block internal egress and HTTP redirects.
func ValidatePublicGitSource(ctx context.Context, rawURL, ref string) (string, string, error) {
	return validatePublicGitSource(ctx, rawURL, ref, net.DefaultResolver.LookupIPAddr)
}

func validatePublicGitSource(ctx context.Context, rawURL, ref string, lookup func(context.Context, string) ([]net.IPAddr, error)) (string, string, error) {
	canonical, host, err := canonicalPublicGitSource(rawURL, ref)
	if err != nil {
		return "", "", err
	}
	addresses, err := lookup(ctx, host)
	if err != nil || len(addresses) == 0 {
		return "", "", ErrSourceRoute
	}
	for _, address := range addresses {
		ip, ok := netip.AddrFromSlice(address.IP)
		if !ok || !publicSourceAddress(ip.Unmap()) {
			return "", "", ErrSourceRoute
		}
	}
	return canonical, ref, nil
}

func canonicalPublicGitSource(rawURL, ref string) (string, string, error) {
	if len(rawURL) == 0 || len(rawURL) > 2048 || len(ref) > 128 {
		return "", "", ErrInvalid
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.RawFragment != "" || u.Port() != "" || u.EscapedPath() != u.Path {
		return "", "", ErrInvalid
	}
	host := strings.ToLower(u.Hostname())
	if host != "github.com" && host != "gitlab.com" {
		return "", "", ErrInvalid
	}
	segments := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if !strings.HasPrefix(u.Path, "/") || len(segments) < 2 || (host == "github.com" && len(segments) != 2) {
		return "", "", ErrInvalid
	}
	for _, segment := range segments {
		if len(segment) > 128 || !sourceSegmentPattern.MatchString(segment) ||
			strings.Contains(segment, "..") || strings.HasSuffix(segment, ".") || segment == ".git" {
			return "", "", ErrInvalid
		}
	}
	if ref != "" {
		if !sourceRefPattern.MatchString(ref) || strings.Contains(ref, "..") || strings.Contains(ref, "//") ||
			strings.HasSuffix(ref, ".") || strings.HasSuffix(ref, "/") {
			return "", "", ErrInvalid
		}
		for _, part := range strings.Split(ref, "/") {
			if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
				return "", "", ErrInvalid
			}
		}
	}
	return "https://" + host + u.Path, host, nil
}

func publicSourceAddress(ip netip.Addr) bool {
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	if ip.Is4() {
		for _, prefix := range reservedIPv4 {
			if prefix.Contains(ip) {
				return false
			}
		}
		return true
	}
	if !publicIPv6.Contains(ip) {
		return false
	}
	for _, prefix := range reservedIPv6 {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func (s *Store) GetProjectSource(ctx context.Context, orgID, projectID string) (ProjectSource, error) {
	ctx = tenant.Org(ctx, orgID)
	if !ids(orgID, projectID) {
		return ProjectSource{}, ErrInvalid
	}
	var source ProjectSource
	err := s.pool.QueryRow(ctx, `SELECT project_id,repository_url,git_ref,COALESCE(git_connection_id,''),updated_at
		FROM workflow_project_sources WHERE organization_id=$1 AND project_id=$2`, orgID, projectID).
		Scan(&source.ProjectID, &source.RepositoryURL, &source.Ref, &source.GitConnectionID, &source.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectSource{}, ErrNotFound
	}
	return source, err
}

// SetProjectSourceAs validates the route, then rechecks the live owner/admin
// session and writes the source and principal audit record atomically.
// A non-empty gitConnectionID makes the source private: the dispatcher is
// granted git.read and git.write on that connection for this repository.
func (s *Store) SetProjectSourceAs(ctx context.Context, caller identity.Caller, projectID, rawURL, ref, gitConnectionID string) (ProjectSource, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID) ||
		(caller.Role != "owner" && caller.Role != "admin") {
		return ProjectSource{}, ErrProjectSourceDenied
	}
	if !ids(projectID) {
		return ProjectSource{}, ErrInvalid
	}
	repositoryURL, ref, err := ValidatePublicGitSource(ctx, rawURL, ref)
	if err != nil {
		return ProjectSource{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProjectSource{}, err
	}
	defer tx.Rollback(ctx)
	var role string
	err = tx.QueryRow(ctx, `SELECT m.role FROM identity_sessions s
		JOIN identity_memberships m ON m.organization_id=s.organization_id AND m.principal_id=s.principal_id
		JOIN identity_principals p ON p.id=s.principal_id
		JOIN identity_organizations o ON o.id=s.organization_id
		WHERE s.organization_id=$1 AND s.id=$2 AND s.principal_id=$3
		AND m.role=$4 AND m.role IN ('owner','admin') AND $5::timestamptz>clock_timestamp()
		AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
		AND m.state='active' AND p.state='active'
		AND (s.auth_method<>'local' OR o.login_policy IN ('local','mixed'))
		AND (o.mfa_policy<>'required' OR s.mfa_level='totp')
		FOR SHARE OF s,m,p,o`, caller.OrganizationID, caller.SessionID, caller.PrincipalID,
		caller.Role, caller.AccessExpires).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectSource{}, ErrProjectSourceDenied
	}
	if err != nil {
		return ProjectSource{}, err
	}
	var exists bool
	err = tx.QueryRow(ctx, `SELECT true FROM workflow_projects WHERE organization_id=$1 AND id=$2 FOR SHARE`,
		caller.OrganizationID, projectID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectSource{}, ErrNotFound
	}
	if err != nil {
		return ProjectSource{}, err
	}
	var source ProjectSource
	err = tx.QueryRow(ctx, `INSERT INTO workflow_project_sources
		(organization_id,project_id,repository_url,git_ref,git_connection_id) VALUES ($1,$2,$3,$4,NULLIF($5,''))
		ON CONFLICT (organization_id,project_id) DO UPDATE
		SET repository_url=EXCLUDED.repository_url,git_ref=EXCLUDED.git_ref,
			git_connection_id=EXCLUDED.git_connection_id,updated_at=clock_timestamp()
		RETURNING project_id,repository_url,git_ref,COALESCE(git_connection_id,''),updated_at`,
		caller.OrganizationID, projectID, repositoryURL, ref, gitConnectionID).
		Scan(&source.ProjectID, &source.RepositoryURL, &source.Ref, &source.GitConnectionID, &source.UpdatedAt)
	if err != nil {
		return ProjectSource{}, err
	}
	if err := access.SetProjectGit(ctx, tx, caller.OrganizationID, projectID, gitConnectionID, repositoryURL,
		caller.PrincipalID); errors.Is(err, access.ErrDenied) {
		return ProjectSource{}, ErrGitConnection
	} else if err != nil {
		return ProjectSource{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events
		(organization_id,actor_kind,actor_id,action,subject_id)
		VALUES ($1,'principal',$2,'workflow.project_source.set',$3)`,
		caller.OrganizationID, caller.PrincipalID, projectID); err != nil {
		return ProjectSource{}, err
	}
	return source, tx.Commit(ctx)
}
