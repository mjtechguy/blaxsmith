package main

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/extension"
	"github.com/mjtechguy/blaxsmith/internal/gitfetch"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

// extensionService serves the extension registry. Reads need a live
// session; preview, install, update checks, and grants are owner/admin only,
// and mutations also require CSRF and recheck the session under lock.
type extensionService struct {
	guard *identity.BrowserGuard
	store *workflow.Store
	// fetch resolves a public Git ref (or commit) through the same pinned
	// proxy as project sources. ponytail: private extension repositories need
	// an organization-level git.read grant for the platform itself, which
	// does not exist yet; they fail as an unsupported source.
	fetch func(ctx context.Context, repositoryURL, ref string) (gitfetch.Source, error)
}

func newExtensionService(guard *identity.BrowserGuard, store *workflow.Store) *extensionService {
	return &extensionService{guard: guard, store: store, fetch: gitfetch.Fetch}
}

func extensionError(err error) error {
	var invalid *workflow.ExtensionInvalidError
	switch {
	case errors.As(err, &invalid):
		return connect.NewError(connect.CodeInvalidArgument, errors.New(invalid.Error()))
	case errors.Is(err, workflow.ErrExtensionDenied):
		return connect.NewError(connect.CodePermissionDenied, errors.New("extension administration denied"))
	case errors.Is(err, extension.ErrApproval):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, workflow.ErrConflict):
		return connect.NewError(connect.CodeAlreadyExists, errors.New("this extension version is installed from a different commit, manifest, or repository"))
	}
	return workflowError(err)
}

func extensionMessage(e workflow.Extension) *api.Extension {
	checked := ""
	if e.LatestCheckedAt != nil {
		checked = adminTime(*e.LatestCheckedAt)
	}
	return &api.Extension{Id: e.ID, Key: e.Key, RepositoryUrl: e.RepositoryURL, GitRef: e.GitRef,
		CurrentVersionId: e.CurrentVersionID, CurrentVersion: e.CurrentVersion, CurrentCommit: e.CurrentCommit,
		LatestRefCommit: e.LatestRefCommit, UpdateAvailable: e.UpdateAvailable(), VersionCount: e.VersionCount,
		LatestCheckedAt: checked, CreatedAt: adminTime(e.CreatedAt), UpdatedAt: adminTime(e.UpdatedAt)}
}

func permissionMessages(m extension.Manifest) []*api.ExtensionPermission {
	var out []*api.ExtensionPermission
	for _, p := range m.Permissions() {
		out = append(out, &api.ExtensionPermission{Id: p.ID, Kind: p.Kind, Description: p.Description, Optional: p.Optional})
	}
	return out
}

func templateMessages(m extension.Manifest) []*api.ExtensionTemplate {
	var out []*api.ExtensionTemplate
	for _, t := range m.StageTemplates {
		out = append(out, &api.ExtensionTemplate{Id: t.ID, Title: t.Title, Mode: t.Mode, Harness: t.Harness, Kinds: t.Kinds,
			Reference: m.ID + "@" + m.Version + "/" + t.ID})
	}
	return out
}

func extensionVersionMessage(v workflow.ExtensionVersion) *api.ExtensionVersion {
	msg := &api.ExtensionVersion{Id: v.ID, ExtensionId: v.ExtensionID, Version: v.Version, RepositoryUrl: v.RepositoryURL,
		GitRef: v.GitRef, Commit: v.Commit, ManifestJson: string(v.Manifest), ManifestSha256: v.ManifestSHA256,
		ManifestOrigin: v.ManifestOrigin, ManifestPath: v.ManifestPath, ApprovedPermissions: v.Approved,
		PermissionsSha256: v.PermissionsSHA256, InstalledBy: v.InstalledBy, InstalledByUsername: v.InstalledByUsername,
		CreatedAt: adminTime(v.CreatedAt)}
	if m, err := extension.Parse(v.Manifest); err == nil {
		msg.Permissions, msg.Templates = permissionMessages(m), templateMessages(m)
	}
	return msg
}

