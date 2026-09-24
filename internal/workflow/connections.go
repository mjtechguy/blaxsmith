package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/identity"
)

// Connections hub (plan §10.2–10.3). Organization connections are managed by
// org owners/admins and reach projects only through standing grants; project
// connections by that project's admins; personal connections by their owner,
// and a personal grant serves only runs its owner launches. No function here
// returns secret material.

var ErrConnectionDenied = errors.New("connection management denied")

const (
	ScopeOrganization     = "organization"
	ScopeProject          = "project"
	ScopePersonal         = "personal"
	ScopeProjectAvailable = "project_available"
)

type ConnectionGrant struct {
	ID, ProjectID, ProjectName, GranteeKind, GranteeID, GranteeName string
	CreatedAt                                                       time.Time
}

type ConnectionUse struct {
	ID, ProjectID, ProjectName, Model, GranteeKind string
	CreatedAt                                      time.Time
}

type Connection struct {
	ID, Scope, OwnerID, OwnerName, Kind, Provider, Account, Label, State string
	Grants                                                               []ConnectionGrant
	Uses                                                                 []ConnectionUse
	LastUsed, ModelsCheckedAt                                            *time.Time
	CreatedAt                                                            time.Time
	ModelCount                                                           int32
	ModelsError                                                          string
	CanManage                                                            bool
}

type ConnectionModel struct {
	ID, DisplayName string
	ReleasedAt      *time.Time
	ContextTokens   int32
	Capabilities    string
	Harnesses       []string
}

// ponytail: which models each pinned harness accepts is a static rule table.
// Replace with the approved-runtime records if the harness pins get richer.
var harnessRules = []struct {
	Harness string
	Accepts func(provider, model string) bool
}{
	{"codex", func(p, m string) bool {
		return p == "openai" && (strings.HasPrefix(m, "gpt-") || strings.HasPrefix(m, "codex") ||
			(len(m) > 1 && m[0] == 'o' && m[1] >= '0' && m[1] <= '9')) &&
			!strings.Contains(m, "audio") && !strings.Contains(m, "realtime") && !strings.Contains(m, "tts") &&
			!strings.Contains(m, "transcribe") && !strings.Contains(m, "image") && !strings.Contains(m, "search")
	}},
	{"claude-code", func(p, m string) bool { return p == "anthropic" && strings.HasPrefix(m, "claude-") }},
	{"opencode", func(p, m string) bool {
		return p == "openai" || p == "anthropic" || p == "opencode" || p == "opencode-go"
	}},
}

// HarnessesFor lists the pinned harnesses that accept provider/model.
func HarnessesFor(provider, model string) []string {
	var out []string
	for _, rule := range harnessRules {
		if rule.Accepts(provider, model) {
			out = append(out, rule.Harness)
		}
	}
	return out
}

func validScope(scope string) bool {
	return scope == ScopeOrganization || scope == ScopeProject || scope == ScopePersonal
}

// beginScoped opens a transaction holding the caller's live session and
// authorizes creating or managing connections at scope.
func (s *Store) beginScoped(ctx context.Context, caller identity.Caller, scope, projectID string) (pgx.Tx, error) {
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID) || !validScope(scope) ||
		(scope == ScopeProject) != (projectID != "") || (projectID != "" && !ids(projectID)) {
		return nil, ErrInvalid
	}
	if scope == ScopeOrganization && caller.Role != "owner" && caller.Role != "admin" {
		return nil, ErrConnectionDenied
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if err := lockCallerSession(ctx, tx, caller, scope != ScopeOrganization); err != nil {
		tx.Rollback(ctx)
		return nil, err
	}
	if scope == ScopeProject {
		if err := s.requireProjectAdmin(ctx, tx, caller, projectID); err != nil {
			tx.Rollback(ctx)
			return nil, err
		}
	}
	return tx, nil
}

