package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/extension"
	"github.com/mjtechguy/blaxsmith/internal/identity"
)

// ErrExtensionDenied means the caller may not install or change extensions.
var ErrExtensionDenied = errors.New("extension administration denied")

// ExtensionInvalidError carries a manifest validation failure.
type ExtensionInvalidError struct{ Field *extension.Error }

func (e *ExtensionInvalidError) Error() string { return e.Field.Error() }

type Extension struct {
	ID, Key, RepositoryURL, GitRef   string
	CurrentVersionID, CurrentVersion string
	CurrentCommit, LatestRefCommit   string
	VersionCount                     int32
	LatestCheckedAt                  *time.Time
	CreatedAt, UpdatedAt             time.Time
}

// UpdateAvailable reports that the installed ref now resolves elsewhere.
func (e Extension) UpdateAvailable() bool {
	return e.LatestRefCommit != "" && e.CurrentCommit != "" && e.LatestRefCommit != e.CurrentCommit
}

type ExtensionVersion struct {
	ID, ExtensionID, Version, RepositoryURL, GitRef, Commit string
	Manifest                                                []byte
	ManifestSHA256, ManifestOrigin, ManifestPath            string
	Approved                                                []string
	PermissionsSHA256                                       string
	InstalledBy, InstalledByUsername                        string
	CreatedAt                                               time.Time
}

// ExtensionSource is a fetched repository at one resolved commit.
type ExtensionSource struct {
	RepositoryURL, GitRef, Commit string
	Directory                     string // Local Git repository containing Commit.
	// ManifestPath is read from the commit unless Overlay is set.
	ManifestPath string
	Overlay      []byte
}

// ExtensionPreview is what an administrator approves.
type ExtensionPreview struct {
	Manifest       extension.Manifest
	ManifestJSON   []byte
	ManifestSHA256 string
	Commit         string
	Permissions    []extension.Permission
}

// PreviewExtension validates a fetched source without persisting anything.
func (s *Store) PreviewExtension(ctx context.Context, caller identity.Caller, src ExtensionSource) (ExtensionPreview, error) {
	if err := requireExtensionAdmin(caller); err != nil {
		return ExtensionPreview{}, err
	}
	return previewExtension(ctx, src)
}

func previewExtension(ctx context.Context, src ExtensionSource) (ExtensionPreview, error) {
	if src.Directory == "" || src.Commit == "" || src.RepositoryURL == "" || src.GitRef == "" || len(src.GitRef) > 128 {
		return ExtensionPreview{}, ErrInvalid
	}
	tree, err := extension.OpenGitTree(ctx, src.Directory, src.Commit)
	if err != nil {
		return ExtensionPreview{}, fmt.Errorf("read extension commit: %w", err)
	}
	data := src.Overlay
	if data == nil {
		if src.ManifestPath == "" {
			src.ManifestPath = extension.ManifestPath
		}
		if !extension.ValidPath(src.ManifestPath) || !tree.IsFile(src.ManifestPath) {
			return ExtensionPreview{}, &ExtensionInvalidError{&extension.Error{Path: "$",
				Message: fmt.Sprintf("%s is not committed at %s; install with an overlay manifest", src.ManifestPath, src.Commit)}}
		}
		if data, err = tree.Read(src.ManifestPath); err != nil {
			return ExtensionPreview{}, err
		}
	}
	m, perr := extension.Parse(data)
	if perr != nil {
		return ExtensionPreview{}, &ExtensionInvalidError{perr}
	}
	if perr := extension.ValidateTree(m, tree); perr != nil {
		return ExtensionPreview{}, &ExtensionInvalidError{perr}
	}
	return ExtensionPreview{Manifest: m, ManifestJSON: slices.Clone(data), ManifestSHA256: extension.Digest(data),
		Commit: src.Commit, Permissions: m.Permissions()}, nil
}

func requireExtensionAdmin(caller identity.Caller) error {
	if caller.Role != "owner" && caller.Role != "admin" {
		return ErrExtensionDenied
	}
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID) {
		return ErrFenced
	}
	return nil
}

