package access

import (
	"context"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
)

// AuthorizeGitWrite resolves the project's active git.write grant for repoURL
// to its connection. Only the platform pushes the run branch with it; no
// agent or guest ever receives this credential. ExternalAccountID is the Git
// username for HTTPS basic auth.
func AuthorizeGitWrite(ctx context.Context, tx pgx.Tx, organizationID, projectID, repoURL string) (Decision, error) {
	u, err := url.Parse(repoURL)
	if tx == nil || organizationID == "" || projectID == "" || err != nil || u.Scheme != "https" ||
		u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return Decision{}, ErrDenied
	}
	var d Decision
	err = tx.QueryRow(ctx, `SELECT c.id,c.provider_registration_id,c.external_account_id,g.id,g.delivery_mode
		FROM access_grants g
		JOIN access_connections c ON c.organization_id=g.organization_id AND c.id=g.connection_id
		JOIN access_provider_registrations p ON p.organization_id=c.organization_id AND p.id=c.provider_registration_id
		WHERE g.organization_id=$1 AND g.project_id=$2 AND g.capability='git.write' AND g.resource=$3
		AND g.grantee_kind='workload' AND g.grantee_id='`+DispatcherGrantee+`'
		AND g.revoked_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>clock_timestamp())
		AND c.state='active' AND p.state='active' AND p.provider_kind='git' AND p.origin=$4
		ORDER BY g.created_at DESC,g.id LIMIT 1 FOR SHARE OF g,c,p`,
		organizationID, projectID, repoURL, "https://"+strings.ToLower(u.Host)).
		Scan(&d.ConnectionID, &d.ProviderID, &d.ExternalAccountID, &d.GrantID, &d.DeliveryMode)
	if err != nil {
		return Decision{}, deniedOrError("git write grant", err)
	}
	return d, nil
}