func (s *Store) requireProjectAdmin(ctx context.Context, tx pgx.Tx, caller identity.Caller, projectID string) error {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT true FROM workflow_projects WHERE organization_id=$1 AND id=$2 FOR SHARE`,
		caller.OrganizationID, projectID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	ok, err := s.authz.CanAdministerProject(ctx, tx, caller, projectID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrConnectionDenied
	}
	return nil
}

func scopeOwner(caller identity.Caller, scope, projectID string) (string, string) {
	switch scope {
	case ScopeOrganization:
		return "organization", caller.OrganizationID
	case ScopeProject:
		return "project", projectID
	}
	return "user", caller.PrincipalID
}

func audit(ctx context.Context, tx pgx.Tx, caller identity.Caller, action, subject string) error {
	_, err := tx.Exec(ctx, `INSERT INTO identity_audit_events (organization_id,actor_kind,actor_id,action,subject_id)
		VALUES ($1,'principal',$2,$3,$4)`, caller.OrganizationID, caller.PrincipalID, action, subject)
	return err
}

func commitAudited(ctx context.Context, tx pgx.Tx, caller identity.Caller, action, subject string) error {
	if err := audit(ctx, tx, caller, action, subject); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var catalogModelID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func knownHarness(harness string) bool {
	for _, rule := range harnessRules {
		if rule.Harness == harness {
			return true
		}
	}
	return false
}

func cleanLabel(label string) (*string, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return nil, nil
	}
	if len(label) > 120 || strings.ContainsAny(label, "\r\n\x00") {
		return nil, ErrInvalid
	}
	return &label, nil
}

// CreateAPIKeyConnectionAs stores a model API key at scope with the model
// list the caller already fetched with it (the fetch is the key check).
func (s *Store) CreateAPIKeyConnectionAs(ctx context.Context, caller identity.Caller, scope, projectID, provider, label string,
	key []byte, models []access.CatalogModel, modelsErr string, secrets *access.SecretStore) (Connection, error) {
	origin := access.ModelOrigin(provider)
	if origin == "" || len(key) == 0 || len(key) > 8192 || strings.ContainsAny(string(key), "\r\n\x00 ") || secrets == nil {
		return Connection{}, ErrInvalid
	}
	cleaned, err := cleanLabel(label)
	if err != nil {
		return Connection{}, err
	}
	tx, err := s.beginScoped(ctx, caller, scope, projectID)
	if err != nil {
		return Connection{}, err
	}
	defer tx.Rollback(ctx)
	ownerKind, ownerID := scopeOwner(caller, scope, projectID)
	var providerID, connectionID string
	if err := tx.QueryRow(ctx, `INSERT INTO access_provider_registrations
		(organization_id,id,provider_kind,origin,delivery_modes,state)
		VALUES ($1,gen_random_uuid()::text,$2,$3,ARRAY['native_raw'],'active') RETURNING id`,
		caller.OrganizationID, provider, origin).Scan(&providerID); err != nil {
		return Connection{}, err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO access_connections
		(organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state,label)
		VALUES ($1,gen_random_uuid()::text,$2,$3,$4,$5,'api_key','active',$6) RETURNING id`,
		caller.OrganizationID, ownerKind, ownerID, providerID, "unverified-"+provider+"-api-key", cleaned).Scan(&connectionID); err != nil {
		return Connection{}, err
	}
	if _, err := secrets.RotateTx(ctx, tx, caller.OrganizationID, connectionID, 0, key, nil); err != nil {
		return Connection{}, err
	}
	if err := replaceModels(ctx, tx, caller.OrganizationID, connectionID, models, modelsErr); err != nil {
		return Connection{}, err
	}
	if err := audit(ctx, tx, caller, "access.connection.created", connectionID); err != nil {
		return Connection{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Connection{}, err
	}
	return s.connectionFor(ctx, caller, connectionID)
}