// InstallExtensionAs validates src again, checks the approval, and records a
// new immutable version as the extension's current one. The same version
// string at the same commit and manifest is idempotent; at a different commit
// or manifest it conflicts, so extension@version always names one install.
func (s *Store) InstallExtensionAs(ctx context.Context, caller identity.Caller, src ExtensionSource, approved []string) (Extension, ExtensionVersion, error) {
	if err := requireExtensionAdmin(caller); err != nil {
		return Extension{}, ExtensionVersion{}, err
	}
	preview, err := previewExtension(ctx, src)
	if err != nil {
		return Extension{}, ExtensionVersion{}, err
	}
	sorted, err := preview.Manifest.Approve(approved)
	if err != nil {
		return Extension{}, ExtensionVersion{}, err
	}
	origin, manifestPath := "overlay", ""
	if src.Overlay == nil {
		origin, manifestPath = "repository", src.ManifestPath
		if manifestPath == "" {
			manifestPath = extension.ManifestPath
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Extension{}, ExtensionVersion{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockCallerSession(ctx, tx, caller, false); err != nil {
		return Extension{}, ExtensionVersion{}, err
	}
	org := caller.OrganizationID
	var extensionID, repositoryURL string
	err = tx.QueryRow(ctx, `INSERT INTO workflow_extensions (organization_id,extension_key,repository_url,git_ref,created_by)
		VALUES ($1,$2,$3,$4,$5) ON CONFLICT (organization_id,extension_key) DO UPDATE SET updated_at=workflow_extensions.updated_at
		RETURNING id::text,repository_url`, org, preview.Manifest.ID, src.RepositoryURL, src.GitRef, caller.PrincipalID).
		Scan(&extensionID, &repositoryURL)
	if err != nil {
		return Extension{}, ExtensionVersion{}, err
	}
	if repositoryURL != src.RepositoryURL {
		return Extension{}, ExtensionVersion{}, ErrConflict // One extension id, one repository.
	}
	var versionID, existingCommit, existingSHA string
	err = tx.QueryRow(ctx, `SELECT id::text,commit_sha,manifest_sha256 FROM workflow_extension_versions
		WHERE organization_id=$1 AND extension_id=$2 AND version=$3`, org, extensionID, preview.Manifest.Version).
		Scan(&versionID, &existingCommit, &existingSHA)
	switch {
	case err == nil:
		if existingCommit != src.Commit || existingSHA != preview.ManifestSHA256 {
			return Extension{}, ExtensionVersion{}, ErrConflict
		}
	case errors.Is(err, pgx.ErrNoRows):
		if err := tx.QueryRow(ctx, `INSERT INTO workflow_extension_versions
			(organization_id,extension_id,version,repository_url,git_ref,commit_sha,manifest_json,manifest_sha256,
			 manifest_origin,manifest_path,approved_permissions,permissions_sha256,installed_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,encode(sha256($7),'hex'),$8,$9,$10,$11,$12) RETURNING id::text`,
			org, extensionID, preview.Manifest.Version, src.RepositoryURL, src.GitRef, src.Commit, preview.ManifestJSON,
			origin, manifestPath, sorted, extension.PermissionsDigest(sorted), caller.PrincipalID).Scan(&versionID); err != nil {
			return Extension{}, ExtensionVersion{}, fmt.Errorf("insert extension version: %w", err)
		}
		detail, _ := json.Marshal(map[string]any{"version": preview.Manifest.Version, "commit": src.Commit,
			"manifest_sha256": preview.ManifestSHA256, "approved_permissions": sorted})
		if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events (organization_id,actor_kind,actor_id,action,subject_id,detail)
			VALUES ($1,'principal',$2,'workflow.extension.installed',$3,$4)`, org, caller.PrincipalID, versionID, detail); err != nil {
			return Extension{}, ExtensionVersion{}, err
		}
	default:
		return Extension{}, ExtensionVersion{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_extensions SET current_version_id=$3,git_ref=$4,latest_ref_commit=$5,
		latest_checked_at=clock_timestamp(),updated_at=clock_timestamp() WHERE organization_id=$1 AND id=$2`,
		org, extensionID, versionID, src.GitRef, src.Commit); err != nil {
		return Extension{}, ExtensionVersion{}, err
	}
	e, err := getExtension(ctx, tx, org, extensionID)
	if err != nil {
		return Extension{}, ExtensionVersion{}, err
	}
	v, err := getExtensionVersion(ctx, tx, org, versionID)
	if err != nil {
		return Extension{}, ExtensionVersion{}, err
	}
	return e, v, tx.Commit(ctx)
}

// RecordExtensionRefAs stores the latest resolution of an extension's ref,
// which drives "update available".
func (s *Store) RecordExtensionRefAs(ctx context.Context, caller identity.Caller, extensionID, commit string) (Extension, error) {
	if err := requireExtensionAdmin(caller); err != nil {
		return Extension{}, err
	}
	if !ids(extensionID) {
		return Extension{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Extension{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockCallerSession(ctx, tx, caller, false); err != nil {
		return Extension{}, err
	}
	tag, err := tx.Exec(ctx, `UPDATE workflow_extensions SET latest_ref_commit=$3,latest_checked_at=clock_timestamp()
		WHERE organization_id=$1 AND id=$2`, caller.OrganizationID, extensionID, commit)
	if err != nil {
		return Extension{}, err
	}
	if tag.RowsAffected() != 1 {
		return Extension{}, ErrNotFound
	}
	e, err := getExtension(ctx, tx, caller.OrganizationID, extensionID)
	if err != nil {
		return Extension{}, err
	}
	return e, tx.Commit(ctx)
}

// ListExtensions returns the organization's extensions to any active member.
// With projectID, it returns only those the caller may use in that project.
func (s *Store) ListExtensions(ctx context.Context, caller identity.Caller, projectID string) ([]Extension, error) {
	if !ids(caller.OrganizationID) || (projectID != "" && !ids(projectID)) {
		return nil, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx) // Not read-only: access.CanUse locks grant rows FOR SHARE.
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if projectID != "" {
		if err := projectExists(ctx, tx, caller.OrganizationID, projectID); err != nil {
			return nil, err
		}
	}
	rows, err := tx.Query(ctx, `SELECT `+extensionColumns+` WHERE e.organization_id=$1 ORDER BY e.extension_key LIMIT 200`,
		caller.OrganizationID)
	if err != nil {
		return nil, err
	}
	all, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Extension, error) { return scanExtension(row) })
	if err != nil || projectID == "" {
		return all, err
	}
	out := make([]Extension, 0, len(all))
	for _, e := range all {
		ok, err := s.authz.CanUse(ctx, tx, caller, projectID, access.ResourceExtension, e.ID)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, e)
		}
	}
	return out, nil
}

// GetExtension returns one extension and its versions, newest first.
func (s *Store) GetExtension(ctx context.Context, caller identity.Caller, extensionID string) (Extension, []ExtensionVersion, error) {
	if !ids(caller.OrganizationID, extensionID) {
		return Extension{}, nil, ErrNotFound
	}
	e, err := getExtension(ctx, s.pool, caller.OrganizationID, extensionID)
	if err != nil {
		return Extension{}, nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+extensionVersionColumns+` WHERE v.organization_id=$1 AND v.extension_id=$2
		ORDER BY v.created_at DESC,v.id LIMIT 200`, caller.OrganizationID, extensionID)
	if err != nil {
		return Extension{}, nil, err
	}
	versions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ExtensionVersion, error) { return scanExtensionVersion(row) })
	return e, versions, err
}

