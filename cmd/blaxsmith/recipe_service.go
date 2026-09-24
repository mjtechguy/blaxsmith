package main

import (
	"context"
	"errors"
	"path"
	"strings"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

// recipeService serves the managed recipe library. Reads need a live session;
// mutations also require CSRF and recheck the session and role under lock.
type recipeService struct {
	guard    *identity.BrowserGuard
	store    *workflow.Store
	workflow *workflowService // Project source fetch for file pickers.
}

func recipeError(err error) error {
	var invalid *workflow.RecipeInvalidError
	switch {
	case errors.As(err, &invalid):
		return connect.NewError(connect.CodeInvalidArgument, errors.New(invalid.Error()))
	case errors.Is(err, workflow.ErrRecipeDenied):
		return connect.NewError(connect.CodePermissionDenied, errors.New("recipe change denied"))
	case errors.Is(err, workflow.ErrConflict):
		return connect.NewError(connect.CodeAlreadyExists, errors.New("a recipe with this name already exists in this scope"))
	}
	return workflowError(err)
}

func libraryRecipeMessage(r workflow.LibraryRecipe) *api.LibraryRecipe {
	return &api.LibraryRecipe{Id: r.ID, ProjectId: r.ProjectID, Name: r.Name, Description: r.Description,
		CurrentVersionId: r.CurrentVersionID, CurrentVersion: r.CurrentVersion, VersionCount: r.VersionCount,
		CreatedAt: adminTime(r.CreatedAt), UpdatedAt: adminTime(r.UpdatedAt)}
}

func recipeVersionMessage(v workflow.RecipeVersion) *api.RecipeVersion {
	return &api.RecipeVersion{Id: v.ID, RecipeId: v.RecipeID, Version: v.Version, RecipeJson: string(v.JSON),
		Sha256: v.SHA256, FrozenPath: v.FrozenPath, AuthorPrincipalId: v.AuthorID, AuthorUsername: v.AuthorUsername,
		CreatedAt: adminTime(v.CreatedAt)}
}

func validationErrors(err error) []*api.RecipeValidationError {
	var invalid *workflow.RecipeInvalidError
	if errors.As(err, &invalid) {
		return []*api.RecipeValidationError{{Path: invalid.Field.Path, Message: invalid.Field.Message}}
	}
	return nil
}

func (s *recipeService) ListRecipes(ctx context.Context, req *connect.Request[api.ListRecipesRequest]) (*connect.Response[api.ListRecipesResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	recipes, err := s.store.ListRecipes(ctx, caller, req.Msg.ProjectId)
	if err != nil {
		return nil, recipeError(err)
	}
	response := &api.ListRecipesResponse{}
	for _, r := range recipes {
		response.Recipes = append(response.Recipes, libraryRecipeMessage(r))
	}
	return connect.NewResponse(response), nil
}

func (s *recipeService) GetRecipe(ctx context.Context, req *connect.Request[api.GetRecipeRequest]) (*connect.Response[api.GetRecipeResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	r, versions, err := s.store.GetRecipe(ctx, caller, req.Msg.RecipeId)
	if err != nil {
		return nil, recipeError(err)
	}
	response := &api.GetRecipeResponse{Recipe: libraryRecipeMessage(r)}
	for _, v := range versions {
		response.Versions = append(response.Versions, recipeVersionMessage(v))
	}
	if r.ProjectID == "" {
		grants, err := s.store.RecipeGrants(ctx, caller, r.ID)
		if err != nil {
			return nil, recipeError(err)
		}
		for _, g := range grants {
			response.Grants = append(response.Grants, grantMessage(g))
		}
	}
	return connect.NewResponse(response), nil
}

func (s *recipeService) GrantRecipe(ctx context.Context, req *connect.Request[api.GrantRecipeRequest]) (*connect.Response[api.GrantRecipeResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	grant, err := s.store.GrantRecipeAs(ctx, caller, req.Msg.RecipeId, req.Msg.ProjectId, req.Msg.GranteeKind, req.Msg.GranteeId)
	if err != nil {
		return nil, recipeError(err)
	}
	return connect.NewResponse(&api.GrantRecipeResponse{Grant: grantMessage(grant)}), nil
}

func (s *recipeService) RevokeRecipeGrant(ctx context.Context, req *connect.Request[api.RevokeRecipeGrantRequest]) (*connect.Response[api.RevokeRecipeGrantResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if err := s.store.RevokeRecipeGrantAs(ctx, caller, req.Msg.GrantId); err != nil {
		return nil, recipeError(err)
	}
	return connect.NewResponse(&api.RevokeRecipeGrantResponse{}), nil
}

func (s *recipeService) GetRecipeVersion(ctx context.Context, req *connect.Request[api.GetRecipeVersionRequest]) (*connect.Response[api.GetRecipeVersionResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	v, err := s.store.GetRecipeVersion(ctx, caller, req.Msg.VersionId)
	if err != nil {
		return nil, recipeError(err)
	}
	return connect.NewResponse(&api.GetRecipeVersionResponse{Version: recipeVersionMessage(v)}), nil
}

func (s *recipeService) ValidateRecipe(ctx context.Context, req *connect.Request[api.ValidateRecipeRequest]) (*connect.Response[api.ValidateRecipeResponse], error) {
	if _, err := s.guard.Caller(ctx, req.Header(), false); err != nil {
		return nil, err
	}
	_, order, fe := workflow.ValidateRecipe([]byte(req.Msg.RecipeJson), req.Msg.FrozenPath)
	response := &api.ValidateRecipeResponse{StageOrder: order}
	if fe != nil {
		response.Errors = []*api.RecipeValidationError{{Path: fe.Path, Message: fe.Message}}
	}
	return connect.NewResponse(response), nil
}

func (s *recipeService) CreateRecipe(ctx context.Context, req *connect.Request[api.CreateRecipeRequest]) (*connect.Response[api.CreateRecipeResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	r, v, err := s.store.CreateRecipeAs(ctx, caller, req.Msg.ProjectId, req.Msg.Name, req.Msg.Description,
		[]byte(req.Msg.RecipeJson), req.Msg.FrozenPath)
	if errs := validationErrors(err); errs != nil {
		return connect.NewResponse(&api.CreateRecipeResponse{Errors: errs}), nil
	}
	if err != nil {
		return nil, recipeError(err)
	}
	return connect.NewResponse(&api.CreateRecipeResponse{Recipe: libraryRecipeMessage(r), Version: recipeVersionMessage(v)}), nil
}

func (s *recipeService) CreateRecipeVersion(ctx context.Context, req *connect.Request[api.CreateRecipeVersionRequest]) (*connect.Response[api.CreateRecipeVersionResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	v, err := s.store.CreateRecipeVersionAs(ctx, caller, req.Msg.RecipeId, []byte(req.Msg.RecipeJson), req.Msg.FrozenPath, req.Msg.MakeCurrent)
	if errs := validationErrors(err); errs != nil {
		return connect.NewResponse(&api.CreateRecipeVersionResponse{Errors: errs}), nil
	}
	if err != nil {
		return nil, recipeError(err)
	}
	return connect.NewResponse(&api.CreateRecipeVersionResponse{Version: recipeVersionMessage(v)}), nil
}

func (s *recipeService) SetCurrentRecipeVersion(ctx context.Context, req *connect.Request[api.SetCurrentRecipeVersionRequest]) (*connect.Response[api.SetCurrentRecipeVersionResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if err := s.store.SetCurrentRecipeVersionAs(ctx, caller, req.Msg.RecipeId, req.Msg.VersionId); err != nil {
		return nil, recipeError(err)
	}
	r, _, err := s.store.GetRecipe(ctx, caller, req.Msg.RecipeId)
	if err != nil {
		return nil, recipeError(err)
	}
	return connect.NewResponse(&api.SetCurrentRecipeVersionResponse{Recipe: libraryRecipeMessage(r)}), nil
}

func (s *recipeService) CloneRecipe(ctx context.Context, req *connect.Request[api.CloneRecipeRequest]) (*connect.Response[api.CloneRecipeResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	r, v, err := s.store.CloneRecipeAs(ctx, caller, req.Msg.SourceVersionId, req.Msg.ProjectId, req.Msg.Name, req.Msg.Description)
	if err != nil {
		return nil, recipeError(err)
	}
	return connect.NewResponse(&api.CloneRecipeResponse{Recipe: libraryRecipeMessage(r), Version: recipeVersionMessage(v)}), nil
}

var harnessProvider = map[string]string{"claude-code": "anthropic", "codex": "openai", "opencode": ""}

func (s *recipeService) GetRecipeEditorOptions(ctx context.Context, req *connect.Request[api.GetRecipeEditorOptionsRequest]) (*connect.Response[api.GetRecipeEditorOptionsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	connections, err := s.store.RecipeModelConnections(ctx, caller)
	if err != nil {
		return nil, recipeError(err)
	}
	response := &api.GetRecipeEditorOptionsResponse{StageKinds: recipe.StageKinds}
	for _, harness := range recipe.Harnesses {
		response.Harnesses = append(response.Harnesses, &api.RecipeHarnessOption{Harness: harness,
			Provider: harnessProvider[harness], Efforts: recipe.Efforts[harness]})
	}
	for _, c := range connections {
		response.Connections = append(response.Connections, &api.RecipeModelConnection{Id: c.ID, Provider: c.Provider,
			Account: c.Account, Models: c.Models})
	}
	return connect.NewResponse(response), nil
}

const maxPromptPaths = 2000

func (s *recipeService) ListProjectRecipeFiles(ctx context.Context, req *connect.Request[api.ListProjectRecipeFilesRequest]) (*connect.Response[api.ListProjectRecipeFilesResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	if _, err := s.store.GetProject(ctx, caller.OrganizationID, req.Msg.ProjectId); err != nil {
		return nil, workflowError(err)
	}
	_, _, fetched, err := s.workflow.fetchProjectSource(ctx, caller.OrganizationID, req.Msg.ProjectId)
	if err != nil {
		return nil, err
	}
	defer fetched.Close()
	commit, files, err := recipe.ListFiles(ctx, fetched.Directory, fetched.Commit)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("repository files could not be listed"))
	}
	response := &api.ListProjectRecipeFilesResponse{Commit: commit}
	for _, name := range files {
		if path.Base(name) == "SKILL.md" {
			response.SkillPaths = append(response.SkillPaths, name)
		}
		if strings.HasSuffix(strings.ToLower(name), ".md") && len(response.PromptPaths) < maxPromptPaths {
			response.PromptPaths = append(response.PromptPaths, name)
		}
	}
	return connect.NewResponse(response), nil
}