func (s *extensionService) ListExtensions(ctx context.Context, req *connect.Request[api.ListExtensionsRequest]) (*connect.Response[api.ListExtensionsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	list, err := s.store.ListExtensions(ctx, caller)
	if err != nil {
		return nil, extensionError(err)
	}
	response := &api.ListExtensionsResponse{}
	for _, e := range list {
		response.Extensions = append(response.Extensions, extensionMessage(e))
	}
	return connect.NewResponse(response), nil
}

func (s *extensionService) GetExtension(ctx context.Context, req *connect.Request[api.GetExtensionRequest]) (*connect.Response[api.GetExtensionResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	e, versions, err := s.store.GetExtension(ctx, caller, req.Msg.ExtensionId)
	if err != nil {
		return nil, extensionError(err)
	}
	response := &api.GetExtensionResponse{Extension: extensionMessage(e)}
	for _, v := range versions {
		response.Versions = append(response.Versions, extensionVersionMessage(v))
	}
	grants, err := s.store.ExtensionGrants(ctx, caller, e.ID)
	if err != nil {
		return nil, extensionError(err)
	}
	for _, g := range grants {
		response.Grants = append(response.Grants, grantMessage(g))
	}
	return connect.NewResponse(response), nil
}

// source fetches ref and returns the store input; the caller closes it.
func (s *extensionService) source(ctx context.Context, msg *api.ExtensionSource, ref string) (workflow.ExtensionSource, gitfetch.Source, error) {
	if msg == nil || gitfetch.Validate(msg.RepositoryUrl, msg.GitRef) != nil || msg.GitRef == "" ||
		(msg.ManifestPath != "" && !extension.ValidPath(msg.ManifestPath)) || len(msg.OverlayManifestJson) > extension.MaxManifestBytes {
		return workflow.ExtensionSource{}, gitfetch.Source{}, connect.NewError(connect.CodeInvalidArgument,
			errors.New("use a public GitHub or GitLab HTTPS repository, a branch, tag, or commit, and a repository-relative manifest path"))
	}
	fetched, err := s.fetch(ctx, msg.RepositoryUrl, ref)
	if err != nil {
		return workflow.ExtensionSource{}, gitfetch.Source{}, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("extension repository fetch failed; check the URL, the ref, and that the repository is public"))
	}
	src := workflow.ExtensionSource{RepositoryURL: msg.RepositoryUrl, GitRef: msg.GitRef, Commit: fetched.Commit,
		Directory: fetched.Directory, ManifestPath: msg.ManifestPath}
	if msg.OverlayManifestJson != "" {
		src.Overlay, src.ManifestPath = []byte(msg.OverlayManifestJson), ""
	}
	return src, fetched, nil
}

func invalidExtension(err error) []*api.ExtensionValidationError {
	var invalid *workflow.ExtensionInvalidError
	if errors.As(err, &invalid) {
		return []*api.ExtensionValidationError{{Path: invalid.Field.Path, Message: invalid.Field.Message}}
	}
	return nil
}

func (s *extensionService) PreviewExtensionInstall(ctx context.Context, req *connect.Request[api.PreviewExtensionInstallRequest]) (*connect.Response[api.PreviewExtensionInstallResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if caller.Role != "owner" && caller.Role != "admin" {
		return nil, extensionError(workflow.ErrExtensionDenied)
	}
	src, fetched, err := s.source(ctx, req.Msg.Source, req.Msg.GetSource().GetGitRef())
	if err != nil {
		return nil, err
	}
	defer fetched.Close()
	preview, err := s.store.PreviewExtension(ctx, caller, src)
	if errs := invalidExtension(err); errs != nil {
		return connect.NewResponse(&api.PreviewExtensionInstallResponse{Commit: src.Commit, Errors: errs}), nil
	}
	if err != nil {
		return nil, extensionError(err)
	}
	response := &api.PreviewExtensionInstallResponse{Commit: preview.Commit, ExtensionKey: preview.Manifest.ID,
		Version: preview.Manifest.Version, ManifestJson: string(preview.ManifestJSON), ManifestSha256: preview.ManifestSHA256,
		Permissions: permissionMessages(preview.Manifest), Templates: templateMessages(preview.Manifest)}
	if list, err := s.store.ListExtensions(ctx, caller); err == nil {
		for _, e := range list {
			if e.Key == preview.Manifest.ID {
				response.ExistingExtensionId = e.ID
			}
		}
	}
	return connect.NewResponse(response), nil
}

func (s *extensionService) InstallExtension(ctx context.Context, req *connect.Request[api.InstallExtensionRequest]) (*connect.Response[api.InstallExtensionResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if caller.Role != "owner" && caller.Role != "admin" {
		return nil, extensionError(workflow.ErrExtensionDenied)
	}
	if !gitfetch.IsCommit(req.Msg.ExpectedCommit) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("install needs the commit the preview resolved"))
	}
	// Fetch exactly the previewed commit; the ref may have moved since.
	src, fetched, err := s.source(ctx, req.Msg.Source, req.Msg.ExpectedCommit)
	if err != nil {
		return nil, err
	}
	defer fetched.Close()
	if src.Commit != req.Msg.ExpectedCommit {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the repository did not return the previewed commit"))
	}
	e, v, err := s.store.InstallExtensionAs(ctx, caller, src, req.Msg.ApprovedPermissions)
	if errs := invalidExtension(err); errs != nil {
		return connect.NewResponse(&api.InstallExtensionResponse{Errors: errs}), nil
	}
	if err != nil {
		return nil, extensionError(err)
	}
	return connect.NewResponse(&api.InstallExtensionResponse{Extension: extensionMessage(e), Version: extensionVersionMessage(v)}), nil
}

func (s *extensionService) CheckExtensionUpdate(ctx context.Context, req *connect.Request[api.CheckExtensionUpdateRequest]) (*connect.Response[api.CheckExtensionUpdateResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if caller.Role != "owner" && caller.Role != "admin" {
		return nil, extensionError(workflow.ErrExtensionDenied)
	}
	e, _, err := s.store.GetExtension(ctx, caller, req.Msg.ExtensionId)
	if err != nil {
		return nil, extensionError(err)
	}
	_, fetched, err := s.source(ctx, &api.ExtensionSource{RepositoryUrl: e.RepositoryURL, GitRef: e.GitRef}, e.GitRef)
	if err != nil {
		return nil, err
	}
	defer fetched.Close()
	updated, err := s.store.RecordExtensionRefAs(ctx, caller, e.ID, fetched.Commit)
	if err != nil {
		return nil, extensionError(err)
	}
	return connect.NewResponse(&api.CheckExtensionUpdateResponse{Extension: extensionMessage(updated)}), nil
}

func (s *extensionService) GrantExtension(ctx context.Context, req *connect.Request[api.GrantExtensionRequest]) (*connect.Response[api.GrantExtensionResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	grant, err := s.store.GrantExtensionAs(ctx, caller, req.Msg.ExtensionId, req.Msg.ProjectId, req.Msg.GranteeKind, req.Msg.GranteeId)
	if err != nil {
		return nil, extensionError(err)
	}
	return connect.NewResponse(&api.GrantExtensionResponse{Grant: grantMessage(grant)}), nil
}

func (s *extensionService) RevokeExtensionGrant(ctx context.Context, req *connect.Request[api.RevokeExtensionGrantRequest]) (*connect.Response[api.RevokeExtensionGrantResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if err := s.store.RevokeExtensionGrantAs(ctx, caller, req.Msg.GrantId); err != nil {
		return nil, extensionError(err)
	}
	return connect.NewResponse(&api.RevokeExtensionGrantResponse{}), nil
}
