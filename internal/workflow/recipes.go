package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
)

// ErrRecipeDenied means the caller may read but not change a recipe scope.
var ErrRecipeDenied = errors.New("recipe change denied")

// RecipeInvalidError carries the first validation failure with its JSON path.
type RecipeInvalidError struct{ Field *recipe.FieldError }

func (e *RecipeInvalidError) Error() string {
	return "recipe invalid at " + e.Field.Path + ": " + e.Field.Message
}
func (e *RecipeInvalidError) Unwrap() error { return ErrInvalid }

// LibraryRecipe is an organization-scoped (ProjectID empty) or project-scoped
// recipe. Its versions are immutable; CurrentVersionID is the default choice.
type LibraryRecipe struct {
	ID, ProjectID, Name, Description, CurrentVersionID string
	CurrentVersion, VersionCount                       int32
	CreatedAt, UpdatedAt                               time.Time
}

type RecipeVersion struct {
	ID, RecipeID             string
	Version                  int32
	JSON                     []byte // Exact validated bytes; empty in version lists.
	SHA256, FrozenPath       string
	AuthorID, AuthorUsername string // Empty for the installation seed.
	CreatedAt                time.Time
}

// ResourceAccess decides whether a principal may use an organization-level
// resource from a project. It runs inside the caller's transaction.
// merge: lane U CanUse — satisfied by access.CanUse (lane-u-users 9c58ffa);
// use access.ResourceRecipe for the kind. There, owners and admins get no
// implicit use: every project needs an access_resource_grants row.
type ResourceAccess interface {
	CanUse(ctx context.Context, tx pgx.Tx, principal identity.Caller, projectID, resourceKind, resourceID string) (bool, error)
}

// ResourceRecipe is the resourceKind for library recipes.
const ResourceRecipe = "recipe"

// orgVisibleRecipes is the temporary grant policy: an organization recipe is
// usable by every project in its organization.
// merge: lane U CanUse — delete once access.CanUse is wired in New.
type orgVisibleRecipes struct{}