// ExtensionResolver resolves extension@version references for one launch
// and remembers each version row it returned, for provenance and grants.
type ExtensionResolver struct {
	store    *Store
	org      string
	resolved map[string]string // "id@version" -> version row id
	grantIDs map[string]string // "id@version" -> extension row id
}

func (s *Store) extensionResolver(org string) *ExtensionResolver {
	return &ExtensionResolver{store: s, org: org, resolved: map[string]string{}, grantIDs: map[string]string{}}
}

// Resolve returns the installed, approved version of id@version.
func (r *ExtensionResolver) Resolve(ctx context.Context, id, version string) (extension.Pin, error) {
	var pin extension.Pin
	var versionID, extensionID string
	err := r.store.pool.QueryRow(ctx, `SELECT v.id::text,e.id::text,v.repository_url,v.commit_sha,v.manifest_json,v.approved_permissions
		FROM workflow_extension_versions v JOIN workflow_extensions e ON e.organization_id=v.organization_id AND e.id=v.extension_id
		WHERE v.organization_id=$1 AND e.extension_key=$2 AND v.version=$3`, r.org, id, version).
		Scan(&versionID, &extensionID, &pin.RepositoryURL, &pin.Commit, &pin.Manifest, &pin.Approved)
	if errors.Is(err, pgx.ErrNoRows) {
		return pin, fmt.Errorf("extension %s@%s is not installed", id, version)
	}
	if err != nil {
		return pin, err
	}
	pin.ID, pin.Version = id, version
	r.resolved[id+"@"+version], r.grantIDs[id+"@"+version] = versionID, extensionID
	return pin, nil
}