// CreateGitTokenConnectionAs stores a GitHub/GitLab token at organization or
// project scope. Organization scope is CreateGitConnectionAs.
func (s *Store) CreateGitTokenConnectionAs(ctx context.Context, caller identity.Caller, scope, projectID, host, username, authMethod string,
	token []byte, secrets *access.SecretStore) (Connection, error) {
	if scope == ScopePersonal {
		return Connection{}, ErrInvalid
	}
	if (host != "github.com" && host != "gitlab.com") || !gitUsername.MatchString(username) ||
		len(token) == 0 || len(token) > 8192 || strings.ContainsAny(string(token), "\r\n\x00 ") || secrets == nil ||
		(authMethod != "token" && authMethod != "oauth_token") {
		return Connection{}, ErrInvalid
	}
	tx, err := s.beginScoped(ctx, caller, scope, projectID)
	if err != nil {
		return Connection{}, err
	}
	defer tx.Rollback(ctx)
	ownerKind, ownerID := scopeOwner(caller, scope, projectID)
	id, err := insertGitConnection(ctx, tx, caller, ownerKind, ownerID, host, username, authMethod, token, secrets)
	if err != nil {
		return Connection{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Connection{}, err
	}
	return s.connectionFor(ctx, caller, id)
}

func insertGitConnection(ctx context.Context, tx pgx.Tx, caller identity.Caller, ownerKind, ownerID, host, username, authMethod string,
	token []byte, secrets *access.SecretStore) (string, error) {
	var providerID, id string
	if err := tx.QueryRow(ctx, `INSERT INTO access_provider_registrations
		(organization_id,id,provider_kind,origin,delivery_modes,state)
		VALUES ($1,gen_random_uuid()::text,'git',$2,ARRAY['native_raw'],'active') RETURNING id`,
		caller.OrganizationID, "https://"+host).Scan(&providerID); err != nil {
		return "", err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO access_connections
		(organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state)
		VALUES ($1,gen_random_uuid()::text,$2,$3,$4,$5,$6,'active') RETURNING id`,
		caller.OrganizationID, ownerKind, ownerID, providerID, username, authMethod).Scan(&id); err != nil {
		return "", err
	}
	if _, err := secrets.RotateTx(ctx, tx, caller.OrganizationID, id, 0, token, nil); err != nil {
		return "", err
	}
	return id, audit(ctx, tx, caller, "access.git_connection.created", id)
}

// CreateCodexSubscriptionAs stores the caller's own Codex ChatGPT login
// (from the device flow or a pasted auth.json). It grants nothing until the
// owner adds a use in a project.
func (s *Store) CreateCodexSubscriptionAs(ctx context.Context, caller identity.Caller, credential []byte,
	secrets *access.SecretStore) (Connection, error) {
	if len(credential) == 0 || secrets == nil {
		return Connection{}, ErrInvalid
	}
	tx, err := s.beginScoped(ctx, caller, ScopePersonal, "")
	if err != nil {
		return Connection{}, err
	}
	defer tx.Rollback(ctx)
	var providerID string
	if err := tx.QueryRow(ctx, `INSERT INTO access_provider_registrations
		(organization_id,id,provider_kind,origin,delivery_modes,state)
		VALUES ($1,gen_random_uuid()::text,'openai','https://api.openai.com',ARRAY['oauth_access'],'active')
		RETURNING id`, caller.OrganizationID).Scan(&providerID); err != nil {
		return Connection{}, err
	}
	connectionID, _, err := access.CreateCodexConnection(ctx, tx, secrets, caller.OrganizationID,
		caller.PrincipalID, providerID, credential)
	if errors.Is(err, access.ErrDenied) {
		return Connection{}, ErrInvalid
	}
	if err != nil {
		return Connection{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE access_connections SET models_checked_at=clock_timestamp()
		WHERE organization_id=$1 AND id=$2`, caller.OrganizationID, connectionID); err != nil {
		return Connection{}, err
	}
	if err := audit(ctx, tx, caller, "access.subscription.connected", connectionID); err != nil {
		return Connection{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Connection{}, err
	}
	return s.connectionFor(ctx, caller, connectionID)
}

func replaceModels(ctx context.Context, tx pgx.Tx, orgID, connectionID string, models []access.CatalogModel, modelsErr string) error {
	if len(modelsErr) > 500 {
		modelsErr = modelsErr[:500]
	}
	if modelsErr == "" {
		if _, err := tx.Exec(ctx, `DELETE FROM access_connection_models WHERE organization_id=$1 AND connection_id=$2`,
			orgID, connectionID); err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, m := range models {
			if seen[m.ID] || !catalogModelID.MatchString(m.ID) || len(seen) >= 2000 {
				continue
			}
			seen[m.ID] = true
			name := m.DisplayName
			if len(name) > 200 || name == "" {
				name = m.ID
			}
			var context *int32
			if m.ContextTokens > 0 && m.ContextTokens < 1<<31 {
				v := int32(m.ContextTokens)
				context = &v
			}
			var capabilities []byte
			if len(m.Capabilities) > 0 && len(m.Capabilities) < 16<<10 && json.Valid(m.Capabilities) {
				capabilities = m.Capabilities
			}
			if _, err := tx.Exec(ctx, `INSERT INTO access_connection_models
				(organization_id,connection_id,model_id,display_name,released_at,context_tokens,capabilities)
				VALUES ($1,$2,$3,$4,$5,$6,$7)`, orgID, connectionID, m.ID, name, m.ReleasedAt, context, capabilities); err != nil {
				return err
			}
		}
	}
	_, err := tx.Exec(ctx, `UPDATE access_connections SET models_checked_at=clock_timestamp(),models_error=NULLIF($3,'')
		WHERE organization_id=$1 AND id=$2`, orgID, connectionID, modelsErr)
	return err
}

// StoreConnectionModels records a refresh. A failed refresh keeps the last
// good list and records the error.
func (s *Store) StoreConnectionModels(ctx context.Context, orgID, connectionID string, models []access.CatalogModel, modelsErr string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := replaceModels(ctx, tx, orgID, connectionID, models, modelsErr); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ConnectionSecret is what the platform itself uses to talk to a provider
// (model listing, Git discovery). The caller must Clear it.
type ConnectionSecret struct {
	OrganizationID, ConnectionID, Kind, Provider, Host, Account string
	Secret                                                      access.Secret
}

// ReadConnectionSecretAs returns a secret for platform-side provider calls
// after the same visibility check as ListConnectionModels.
func (s *Store) ReadConnectionSecretAs(ctx context.Context, caller identity.Caller, connectionID string, secrets *access.SecretStore) (ConnectionSecret, error) {
	if _, err := s.visibleConnection(ctx, caller, connectionID); err != nil {
		return ConnectionSecret{}, err
	}
	return s.ReadConnectionSecret(ctx, caller.OrganizationID, connectionID, secrets)
}

// ReadConnectionSecret is the unauthenticated form for the background model refresh.
func (s *Store) ReadConnectionSecret(ctx context.Context, orgID, connectionID string, secrets *access.SecretStore) (ConnectionSecret, error) {
	if secrets == nil {
		return ConnectionSecret{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ConnectionSecret{}, err
	}
	defer tx.Rollback(ctx)
	out := ConnectionSecret{OrganizationID: orgID, ConnectionID: connectionID}
	var origin, authMethod string
	err = tx.QueryRow(ctx, `SELECT p.provider_kind,p.origin,c.auth_method,c.external_account_id FROM access_connections c
		JOIN access_provider_registrations p ON p.organization_id=c.organization_id AND p.id=c.provider_registration_id
		WHERE c.organization_id=$1 AND c.id=$2 AND c.state='active'`, orgID, connectionID).
		Scan(&out.Provider, &origin, &authMethod, &out.Account)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConnectionSecret{}, ErrNotFound
	}
	if err != nil {
		return ConnectionSecret{}, err
	}
	out.Kind = connectionKind(authMethod, out.Provider)
	if out.Kind == "git" {
		out.Host = strings.TrimPrefix(origin, "https://")
	}
	if out.Kind == "subscription" || out.Provider == access.GitHubOAuthProvider {
		return ConnectionSecret{}, ErrInvalid // Refresh custody reads subscription secrets, never this path.
	}
	out.Secret, err = secrets.ReadCurrent(ctx, tx, orgID, connectionID)
	if err != nil {
		return ConnectionSecret{}, err
	}
	return out, tx.Commit(ctx)
}

// DueModelRefresh lists active API-key connections whose model list is older than age.
func (s *Store) DueModelRefresh(ctx context.Context, age time.Duration, limit int) ([][2]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT organization_id,id FROM access_connections
		WHERE state='active' AND auth_method='api_key'
		AND (models_checked_at IS NULL OR models_checked_at<clock_timestamp()-$1::interval)
		ORDER BY models_checked_at NULLS FIRST LIMIT $2`, age.String(), limit)
	if err != nil {
		return nil, err
	}
	var out [][2]string
	var org, id string
	_, err = pgx.ForEachRow(rows, []any{&org, &id}, func() error {
		out = append(out, [2]string{org, id})
		return nil
	})
	return out, err
}

func connectionKind(authMethod, provider string) string {
	switch {
	case authMethod == access.CodexSubscriptionAuth:
		return "subscription"
	case provider == "git":
		return "git"
	case provider == access.GitHubOAuthProvider:
		return "oauth_app"
	}
	return "api_key"
}

func scopeName(ownerKind string) string {
	switch ownerKind {
	case "organization":
		return ScopeOrganization
	case "project":
		return ScopeProject
	}
	return ScopePersonal
}

// connectionColumns reads one row for connectionRow; never a secret column.
const connectionColumns = `c.id,c.owner_kind,c.owner_id,
	COALESCE(CASE c.owner_kind WHEN 'organization' THEN o.slug WHEN 'project' THEN pr.name ELSE u.username END,''),
	c.auth_method,p.provider_kind,p.origin,c.external_account_id,COALESCE(c.label,''),
	CASE WHEN c.state<>'active' THEN c.state WHEN os.reconnect_reason IS NOT NULL THEN 'reconnect_required' ELSE 'active' END,
	(SELECT max(l.reserved_at) FROM access_leases l WHERE l.organization_id=c.organization_id AND l.connection_id=c.id),
	c.created_at,c.models_checked_at,COALESCE(c.models_error,''),
	(SELECT count(*)::integer FROM access_connection_models m WHERE m.organization_id=c.organization_id AND m.connection_id=c.id)
	FROM access_connections c
	JOIN access_provider_registrations p ON p.organization_id=c.organization_id AND p.id=c.provider_registration_id
	LEFT JOIN access_oauth_sessions os ON os.organization_id=c.organization_id AND os.connection_id=c.id
	LEFT JOIN identity_organizations o ON c.owner_kind='organization' AND o.id::text=c.owner_id
	LEFT JOIN workflow_projects pr ON c.owner_kind='project' AND pr.organization_id::text=c.organization_id AND pr.id::text=c.owner_id
	LEFT JOIN identity_principals u ON c.owner_kind='user' AND u.id::text=c.owner_id`

func scanConnection(row pgx.Row) (Connection, error) {
	var c Connection
	var ownerKind, authMethod, provider, origin string
	err := row.Scan(&c.ID, &ownerKind, &c.OwnerID, &c.OwnerName, &authMethod, &provider, &origin, &c.Account, &c.Label,
		&c.State, &c.LastUsed, &c.CreatedAt, &c.ModelsCheckedAt, &c.ModelsError, &c.ModelCount)
	if err != nil {
		return c, err
	}
	c.Scope, c.Kind, c.Provider = scopeName(ownerKind), connectionKind(authMethod, provider), provider
	switch c.Kind {
	case "git":
		c.Provider = strings.TrimSuffix(strings.TrimPrefix(origin, "https://"), ".com")
	case "subscription":
		c.Provider = "codex"
	case "api_key":
		c.Account = ""
	}
	return c, nil
}

// ListConnectionsAs returns connections at scope with their grants and uses.
func (s *Store) ListConnectionsAs(ctx context.Context, caller identity.Caller, scope, projectID string) ([]Connection, error) {
	if !ids(caller.OrganizationID, caller.PrincipalID) || (projectID != "" && !ids(projectID)) {
		return nil, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	org := caller.OrganizationID
	var where string
	args := []any{org}
	manage := true
	switch scope {
	case ScopeOrganization:
		if caller.Role != "owner" && caller.Role != "admin" {
			return nil, ErrConnectionDenied
		}
		where = `c.owner_kind='organization' AND p.provider_kind<>'` + access.GitHubOAuthProvider + `'`
	case ScopeProject:
		if projectID == "" {
			return nil, ErrInvalid
		}
		if err := s.requireProjectAdmin(ctx, tx, caller, projectID); err != nil {
			return nil, err
		}
		where, args = `c.owner_kind='project' AND c.owner_id=$2`, append(args, projectID)
	case ScopePersonal:
		where, args = `c.owner_kind='user' AND c.owner_id=$2`, append(args, caller.PrincipalID)
	case ScopeProjectAvailable:
		if projectID == "" {
			return nil, ErrInvalid
		}
		manage = false
		where = `c.owner_kind='organization' AND c.state='active' AND EXISTS (SELECT 1 FROM access_resource_grants g
			WHERE g.organization_id::text=c.organization_id AND g.resource_kind='connection' AND g.resource_id=c.id
			AND g.revoked_at IS NULL)`
	default:
		return nil, ErrInvalid
	}
	rows, err := tx.Query(ctx, `SELECT `+connectionColumns+` WHERE c.organization_id=$1 AND `+where+`
		ORDER BY c.state='active' DESC,c.created_at DESC,c.id LIMIT 200`, args...)
	if err != nil {
		return nil, err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Connection, error) { return scanConnection(row) })
	if err != nil {
		return nil, err
	}
	out := items[:0]
	for _, c := range items {
		if scope == ScopeProjectAvailable {
			ok, err := s.authz.CanUse(ctx, tx, caller, projectID, access.ResourceConnection, c.ID)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		c.CanManage = manage
		out = append(out, c)
	}
	return out, s.attachGrantsAndUses(ctx, tx, org, scope, projectID, out)
}

func (s *Store) attachGrantsAndUses(ctx context.Context, tx pgx.Tx, org, scope, projectID string, items []Connection) error {
	if len(items) == 0 {
		return nil
	}
	index := map[string]int{}
	idList := make([]string, len(items))
	for i, c := range items {
		index[c.ID], idList[i] = i, c.ID
	}
	project := ""
	if scope == ScopeProjectAvailable {
		project = projectID // A project page sees only its own grants and uses.
	}
	var id, connectionID, pid, pname, kind, grantee, username, resource string
	var created time.Time
	if project == "" {
		// Standing grants live in lane U's access_resource_grants: exactly one
		// of a project, a principal, or a minimum role.
		rows, err := tx.Query(ctx, `SELECT g.id::text,g.resource_id,COALESCE(g.grantee_project_id::text,''),COALESCE(pr.name,''),
			CASE WHEN g.grantee_project_id IS NOT NULL THEN 'project' WHEN g.grantee_principal_id IS NOT NULL THEN 'user' ELSE 'role' END,
			COALESCE(g.grantee_principal_id::text,g.grantee_role,''),COALESCE(u.username,''),g.created_at
			FROM access_resource_grants g
			LEFT JOIN workflow_projects pr ON pr.organization_id=g.organization_id AND pr.id=g.grantee_project_id
			LEFT JOIN identity_principals u ON u.id=g.grantee_principal_id
			WHERE g.organization_id=$1::uuid AND g.resource_kind='connection' AND g.resource_id=ANY($2) AND g.revoked_at IS NULL
			ORDER BY g.created_at,g.id LIMIT 2000`, org, idList)
		if err != nil {
			return err
		}
		if _, err := pgx.ForEachRow(rows, []any{&id, &connectionID, &pid, &pname, &kind, &grantee, &username, &created}, func() error {
			c := &items[index[connectionID]]
			c.Grants = append(c.Grants, ConnectionGrant{ID: id, ProjectID: pid, ProjectName: pname, GranteeKind: kind,
				GranteeID: grantee, GranteeName: username, CreatedAt: created})
			return nil
		}); err != nil {
			return err
		}
	}
	rows, err := tx.Query(ctx, `SELECT g.id,g.connection_id,g.project_id,COALESCE(pr.name,''),g.grantee_kind,g.resource,g.created_at
		FROM access_grants g
		LEFT JOIN workflow_projects pr ON pr.organization_id::text=g.organization_id AND pr.id::text=g.project_id
		WHERE g.organization_id=$1 AND g.connection_id=ANY($2) AND g.revoked_at IS NULL
		AND g.capability='model.invoke' AND ($3='' OR g.project_id=$3)
		ORDER BY g.created_at,g.id LIMIT 2000`, org, idList, project)
	if err != nil {
		return err
	}
	_, err = pgx.ForEachRow(rows, []any{&id, &connectionID, &pid, &pname, &kind, &resource, &created}, func() error {
		c := &items[index[connectionID]]
		_, model, _ := strings.Cut(resource, "/")
		c.Uses = append(c.Uses, ConnectionUse{ID: id, ProjectID: pid, ProjectName: pname, Model: model, GranteeKind: kind, CreatedAt: created})
		return nil
	})
	return err
}

func (s *Store) connectionFor(ctx context.Context, caller identity.Caller, connectionID string) (Connection, error) {
	c, err := scanConnection(s.pool.QueryRow(ctx, `SELECT `+connectionColumns+` WHERE c.organization_id=$1 AND c.id=$2`,
		caller.OrganizationID, connectionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Connection{}, ErrNotFound
	}
	c.CanManage = true
	return c, err
}

// connectionRow is the authority-relevant view of one connection.
type connectionRow struct {
	ID, OwnerKind, OwnerID, AuthMethod, Provider, State string
}

func lockConnection(ctx context.Context, tx pgx.Tx, orgID, connectionID string, update bool) (connectionRow, error) {
	lock := "FOR SHARE OF c"
	if update {
		lock = "FOR UPDATE OF c"
	}
	var r connectionRow
	err := tx.QueryRow(ctx, `SELECT c.id,c.owner_kind,c.owner_id,c.auth_method,p.provider_kind,c.state
		FROM access_connections c JOIN access_provider_registrations p
			ON p.organization_id=c.organization_id AND p.id=c.provider_registration_id
		WHERE c.organization_id=$1 AND c.id=$2 `+lock, orgID, connectionID).
		Scan(&r.ID, &r.OwnerKind, &r.OwnerID, &r.AuthMethod, &r.Provider, &r.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// visibleConnection: org owners/admins see organization and project
// connections; project admins see their project's; anyone sees their own
// personal ones and organization connections granted to them in a project.
func (s *Store) visibleConnection(ctx context.Context, caller identity.Caller, connectionID string) (connectionRow, error) {
	if !ids(caller.OrganizationID, caller.PrincipalID) || connectionID == "" || len(connectionID) > 64 {
		return connectionRow{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return connectionRow{}, err
	}
	defer tx.Rollback(ctx)
	r, err := lockConnection(ctx, tx, caller.OrganizationID, connectionID, false)
	if err != nil {
		return r, err
	}
	admin := caller.Role == "owner" || caller.Role == "admin"
	switch r.OwnerKind {
	case "user":
		if r.OwnerID == caller.PrincipalID {
			return r, nil
		}
	case "project":
		if ok, err := s.authz.CanAdministerProject(ctx, tx, caller, r.OwnerID); err != nil || ok {
			return r, err
		}
	case "organization":
		if admin && r.Provider != access.GitHubOAuthProvider {
			return r, nil
		}
		if r.Provider == access.GitHubOAuthProvider {
			break
		}
		rows, err := tx.Query(ctx, `SELECT DISTINCT COALESCE(grantee_project_id::text,'') FROM access_resource_grants
			WHERE organization_id=$1::uuid AND resource_kind='connection' AND resource_id=$2 AND revoked_at IS NULL LIMIT 200`,
			caller.OrganizationID, connectionID)
		if err != nil {
			return r, err
		}
		projects, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return r, err
		}
		for _, project := range projects {
			if ok, err := s.authz.CanUse(ctx, tx, caller, project, access.ResourceConnection, connectionID); err != nil || ok {
				return r, err
			}
		}
	}
	return connectionRow{}, ErrNotFound
}

// ListConnectionModelsAs returns the connection's stored model list,
// optionally only models a pinned harness accepts.
func (s *Store) ListConnectionModelsAs(ctx context.Context, caller identity.Caller, connectionID, harness string) ([]ConnectionModel, *time.Time, string, error) {
	r, err := s.visibleConnection(ctx, caller, connectionID)
	if err != nil {
		return nil, nil, "", err
	}
	if harness != "" && !knownHarness(harness) {
		return nil, nil, "", ErrInvalid
	}
	var checked *time.Time
	var modelsErr string
	if err := s.pool.QueryRow(ctx, `SELECT models_checked_at,COALESCE(models_error,'') FROM access_connections
		WHERE organization_id=$1 AND id=$2`, caller.OrganizationID, connectionID).Scan(&checked, &modelsErr); err != nil {
		return nil, nil, "", err
	}
	var rows pgx.Rows
	if r.AuthMethod == access.CodexSubscriptionAuth {
		// A ChatGPT plan has no public model list; offer the models the
		// organization approved for the pinned Codex runtime.
		rows, err = s.pool.Query(ctx, `SELECT DISTINCT model,model,NULL::timestamptz,NULL::integer,''
			FROM workflow_tool_runtime_approvals WHERE organization_id=$1 AND harness='codex' AND revoked_at IS NULL
			ORDER BY 1`, caller.OrganizationID)
	} else {
		rows, err = s.pool.Query(ctx, `SELECT model_id,display_name,released_at,context_tokens,COALESCE(capabilities::text,'')
			FROM access_connection_models WHERE organization_id=$1 AND connection_id=$2
			ORDER BY released_at DESC NULLS LAST,model_id`, caller.OrganizationID, connectionID)
	}
	if err != nil {
		return nil, nil, "", err
	}
	all, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ConnectionModel, error) {
		var m ConnectionModel
		var context *int32
		err := row.Scan(&m.ID, &m.DisplayName, &m.ReleasedAt, &context, &m.Capabilities)
		if context != nil {
			m.ContextTokens = *context
		}
		return m, err
	})
	if err != nil {
		return nil, nil, "", err
	}
	out := all[:0]
	for _, m := range all {
		m.Harnesses = HarnessesFor(r.Provider, m.ID)
		if harness == "" || slices.Contains(m.Harnesses, harness) {
			out = append(out, m)
		}
	}
	return out, checked, modelsErr, nil
}

// ensurePolicyMode makes mode part of the project policy when allowed to,
// and otherwise requires it already to be there.
func ensurePolicyMode(ctx context.Context, tx pgx.Tx, orgID, projectID, mode string, mayAdd bool) error {
	if mayAdd {
		_, err := tx.Exec(ctx, `INSERT INTO access_project_policies
			(organization_id,project_id,version,git_read_enabled,delivery_modes)
			VALUES ($1,$2,1,false,ARRAY[$3])
			ON CONFLICT (organization_id,project_id) DO UPDATE SET
			version=CASE WHEN $3=ANY(access_project_policies.delivery_modes)
				THEN access_project_policies.version ELSE access_project_policies.version+1 END,
			delivery_modes=CASE WHEN $3=ANY(access_project_policies.delivery_modes)
				THEN access_project_policies.delivery_modes
				ELSE array_append(access_project_policies.delivery_modes,$3) END`, orgID, projectID, mode)
		return err
	}
	var ok bool
	err := tx.QueryRow(ctx, `SELECT true FROM access_project_policies WHERE organization_id=$1
		AND project_id=$2 AND $3=ANY(delivery_modes) FOR SHARE`, orgID, projectID, mode).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSubscriptionPolicy
	}
	return err
}

// AddConnectionUseAs lets a project use connectionID for one model. An
// organization or project connection becomes a workload grant plus the
// project's model selection; a personal connection becomes a grant to its
// owner, honoured only for runs the owner launches.
func (s *Store) AddConnectionUseAs(ctx context.Context, caller identity.Caller, connectionID, projectID, model string) (ConnectionUse, error) {
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID, projectID) || connectionID == "" ||
		len(connectionID) > 64 || !projectModelName.MatchString(model) {
		return ConnectionUse{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ConnectionUse{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockCallerSession(ctx, tx, caller, true); err != nil {
		return ConnectionUse{}, err
	}
	var projectName string
	err = tx.QueryRow(ctx, `SELECT name FROM workflow_projects WHERE organization_id=$1 AND id=$2 FOR SHARE`,
		caller.OrganizationID, projectID).Scan(&projectName)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConnectionUse{}, ErrNotFound
	}
	if err != nil {
		return ConnectionUse{}, err
	}
	r, err := lockConnection(ctx, tx, caller.OrganizationID, connectionID, false)
	if err != nil {
		return ConnectionUse{}, err
	}
	if r.State != "active" || access.ModelOrigin(r.Provider) == "" {
		return ConnectionUse{}, ErrInvalid
	}
	projectAdmin, err := s.authz.CanAdministerProject(ctx, tx, caller, projectID)
	if err != nil {
		return ConnectionUse{}, err
	}
	resource := r.Provider + "/" + model
	use := ConnectionUse{ProjectID: projectID, ProjectName: projectName, Model: model}
	if r.OwnerKind == "user" {
		if r.OwnerID != caller.PrincipalID {
			return ConnectionUse{}, ErrNotFound
		}
		mode := "native_raw"
		if r.AuthMethod == access.CodexSubscriptionAuth {
			mode = "oauth_access"
		}
		if err := ensurePolicyMode(ctx, tx, caller.OrganizationID, projectID, mode, projectAdmin); err != nil {
			return ConnectionUse{}, err
		}
		err = tx.QueryRow(ctx, `INSERT INTO access_grants
			(organization_id,id,connection_id,project_id,grantee_kind,grantee_id,capability,resource,delivery_mode,issuer_id)
			SELECT $1,gen_random_uuid()::text,$2,$3,'user',$4,'model.invoke',$5,$6,$4
			WHERE NOT EXISTS (SELECT 1 FROM access_grants WHERE organization_id=$1 AND connection_id=$2 AND project_id=$3
				AND grantee_kind='user' AND grantee_id=$4 AND capability='model.invoke' AND resource=$5 AND revoked_at IS NULL)
			RETURNING id,created_at`, caller.OrganizationID, connectionID, projectID, caller.PrincipalID, resource, mode).
			Scan(&use.ID, &use.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ConnectionUse{}, ErrConflict
		}
		if err != nil {
			return ConnectionUse{}, err
		}
		use.GranteeKind = "user"
		return use, commitAudited(ctx, tx, caller, "access.connection.use_added", use.ID)
	}
	if !projectAdmin {
		return ConnectionUse{}, ErrConnectionDenied
	}
	switch r.OwnerKind {
	case "project":
		if r.OwnerID != projectID {
			return ConnectionUse{}, ErrNotFound
		}
	case "organization":
		ok, err := s.authz.CanUse(ctx, tx, caller, projectID, access.ResourceConnection, connectionID)
		if err != nil {
			return ConnectionUse{}, err
		}
		if !ok {
			return ConnectionUse{}, ErrNotFound
		}
	default:
		return ConnectionUse{}, ErrNotFound
	}
	if err := ensurePolicyMode(ctx, tx, caller.OrganizationID, projectID, "native_raw", true); err != nil {
		return ConnectionUse{}, err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO access_grants
		(organization_id,id,connection_id,project_id,grantee_kind,grantee_id,capability,resource,delivery_mode,issuer_id)
		VALUES ($1,gen_random_uuid()::text,$2,$3,'workload',$4,'model.invoke',$5,'native_raw',$6)
		RETURNING id,created_at`, caller.OrganizationID, connectionID, projectID, access.DispatcherGrantee, resource,
		caller.PrincipalID).Scan(&use.ID, &use.CreatedAt); err != nil {
		return ConnectionUse{}, err
	}
	var selection string
	err = tx.QueryRow(ctx, `INSERT INTO workflow_project_model_grants
		(organization_id,project_id,provider,model,grant_id,grantee_id,approved_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (organization_id,project_id,provider,model) WHERE revoked_at IS NULL DO NOTHING
		RETURNING id`, caller.OrganizationID, projectID, r.Provider, model, use.ID, access.DispatcherGrantee,
		caller.PrincipalID).Scan(&selection)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConnectionUse{}, ErrConflict
	}
	if err != nil {
		return ConnectionUse{}, err
	}
	use.GranteeKind = "workload"
	return use, commitAudited(ctx, tx, caller, "access.connection.use_added", use.ID)
}

// revokeGrants revokes grants matching where (with $1 the organization),
// their project model selections, and every lease bound to them.
func revokeGrants(ctx context.Context, tx pgx.Tx, where string, args ...any) error {
	rows, err := tx.Query(ctx, `UPDATE access_grants g SET revoked_at=clock_timestamp(),version=version+1
		WHERE g.organization_id=$1 AND g.revoked_at IS NULL AND `+where+` RETURNING g.id`, args...)
	if err != nil {
		return err
	}
	revoked, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(revoked) == 0 {
		return err
	}
	org := args[0]
	if _, err := tx.Exec(ctx, `UPDATE workflow_project_model_grants SET revoked_at=clock_timestamp()
		WHERE organization_id::text=$1 AND grant_id=ANY($2) AND revoked_at IS NULL`, org, revoked); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE access_leases SET revoked_at=clock_timestamp() WHERE organization_id=$1
		AND revoked_at IS NULL AND binding_id IN (SELECT id FROM access_bindings WHERE organization_id=$1 AND grant_id=ANY($2))`,
		org, revoked)
	return err
}

// RemoveConnectionUseAs revokes one model use (a model.invoke grant).
func (s *Store) RemoveConnectionUseAs(ctx context.Context, caller identity.Caller, useID string) error {
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID) || useID == "" || len(useID) > 64 {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockCallerSession(ctx, tx, caller, true); err != nil {
		return err
	}
	var projectID, connectionID string
	err = tx.QueryRow(ctx, `SELECT project_id,connection_id FROM access_grants WHERE organization_id=$1 AND id=$2
		AND capability='model.invoke' FOR UPDATE`, caller.OrganizationID, useID).Scan(&projectID, &connectionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	r, err := lockConnection(ctx, tx, caller.OrganizationID, connectionID, false)
	if err != nil {
		return err
	}
	if r.OwnerKind == "user" {
		if r.OwnerID != caller.PrincipalID {
			return ErrNotFound
		}
	} else if err := s.requireProjectAdmin(ctx, tx, caller, projectID); err != nil {
		return err
	}
	if err := revokeGrants(ctx, tx, `g.id=$2`, caller.OrganizationID, useID); err != nil {
		return err
	}
	return commitAudited(ctx, tx, caller, "access.connection.use_removed", useID)
}

// GrantConnectionAs records a lane U resource grant on an organization
// connection: to a project, a member, or a minimum role (member, admin,
// owner). Organization owners/admins only.
func (s *Store) GrantConnectionAs(ctx context.Context, caller identity.Caller, connectionID, projectID, granteeKind, granteeID string) (ConnectionGrant, error) {
	if caller.Role != "owner" && caller.Role != "admin" {
		return ConnectionGrant{}, ErrConnectionDenied
	}
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID) || connectionID == "" || len(connectionID) > 64 {
		return ConnectionGrant{}, ErrInvalid
	}
	var to access.Grantee
	switch granteeKind {
	case "project":
		to.ProjectID = projectID
	case "user":
		to.PrincipalID = granteeID
	case "role":
		to.Role = granteeID
	default:
		return ConnectionGrant{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ConnectionGrant{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockCallerSession(ctx, tx, caller, false); err != nil {
		return ConnectionGrant{}, err
	}
	r, err := lockConnection(ctx, tx, caller.OrganizationID, connectionID, false)
	if err != nil {
		return ConnectionGrant{}, err
	}
	if r.OwnerKind != "organization" || r.State != "active" || r.Provider == access.GitHubOAuthProvider {
		return ConnectionGrant{}, ErrInvalid
	}
	id, err := access.GrantResource(ctx, tx, caller, access.ResourceConnection, connectionID, to)
	if errors.Is(err, access.ErrResourceGrantInvalid) {
		return ConnectionGrant{}, ErrInvalid
	}
	if errors.Is(err, access.ErrDenied) {
		return ConnectionGrant{}, ErrConnectionDenied
	}
	if err != nil {
		return ConnectionGrant{}, err
	}
	grant := ConnectionGrant{ID: id, GranteeKind: granteeKind}
	err = tx.QueryRow(ctx, `SELECT COALESCE(g.grantee_project_id::text,''),COALESCE(pr.name,''),
		COALESCE(g.grantee_principal_id::text,g.grantee_role,''),COALESCE(u.username,''),g.created_at
		FROM access_resource_grants g
		LEFT JOIN workflow_projects pr ON pr.organization_id=g.organization_id AND pr.id=g.grantee_project_id
		LEFT JOIN identity_principals u ON u.id=g.grantee_principal_id
		WHERE g.organization_id=$1::uuid AND g.id=$2::uuid`, caller.OrganizationID, id).
		Scan(&grant.ProjectID, &grant.ProjectName, &grant.GranteeID, &grant.GranteeName, &grant.CreatedAt)
	if err != nil {
		return ConnectionGrant{}, err
	}
	return grant, tx.Commit(ctx)
}

// RevokeConnectionGrantAs revokes a standing grant on an organization
// connection. Revoking a project grant also revokes what that project built
// on it: its model uses, Git grants, selections, and leases. A user or role
// grant only gates who may attach the connection; existing uses stay until
// removed.
func (s *Store) RevokeConnectionGrantAs(ctx context.Context, caller identity.Caller, grantID string) error {
	if caller.Role != "owner" && caller.Role != "admin" {
		return ErrConnectionDenied
	}
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID, grantID) {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockCallerSession(ctx, tx, caller, false); err != nil {
		return err
	}
	var connectionID, projectID string
	err = tx.QueryRow(ctx, `SELECT resource_id,COALESCE(grantee_project_id::text,'') FROM access_resource_grants
		WHERE organization_id=$1::uuid AND id=$2::uuid AND resource_kind='connection' AND revoked_at IS NULL FOR UPDATE`,
		caller.OrganizationID, grantID).Scan(&connectionID, &projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := access.RevokeResourceGrant(ctx, tx, caller, grantID); err != nil {
		return err
	}
	if projectID != "" {
		if err := revokeGrants(ctx, tx, `g.connection_id=$2 AND g.project_id=$3 AND g.grantee_kind='workload'`,
			caller.OrganizationID, connectionID, projectID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// RevokeConnectionAs disables a connection and everything granted from it.
// Org owners/admins may disable (never read) any connection in their org.
func (s *Store) RevokeConnectionAs(ctx context.Context, caller identity.Caller, connectionID string) error {
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID) || connectionID == "" || len(connectionID) > 64 {
		return ErrInvalid
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
	admin := caller.Role == "owner" || caller.Role == "admin"
	switch r.OwnerKind {
	case "organization":
		if !admin {
			return ErrConnectionDenied
		}
	case "project":
		if err := s.requireProjectAdmin(ctx, tx, caller, r.OwnerID); err != nil {
			return err
		}
	default:
		if r.OwnerID != caller.PrincipalID && !admin {
			return ErrNotFound
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE access_connections SET state='revoked' WHERE organization_id=$1 AND id=$2`,
		caller.OrganizationID, connectionID); err != nil {
		return err
	}
	if err := revokeGrants(ctx, tx, `g.connection_id=$2`, caller.OrganizationID, connectionID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE access_resource_grants SET revoked_at=clock_timestamp() WHERE organization_id=$1::uuid
		AND resource_kind='connection' AND resource_id=$2 AND revoked_at IS NULL`, caller.OrganizationID, connectionID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE access_leases SET revoked_at=clock_timestamp()
		WHERE organization_id=$1 AND connection_id=$2 AND revoked_at IS NULL`, caller.OrganizationID, connectionID); err != nil {
		return err
	}
	return commitAudited(ctx, tx, caller, "access.connection.revoked", connectionID)
}