func (orgVisibleRecipes) CanUse(ctx context.Context, tx pgx.Tx, principal identity.Caller, projectID, resourceKind, resourceID string) (bool, error) {
	if resourceKind != ResourceRecipe || !ids(principal.OrganizationID, projectID, resourceID) {
		return false, nil
	}
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workflow_recipes r JOIN workflow_projects p
		ON p.organization_id=r.organization_id AND p.id=$3
		WHERE r.organization_id=$1 AND r.id=$2 AND r.project_id IS NULL)`,
		principal.OrganizationID, resourceID, projectID).Scan(&ok)
	return ok, err
}

// SetResourceAccess replaces the organization-resource grant policy.
func (s *Store) SetResourceAccess(access ResourceAccess) { s.access = access }

func (s *Store) resourceAccess() ResourceAccess {
	if s.access == nil {
		return orgVisibleRecipes{}
	}
	return s.access
}

const recipeListLimit = 500

const recipeColumns = `r.id::text,COALESCE(r.project_id::text,''),r.name,r.description,
	COALESCE(r.current_version_id::text,''),COALESCE(cv.version,0),
	(SELECT count(*)::integer FROM workflow_recipe_versions v WHERE v.organization_id=r.organization_id AND v.recipe_id=r.id),
	r.created_at,r.updated_at FROM workflow_recipes r
	LEFT JOIN workflow_recipe_versions cv ON cv.organization_id=r.organization_id AND cv.id=r.current_version_id`

func scanRecipe(row pgx.Row) (LibraryRecipe, error) {
	var r LibraryRecipe
	err := row.Scan(&r.ID, &r.ProjectID, &r.Name, &r.Description, &r.CurrentVersionID, &r.CurrentVersion,
		&r.VersionCount, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// ValidateRecipe checks recipe bytes and a frozen path label; nil means valid.
func ValidateRecipe(data []byte, frozenPath string) (recipe.Recipe, []string, *recipe.FieldError) {
	r, order, fe := recipe.Validate(data)
	if fe == nil && frozenPath != "" && !recipe.ValidPath(frozenPath) {
		fe = &recipe.FieldError{Path: "frozen_path", Message: "use a clean repository-relative file path"}
	}
	return r, order, fe
}

// ListRecipes returns organization recipes when projectID is empty. With a
// project, it returns that project's recipes plus organization recipes the
// project may use. Any organization role may read.
func (s *Store) ListRecipes(ctx context.Context, caller identity.Caller, projectID string) ([]LibraryRecipe, error) {
	if !ids(caller.OrganizationID) || (projectID != "" && !ids(projectID)) {
		return nil, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if projectID != "" {
		if err := projectExists(ctx, tx, caller.OrganizationID, projectID); err != nil {
			return nil, err
		}
	}
	rows, err := tx.Query(ctx, `SELECT `+recipeColumns+`
		WHERE r.organization_id=$1 AND (r.project_id IS NULL OR r.project_id=NULLIF($2,'')::uuid)
		ORDER BY r.project_id NULLS LAST, lower(r.name), r.id LIMIT $3`, caller.OrganizationID, projectID, recipeListLimit)
	if err != nil {
		return nil, err
	}
	all, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (LibraryRecipe, error) { return scanRecipe(row) })
	if err != nil {
		return nil, err
	}
	out := make([]LibraryRecipe, 0, len(all))
	for _, r := range all {
		if projectID != "" && r.ProjectID == "" {
			ok, err := s.resourceAccess().CanUse(ctx, tx, caller, projectID, ResourceRecipe, r.ID)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// GetRecipe returns a recipe and its version metadata, newest first.
func (s *Store) GetRecipe(ctx context.Context, caller identity.Caller, recipeID string) (LibraryRecipe, []RecipeVersion, error) {
	if !ids(caller.OrganizationID, recipeID) {
		return LibraryRecipe{}, nil, ErrInvalid
	}
	r, err := scanRecipe(s.pool.QueryRow(ctx, `SELECT `+recipeColumns+` WHERE r.organization_id=$1 AND r.id=$2`,
		caller.OrganizationID, recipeID))
	if errors.Is(err, pgx.ErrNoRows) {
		return LibraryRecipe{}, nil, ErrNotFound
	}
	if err != nil {
		return LibraryRecipe{}, nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT v.id::text,v.recipe_id::text,v.version,v.sha256,v.frozen_path,
		COALESCE(v.author_principal_id::text,''),COALESCE(p.username,''),v.created_at
		FROM workflow_recipe_versions v LEFT JOIN identity_principals p ON p.id=v.author_principal_id
		WHERE v.organization_id=$1 AND v.recipe_id=$2 ORDER BY v.version DESC LIMIT $3`,
		caller.OrganizationID, recipeID, recipeListLimit)
	if err != nil {
		return LibraryRecipe{}, nil, err
	}
	versions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (RecipeVersion, error) {
		var v RecipeVersion
		err := row.Scan(&v.ID, &v.RecipeID, &v.Version, &v.SHA256, &v.FrozenPath, &v.AuthorID, &v.AuthorUsername, &v.CreatedAt)
		return v, err
	})
	return r, versions, err
}

// GetRecipeVersion returns one version with its exact JSON.
func (s *Store) GetRecipeVersion(ctx context.Context, caller identity.Caller, versionID string) (RecipeVersion, error) {
	if !ids(caller.OrganizationID, versionID) {
		return RecipeVersion{}, ErrInvalid
	}
	return getRecipeVersion(ctx, s.pool, caller.OrganizationID, versionID)
}

type queryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func getRecipeVersion(ctx context.Context, q queryRower, orgID, versionID string) (RecipeVersion, error) {
	var v RecipeVersion
	err := q.QueryRow(ctx, `SELECT v.id::text,v.recipe_id::text,v.version,v.recipe_json,v.sha256,v.frozen_path,
		COALESCE(v.author_principal_id::text,''),COALESCE(p.username,''),v.created_at
		FROM workflow_recipe_versions v LEFT JOIN identity_principals p ON p.id=v.author_principal_id
		WHERE v.organization_id=$1 AND v.id=$2`, orgID, versionID).
		Scan(&v.ID, &v.RecipeID, &v.Version, &v.JSON, &v.SHA256, &v.FrozenPath, &v.AuthorID, &v.AuthorUsername, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RecipeVersion{}, ErrNotFound
	}
	return v, err
}

// CreateRecipeAs creates a recipe in the organization (projectID empty) or a
// project with a validated first version that becomes current.
func (s *Store) CreateRecipeAs(ctx context.Context, caller identity.Caller, projectID, name, description string, data []byte, frozenPath string) (LibraryRecipe, RecipeVersion, error) {
	name, description = strings.TrimSpace(name), strings.TrimSpace(description)
	if name == "" || len(name) > 120 || len(description) > 2000 || (projectID != "" && !ids(projectID)) {
		return LibraryRecipe{}, RecipeVersion{}, ErrInvalid
	}
	parsed, _, fe := ValidateRecipe(data, frozenPath)
	if fe != nil {
		return LibraryRecipe{}, RecipeVersion{}, &RecipeInvalidError{fe}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return LibraryRecipe{}, RecipeVersion{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockRecipeEditor(ctx, tx, caller, projectID); err != nil {
		return LibraryRecipe{}, RecipeVersion{}, err
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO workflow_recipes (organization_id,project_id,name,description,created_by)
		VALUES ($1,NULLIF($2,'')::uuid,$3,$4,$5) ON CONFLICT DO NOTHING RETURNING id::text`,
		caller.OrganizationID, projectID, name, description, caller.PrincipalID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return LibraryRecipe{}, RecipeVersion{}, ErrConflict // Name taken in this scope.
	}
	if err != nil {
		return LibraryRecipe{}, RecipeVersion{}, err
	}
	version, err := insertRecipeVersion(ctx, tx, caller, id, projectID, data, defaultFrozenPath(frozenPath, parsed), true, "workflow.recipe.created")
	if err != nil {
		return LibraryRecipe{}, RecipeVersion{}, err
	}
	r, err := scanRecipe(tx.QueryRow(ctx, `SELECT `+recipeColumns+` WHERE r.organization_id=$1 AND r.id=$2`, caller.OrganizationID, id))
	if err != nil {
		return LibraryRecipe{}, RecipeVersion{}, err
	}
	return r, version, tx.Commit(ctx)
}

// CloneRecipeAs copies one readable version into a new recipe in the target
// scope. Cloning an organization recipe into a project requires that the
// project may use it.
func (s *Store) CloneRecipeAs(ctx context.Context, caller identity.Caller, sourceVersionID, projectID, name, description string) (LibraryRecipe, RecipeVersion, error) {
	if !ids(caller.OrganizationID, sourceVersionID) || (projectID != "" && !ids(projectID)) {
		return LibraryRecipe{}, RecipeVersion{}, ErrInvalid
	}
	source, err := getRecipeVersion(ctx, s.pool, caller.OrganizationID, sourceVersionID)
	if err != nil {
		return LibraryRecipe{}, RecipeVersion{}, err
	}
	var sourceProject string
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(project_id::text,'') FROM workflow_recipes WHERE organization_id=$1 AND id=$2`,
		caller.OrganizationID, source.RecipeID).Scan(&sourceProject); err != nil {
		return LibraryRecipe{}, RecipeVersion{}, err
	}
	if projectID != "" && sourceProject == "" {
		if err := s.canUseInTx(ctx, caller, projectID, source.RecipeID); err != nil {
			return LibraryRecipe{}, RecipeVersion{}, err
		}
	} else if sourceProject != "" && sourceProject != projectID {
		return LibraryRecipe{}, RecipeVersion{}, ErrNotFound // Project recipes stay in their project.
	}
	return s.CreateRecipeAs(ctx, caller, projectID, name, description, source.JSON, source.FrozenPath)
}

func (s *Store) canUseInTx(ctx context.Context, caller identity.Caller, projectID, recipeID string) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	ok, err := s.resourceAccess().CanUse(ctx, tx, caller, projectID, ResourceRecipe, recipeID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// CreateRecipeVersionAs appends an immutable validated version.
func (s *Store) CreateRecipeVersionAs(ctx context.Context, caller identity.Caller, recipeID string, data []byte, frozenPath string, makeCurrent bool) (RecipeVersion, error) {
	if !ids(caller.OrganizationID, recipeID) {
		return RecipeVersion{}, ErrInvalid
	}
	parsed, _, fe := ValidateRecipe(data, frozenPath)
	if fe != nil {
		return RecipeVersion{}, &RecipeInvalidError{fe}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RecipeVersion{}, err
	}
	defer tx.Rollback(ctx)
	projectID, err := lockRecipe(ctx, tx, caller.OrganizationID, recipeID)
	if err != nil {
		return RecipeVersion{}, err
	}
	if err := lockRecipeEditor(ctx, tx, caller, projectID); err != nil {
		return RecipeVersion{}, err
	}
	version, err := insertRecipeVersion(ctx, tx, caller, recipeID, projectID, data, defaultFrozenPath(frozenPath, parsed), makeCurrent, "workflow.recipe.version_created")
	if err != nil {
		return RecipeVersion{}, err
	}
	return version, tx.Commit(ctx)
}

// SetCurrentRecipeVersionAs marks an existing version as the recipe default.
// Runs already launched keep the bytes they froze.
func (s *Store) SetCurrentRecipeVersionAs(ctx context.Context, caller identity.Caller, recipeID, versionID string) error {
	if !ids(caller.OrganizationID, recipeID, versionID) {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	projectID, err := lockRecipe(ctx, tx, caller.OrganizationID, recipeID)
	if err != nil {
		return err
	}
	if err := lockRecipeEditor(ctx, tx, caller, projectID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE workflow_recipes SET current_version_id=$3,updated_at=clock_timestamp()
		WHERE organization_id=$1 AND id=$2 AND EXISTS (SELECT 1 FROM workflow_recipe_versions v
			WHERE v.organization_id=$1 AND v.recipe_id=$2 AND v.id=$3)`, caller.OrganizationID, recipeID, versionID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	if err := recipeAudit(ctx, tx, caller, "workflow.recipe.current_set", versionID, projectID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// LibraryRecipeForLaunch returns the exact bytes and path label of a version
// the caller may launch in projectID: a recipe of that project, or an
// organization recipe the project may use.
func (s *Store) LibraryRecipeForLaunch(ctx context.Context, caller identity.Caller, projectID, versionID string) (RecipeVersion, error) {
	if !ids(caller.OrganizationID, projectID, versionID) {
		return RecipeVersion{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return RecipeVersion{}, err
	}
	defer tx.Rollback(ctx)
	v, err := s.launchableVersion(ctx, tx, caller, projectID, versionID)
	if err != nil {
		return RecipeVersion{}, err
	}
	return v, tx.Commit(ctx)
}

func (s *Store) launchableVersion(ctx context.Context, tx pgx.Tx, caller identity.Caller, projectID, versionID string) (RecipeVersion, error) {
	v, err := getRecipeVersion(ctx, tx, caller.OrganizationID, versionID)
	if err != nil {
		return RecipeVersion{}, err
	}
	var recipeProject string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(project_id::text,'') FROM workflow_recipes WHERE organization_id=$1 AND id=$2`,
		caller.OrganizationID, v.RecipeID).Scan(&recipeProject); err != nil {
		return RecipeVersion{}, err
	}
	switch {
	case recipeProject == projectID:
	case recipeProject == "":
		ok, err := s.resourceAccess().CanUse(ctx, tx, caller, projectID, ResourceRecipe, v.RecipeID)
		if err != nil {
			return RecipeVersion{}, err
		}
		if !ok {
			return RecipeVersion{}, ErrNotFound
		}
	default:
		return RecipeVersion{}, ErrNotFound
	}
	return v, nil
}

func defaultFrozenPath(frozenPath string, r recipe.Recipe) string {
	if frozenPath != "" {
		return frozenPath
	}
	return ".blaxsmith/recipes/" + r.Name + ".json"
}

func projectExists(ctx context.Context, tx pgx.Tx, orgID, projectID string) error {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT true FROM workflow_projects WHERE organization_id=$1 AND id=$2`, orgID, projectID).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func lockRecipe(ctx context.Context, tx pgx.Tx, orgID, recipeID string) (string, error) {
	var projectID string
	err := tx.QueryRow(ctx, `SELECT COALESCE(project_id::text,'') FROM workflow_recipes
		WHERE organization_id=$1 AND id=$2 FOR UPDATE`, orgID, recipeID).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return projectID, err
}

// lockRecipeEditor rechecks the live session under lock. Organization owners
// and admins edit organization recipes. Project recipes are edited by project
// admins; there are no project-level roles, so those are also organization
// owners and admins. Project recipes are not governed by CanUse.
func lockRecipeEditor(ctx context.Context, tx pgx.Tx, caller identity.Caller, projectID string) error {
	if caller.Role != "owner" && caller.Role != "admin" {
		return ErrRecipeDenied
	}
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID) {
		return ErrFenced
	}
	if err := lockProjectModelAccessAdmin(ctx, tx, caller); err != nil {
		return err
	}
	if projectID != "" {
		return projectExists(ctx, tx, caller.OrganizationID, projectID)
	}
	return nil
}

func insertRecipeVersion(ctx context.Context, tx pgx.Tx, caller identity.Caller, recipeID, projectID string, data []byte, frozenPath string, makeCurrent bool, action string) (RecipeVersion, error) {
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO workflow_recipe_versions
		(organization_id,recipe_id,version,recipe_json,sha256,frozen_path,author_principal_id)
		SELECT $1,$2,COALESCE(max(version),0)+1,$3,encode(sha256($3),'hex'),$4,$5
		FROM workflow_recipe_versions WHERE organization_id=$1 AND recipe_id=$2 RETURNING id::text`,
		caller.OrganizationID, recipeID, data, frozenPath, caller.PrincipalID).Scan(&id); err != nil {
		return RecipeVersion{}, fmt.Errorf("insert recipe version: %w", err)
	}
	if makeCurrent {
		if _, err := tx.Exec(ctx, `UPDATE workflow_recipes SET current_version_id=$3,updated_at=clock_timestamp()
			WHERE organization_id=$1 AND id=$2`, caller.OrganizationID, recipeID, id); err != nil {
			return RecipeVersion{}, err
		}
	} else if _, err := tx.Exec(ctx, `UPDATE workflow_recipes SET updated_at=clock_timestamp()
		WHERE organization_id=$1 AND id=$2`, caller.OrganizationID, recipeID); err != nil {
		return RecipeVersion{}, err
	}
	if err := recipeAudit(ctx, tx, caller, action, id, projectID); err != nil {
		return RecipeVersion{}, err
	}
	return getRecipeVersion(ctx, tx, caller.OrganizationID, id)
}

func recipeAudit(ctx context.Context, tx pgx.Tx, caller identity.Caller, action, subjectID, projectID string) error {
	_, err := tx.Exec(ctx, `INSERT INTO identity_audit_events (organization_id,actor_kind,actor_id,action,subject_id,project_id)
		VALUES ($1,'principal',$2,$3,$4,NULLIF($5,'')::uuid)`, caller.OrganizationID, caller.PrincipalID, action, subjectID, projectID)
	return err
}