// recordRunExtensions checks that the launching caller may use every frozen
// extension in the project and records the versions against the run.
func (r *ExtensionResolver) recordRunExtensions(ctx context.Context, tx pgx.Tx, caller *identity.Caller, projectID, runID string, frozen []extension.Frozen) error {
	for _, f := range frozen {
		key := f.ID + "@" + f.Version
		versionID, extensionID := r.resolved[key], r.grantIDs[key]
		var sha string
		if err := tx.QueryRow(ctx, `SELECT manifest_sha256 FROM workflow_extension_versions WHERE organization_id=$1 AND id=$2`,
			r.org, versionID).Scan(&sha); err != nil || sha != f.ManifestSHA256 {
			return ErrConflict
		}
		if caller != nil {
			ok, err := r.store.authz.CanUse(ctx, tx, *caller, projectID, access.ResourceExtension, extensionID) // merge: lane U CanUse
			if err != nil {
				return err
			}
			if !ok {
				return ErrNotFound
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workflow_run_extensions
			(organization_id,run_id,extension_version_id,manifest_sha256,permissions_sha256)
			VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, r.org, runID, versionID, f.ManifestSHA256, f.PermissionsSHA256); err != nil {
			return err
		}
	}
	return nil
}

// RunExtensions lists the extension versions a run froze.
func (s *Store) RunExtensions(ctx context.Context, org, runID string) ([]ExtensionVersion, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+extensionVersionColumns+`
		JOIN workflow_run_extensions re ON re.organization_id=v.organization_id AND re.extension_version_id=v.id
		WHERE v.organization_id=$1 AND re.run_id=$2 ORDER BY v.version`, org, runID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ExtensionVersion, error) { return scanExtensionVersion(row) })
}

const extensionColumns = `e.id::text,e.extension_key,e.repository_url,e.git_ref,COALESCE(e.current_version_id::text,''),
	COALESCE(cv.version,''),COALESCE(cv.commit_sha,''),COALESCE(e.latest_ref_commit,''),
	(SELECT count(*)::integer FROM workflow_extension_versions c WHERE c.organization_id=e.organization_id AND c.extension_id=e.id),
	e.latest_checked_at,e.created_at,e.updated_at
	FROM workflow_extensions e
	LEFT JOIN workflow_extension_versions cv ON cv.organization_id=e.organization_id AND cv.id=e.current_version_id`

func scanExtension(row pgx.Row) (Extension, error) {
	var e Extension
	err := row.Scan(&e.ID, &e.Key, &e.RepositoryURL, &e.GitRef, &e.CurrentVersionID, &e.CurrentVersion, &e.CurrentCommit,
		&e.LatestRefCommit, &e.VersionCount, &e.LatestCheckedAt, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, ErrNotFound
	}
	return e, err
}

func getExtension(ctx context.Context, q queryRower, org, id string) (Extension, error) {
	return scanExtension(q.QueryRow(ctx, `SELECT `+extensionColumns+` WHERE e.organization_id=$1 AND e.id=$2`, org, id))
}

const extensionVersionColumns = `v.id::text,v.extension_id::text,v.version,v.repository_url,v.git_ref,v.commit_sha,v.manifest_json,
	v.manifest_sha256,v.manifest_origin,v.manifest_path,v.approved_permissions,v.permissions_sha256,v.installed_by::text,
	COALESCE(p.username,''),v.created_at
	FROM workflow_extension_versions v LEFT JOIN identity_principals p ON p.id=v.installed_by`

func scanExtensionVersion(row pgx.Row) (ExtensionVersion, error) {
	var v ExtensionVersion
	err := row.Scan(&v.ID, &v.ExtensionID, &v.Version, &v.RepositoryURL, &v.GitRef, &v.Commit, &v.Manifest, &v.ManifestSHA256,
		&v.ManifestOrigin, &v.ManifestPath, &v.Approved, &v.PermissionsSHA256, &v.InstalledBy, &v.InstalledByUsername, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	return v, err
}

func getExtensionVersion(ctx context.Context, q queryRower, org, id string) (ExtensionVersion, error) {
	return scanExtensionVersion(q.QueryRow(ctx, `SELECT `+extensionVersionColumns+` WHERE v.organization_id=$1 AND v.id=$2`, org, id))
}

// Extension grants are the same organization resource grants as recipes
// (kind "extension"), in the same ConnectionGrant shape.
var extensionGrantColumns = strings.Replace(recipeGrantColumns, "g.resource_kind='recipe'", "g.resource_kind='extension'", 1)

// ExtensionGrants lists live grants on an extension; owners/admins only.
func (s *Store) ExtensionGrants(ctx context.Context, caller identity.Caller, extensionID string) ([]ConnectionGrant, error) {
	if !ids(caller.OrganizationID, extensionID) {
		return nil, ErrInvalid
	}
	if caller.Role != "owner" && caller.Role != "admin" {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, extensionGrantColumns+` AND g.resource_id=$2 ORDER BY g.created_at,g.id LIMIT 2000`,
		caller.OrganizationID, extensionID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ConnectionGrant, error) { return scanRecipeGrant(row) })
}

// GrantExtensionAs grants an extension to a project, a member, or a minimum
// role; access.GrantResource audits it.
func (s *Store) GrantExtensionAs(ctx context.Context, caller identity.Caller, extensionID, projectID, granteeKind, granteeID string) (ConnectionGrant, error) {
	if err := requireExtensionAdmin(caller); err != nil {
		return ConnectionGrant{}, err
	}
	if !ids(extensionID) {
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
	if _, err := getExtension(ctx, tx, caller.OrganizationID, extensionID); err != nil {
		return ConnectionGrant{}, err
	}
	id, err := access.GrantResource(ctx, tx, caller, access.ResourceExtension, extensionID, to)
	switch {
	case errors.Is(err, access.ErrResourceGrantInvalid):
		return ConnectionGrant{}, ErrInvalid
	case errors.Is(err, access.ErrDenied):
		return ConnectionGrant{}, ErrExtensionDenied
	case err != nil:
		return ConnectionGrant{}, err
	}
	grant, err := scanRecipeGrant(tx.QueryRow(ctx, extensionGrantColumns+` AND g.id=$2::uuid`, caller.OrganizationID, id))
	if err != nil {
		return ConnectionGrant{}, err
	}
	return grant, tx.Commit(ctx)
}

// RevokeExtensionGrantAs revokes one live extension grant; launched runs
// keep the versions they froze.
func (s *Store) RevokeExtensionGrantAs(ctx context.Context, caller identity.Caller, grantID string) error {
	if err := requireExtensionAdmin(caller); err != nil {
		return err
	}
	if !ids(grantID) {
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
	var found bool
	err = tx.QueryRow(ctx, `SELECT true FROM access_resource_grants WHERE organization_id=$1::uuid AND id=$2::uuid
		AND resource_kind='extension' AND revoked_at IS NULL FOR UPDATE`, caller.OrganizationID, grantID).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := access.RevokeResourceGrant(ctx, tx, caller, grantID); errors.Is(err, access.ErrDenied) {
		return ErrExtensionDenied
	} else if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
