package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

// connectionService serves the connections hub. Every mutation requires
// CSRF and rechecks the session under lock in the store. No response carries
// a key, token, auth.json, client secret, or device authorization id.
type connectionService struct {
	guard   *identity.BrowserGuard
	store   *workflow.Store
	secrets *access.SecretStore
	origin  string
	catalog access.ModelCatalog
	device  access.CodexDevice
	git     access.GitAPI
	tools   *catalogService // Cached tool versions for the health line; may be nil.
}

// Pending sign-ins are encrypted rows in access_pending_sign_ins (see
// access.SecretStore.SavePending), so any replica can finish a flow.
const (
	pendingDevice = "codex_device"
	pendingGitHub = "github_oauth"
	// devicePollHold outlasts one Poll (two provider calls, 30s each).
	devicePollHold = 90 * time.Second
)

type githubState struct {
	Verifier  string `json:"verifier"`
	Scope     string `json:"scope"`
	ProjectID string `json:"project_id"`
	ReturnTo  string `json:"return_to"`
}

const githubCallbackPath = "/oauth/github/callback"

func newConnectionService(guard *identity.BrowserGuard, store *workflow.Store, secrets *access.SecretStore, origin string) *connectionService {
	return &connectionService{guard: guard, store: store, secrets: secrets, origin: origin}
}

func pendingOwner(caller identity.Caller) access.PendingOwner {
	return access.PendingOwner{OrganizationID: caller.OrganizationID, PrincipalID: caller.PrincipalID, SessionID: caller.SessionID}
}

func pendingError(err error) error {
	if errors.Is(err, access.ErrTooManyPending) {
		return connect.NewError(connect.CodeResourceExhausted, err)
	}
	return connect.NewError(connect.CodeInternal, errors.New("sign-in state unavailable"))
}

func connectionError(err error) error {
	switch {
	case errors.Is(err, workflow.ErrConnectionDenied):
		return connect.NewError(connect.CodePermissionDenied, errors.New("connection management denied"))
	case errors.Is(err, access.ErrClaudeSubscriptionDisabled):
		return connect.NewError(connect.CodeFailedPrecondition, access.ErrClaudeSubscriptionDisabled)
	case errors.Is(err, access.ErrKeyRejected):
		return connect.NewError(connect.CodeInvalidArgument, errors.New("the provider rejected this credential"))
	case errors.Is(err, access.ErrBaseURL):
		return connect.NewError(connect.CodeInvalidArgument, access.ErrBaseURL)
	}
	return workflowError(err)
}

func optionalTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return adminTime(*t)
}

// latestHarnessVersions reads the tools catalog's cached latest stable
// versions without ever fetching; an empty map when the catalog is cold.
func (s *connectionService) latestHarnessVersions() map[string]string {
	out := map[string]string{}
	if s.tools == nil {
		return out
	}
	if snapshot, _ := s.tools.snapshot(time.Now()); snapshot != nil {
		for _, tool := range snapshot.Tools {
			out[tool.Tool] = tool.LatestStable
		}
	}
	return out
}

func (s *connectionService) connectionMessage(c workflow.Connection) *api.Connection {
	h := workflow.DeriveHealth(c, s.latestHarnessVersions(), time.Now())
	out := &api.Connection{Health: &api.ConnectionHealth{State: h.State, Auth: h.Auth, Identity: h.Identity,
		CheckedAt: optionalTime(h.CheckedAt), Reason: h.Reason, Message: h.Message, Harness: h.Harness,
		PinnedVersion: h.PinnedVersion, LatestVersion: h.LatestVersion},
		Id: c.ID, Scope: c.Scope, OwnerId: c.OwnerID, OwnerName: c.OwnerName, Kind: c.Kind,
		Provider: c.Provider, Account: c.Account, Label: c.Label, State: c.State, LastUsedAt: optionalTime(c.LastUsed),
		CreatedAt: adminTime(c.CreatedAt), ModelCount: c.ModelCount, ModelsCheckedAt: optionalTime(c.ModelsCheckedAt),
		ModelsError: c.ModelsError, CanManage: c.CanManage, BaseUrl: c.BaseURL}
	for _, g := range c.Grants {
		out.Grants = append(out.Grants, grantMessage(g))
	}
	for _, u := range c.Uses {
		out.Uses = append(out.Uses, useMessage(u))
	}
	return out
}

func grantMessage(g workflow.ConnectionGrant) *api.ConnectionGrant {
	return &api.ConnectionGrant{Id: g.ID, ProjectId: g.ProjectID, ProjectName: g.ProjectName, GranteeKind: g.GranteeKind,
		GranteeId: g.GranteeID, GranteeName: g.GranteeName, CreatedAt: adminTime(g.CreatedAt)}
}

func useMessage(u workflow.ConnectionUse) *api.ConnectionUse {
	return &api.ConnectionUse{Id: u.ID, ProjectId: u.ProjectID, ProjectName: u.ProjectName, Model: u.Model,
		GranteeKind: u.GranteeKind, CreatedAt: adminTime(u.CreatedAt)}
}

func (s *connectionService) ListConnections(ctx context.Context, req *connect.Request[api.ListConnectionsRequest]) (*connect.Response[api.ListConnectionsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	items, err := s.store.ListConnectionsAs(ctx, caller, req.Msg.Scope, req.Msg.ProjectId)
	if err != nil {
		return nil, connectionError(err)
	}
	out := &api.ListConnectionsResponse{}
	for _, c := range items {
		out.Connections = append(out.Connections, s.connectionMessage(c))
	}
	return connect.NewResponse(out), nil
}

// fetchModels lists the provider's models with key, from baseURL when set; a
// rejected key is an error, anything else is stored as the connection's model
// error (for a base URL that lists no models, the user types a model id).
func (s *connectionService) fetchModels(ctx context.Context, provider, baseURL string, key []byte) ([]access.CatalogModel, string, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	models, err := s.catalog.Fetch(fetchCtx, provider, baseURL, key)
	if errors.Is(err, access.ErrKeyRejected) || errors.Is(err, access.ErrDenied) {
		return nil, "", err
	}
	if err != nil {
		return nil, err.Error(), nil
	}
	return models, "", nil
}

func (s *connectionService) CreateApiKeyConnection(ctx context.Context, req *connect.Request[api.CreateApiKeyConnectionRequest]) (*connect.Response[api.CreateApiKeyConnectionResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	key := []byte(strings.TrimSpace(req.Msg.ApiKey))
	defer clear(key)
	if access.ModelOrigin(req.Msg.Provider) == "" || len(key) == 0 {
		return nil, connectionError(workflow.ErrInvalid)
	}
	baseURL, err := access.NormalizeBaseURL(req.Msg.BaseUrl)
	if err != nil {
		return nil, connectionError(err)
	}
	// Authorize before the key or the base URL is used for any request.
	if err := s.store.CheckConnectionScopeAs(ctx, caller, req.Msg.Scope, req.Msg.ProjectId); err != nil {
		return nil, connectionError(err)
	}
	models, modelsErr, err := s.fetchModels(ctx, req.Msg.Provider, baseURL, key)
	if err != nil {
		return nil, connectionError(err)
	}
	c, err := s.store.CreateAPIKeyConnectionAs(ctx, caller, req.Msg.Scope, req.Msg.ProjectId, req.Msg.Provider, req.Msg.Label,
		baseURL, key, models, modelsErr, s.secrets)
	if err != nil {
		return nil, connectionError(err)
	}
	return connect.NewResponse(&api.CreateApiKeyConnectionResponse{Connection: s.connectionMessage(c)}), nil
}

// SetConnectionBaseUrl sets or clears an API-key connection's base URL, then
// lists models from the new endpoint with the stored key.
func (s *connectionService) SetConnectionBaseUrl(ctx context.Context, req *connect.Request[api.SetConnectionBaseUrlRequest]) (*connect.Response[api.SetConnectionBaseUrlResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	baseURL, err := access.NormalizeBaseURL(req.Msg.BaseUrl)
	if err != nil {
		return nil, connectionError(err)
	}
	if err := s.store.SetConnectionBaseURLAs(ctx, caller, req.Msg.ConnectionId, baseURL); err != nil {
		return nil, connectionError(err)
	}
	secret, err := s.store.ReadConnectionSecretAs(ctx, caller, req.Msg.ConnectionId, s.secrets)
	if err == nil {
		_, _, err = s.refresh(ctx, secret)
	}
	if err != nil {
		slog.Warn("model list refresh after a base URL change failed", "error", err)
	}
	c, err := s.store.ManagedConnectionAs(ctx, caller, req.Msg.ConnectionId)
	if err != nil {
		return nil, connectionError(err)
	}
	return connect.NewResponse(&api.SetConnectionBaseUrlResponse{Connection: s.connectionMessage(c)}), nil
}

func (s *connectionService) CreateGitTokenConnection(ctx context.Context, req *connect.Request[api.CreateGitTokenConnectionRequest]) (*connect.Response[api.CreateGitTokenConnectionResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	token := []byte(req.Msg.Token)
	defer clear(token)
	c, err := s.store.CreateGitTokenConnectionAs(ctx, caller, req.Msg.Scope, req.Msg.ProjectId, req.Msg.Host, req.Msg.Username,
		"token", token, s.secrets)
	if err != nil {
		return nil, connectionError(err)
	}
	return connect.NewResponse(&api.CreateGitTokenConnectionResponse{Connection: s.connectionMessage(c)}), nil
}

func (s *connectionService) CreateCodexSubscription(ctx context.Context, req *connect.Request[api.CreateCodexSubscriptionRequest]) (*connect.Response[api.CreateCodexSubscriptionResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	credential := []byte(req.Msg.AuthJson)
	defer clear(credential)
	c, err := s.store.CreateCodexSubscriptionAs(ctx, caller, credential, s.secrets)
	if err != nil {
		return nil, connectionError(err)
	}
	return connect.NewResponse(&api.CreateCodexSubscriptionResponse{Connection: s.connectionMessage(c)}), nil
}

func (s *connectionService) CreateClaudeSubscription(ctx context.Context, req *connect.Request[api.CreateClaudeSubscriptionRequest]) (*connect.Response[api.CreateClaudeSubscriptionResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	token := []byte(strings.TrimSpace(req.Msg.SetupToken))
	defer clear(token)
	c, err := s.store.CreateClaudeSubscriptionAs(ctx, caller, token, s.secrets)
	if err != nil {
		return nil, connectionError(err)
	}
	return connect.NewResponse(&api.CreateClaudeSubscriptionResponse{Connection: s.connectionMessage(c)}), nil
}

func (s *connectionService) GetClaudeSubscriptionPolicy(ctx context.Context, req *connect.Request[api.GetClaudeSubscriptionPolicyRequest]) (*connect.Response[api.GetClaudeSubscriptionPolicyResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	allowed, err := s.store.ClaudeSubscriptionAllowed(ctx, caller)
	if err != nil {
		return nil, connectionError(err)
	}
	return connect.NewResponse(&api.GetClaudeSubscriptionPolicyResponse{AllowMemberClaudeSubscription: allowed}), nil
}

func (s *connectionService) SetClaudeSubscriptionPolicy(ctx context.Context, req *connect.Request[api.SetClaudeSubscriptionPolicyRequest]) (*connect.Response[api.SetClaudeSubscriptionPolicyResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if err := s.store.SetClaudeSubscriptionAllowedAs(ctx, caller, req.Msg.AllowMemberClaudeSubscription); err != nil {
		return nil, connectionError(err)
	}
	return connect.NewResponse(&api.SetClaudeSubscriptionPolicyResponse{AllowMemberClaudeSubscription: req.Msg.AllowMemberClaudeSubscription}), nil
}

func randomID() (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func (s *connectionService) StartCodexDeviceLogin(ctx context.Context, req *connect.Request[api.StartCodexDeviceLoginRequest]) (*connect.Response[api.StartCodexDeviceLoginResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	code, err := s.device.Start(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	expires := time.Now().Add(15 * time.Minute)
	payload, err := json.Marshal(code)
	if err != nil {
		return nil, err
	}
	err = s.secrets.SavePending(ctx, pendingDevice, id, pendingOwner(caller), payload, expires)
	clear(payload)
	if err != nil {
		return nil, pendingError(err)
	}
	return connect.NewResponse(&api.StartCodexDeviceLoginResponse{LoginId: id, VerificationUrl: code.VerificationURL,
		UserCode: code.UserCode, IntervalSeconds: int32(code.Interval / time.Second), ExpiresAt: adminTime(expires)}), nil
}

func (s *connectionService) PollCodexDeviceLogin(ctx context.Context, req *connect.Request[api.PollCodexDeviceLoginRequest]) (*connect.Response[api.PollCodexDeviceLoginResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	payload, err := s.secrets.ClaimPending(ctx, pendingDevice, req.Msg.LoginId, pendingOwner(caller), devicePollHold)
	if errors.Is(err, access.ErrPendingBusy) {
		return connect.NewResponse(&api.PollCodexDeviceLoginResponse{State: "pending"}), nil
	}
	if errors.Is(err, access.ErrDenied) {
		return connect.NewResponse(&api.PollCodexDeviceLoginResponse{State: "expired"}), nil
	}
	if err != nil {
		return nil, pendingError(err)
	}
	var code access.CodexDeviceCode
	err = json.Unmarshal(payload, &code)
	clear(payload)
	if err != nil {
		return nil, pendingError(err)
	}
	credential, err := s.device.Poll(ctx, code)
	// Approved or failed: the code is spent either way. A detached context
	// lets a cancelled poll still release or delete its claim.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	if finishErr := s.secrets.FinishPending(finishCtx, pendingDevice, req.Msg.LoginId,
		!errors.Is(err, access.ErrDevicePending)); finishErr != nil {
		slog.Warn("finish pending device sign-in failed", "error", finishErr)
	}
	cancel()
	if errors.Is(err, access.ErrDevicePending) {
		return connect.NewResponse(&api.PollCodexDeviceLoginResponse{State: "pending"}), nil
	}
	if err != nil {
		return connect.NewResponse(&api.PollCodexDeviceLoginResponse{State: "failed", Error: err.Error()}), nil
	}
	defer clear(credential)
	c, err := s.store.CreateCodexSubscriptionAs(ctx, caller, credential, s.secrets)
	if err != nil {
		return connect.NewResponse(&api.PollCodexDeviceLoginResponse{State: "failed",
			Error: "sign-in succeeded but the login could not be stored: " + connectionError(err).Error()}), nil
	}
	return connect.NewResponse(&api.PollCodexDeviceLoginResponse{State: "connected", Connection: s.connectionMessage(c)}), nil
}

func (s *connectionService) ListConnectionModels(ctx context.Context, req *connect.Request[api.ListConnectionModelsRequest]) (*connect.Response[api.ListConnectionModelsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	models, checked, modelsErr, err := s.store.ListConnectionModelsAs(ctx, caller, req.Msg.ConnectionId, req.Msg.Harness)
	if err != nil {
		return nil, connectionError(err)
	}
	out := &api.ListConnectionModelsResponse{CheckedAt: optionalTime(checked), Error: modelsErr}
	for _, m := range models {
		out.Models = append(out.Models, connectionModelMessage(m))
	}
	return connect.NewResponse(out), nil
}

func (s *connectionService) refresh(ctx context.Context, secret workflow.ConnectionSecret) (int, string, error) {
	defer secret.Secret.Clear()
	if secret.Kind != "api_key" {
		return 0, "", workflow.ErrInvalid
	}
	models, modelsErr, err := s.fetchModels(ctx, secret.Provider, secret.BaseURL, secret.Secret.Bytes)
	if errors.Is(err, access.ErrKeyRejected) {
		modelsErr, err = "the provider rejected this API key", nil
	}
	if err != nil {
		return 0, "", err
	}
	if err := s.store.StoreConnectionModels(ctx, secret.OrganizationID, secret.ConnectionID, models, modelsErr); err != nil {
		return 0, "", err
	}
	return len(models), modelsErr, nil
}

func (s *connectionService) RefreshConnectionModels(ctx context.Context, req *connect.Request[api.RefreshConnectionModelsRequest]) (*connect.Response[api.RefreshConnectionModelsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	secret, err := s.store.ReadConnectionSecretAs(ctx, caller, req.Msg.ConnectionId, s.secrets)
	if err != nil {
		return nil, connectionError(err)
	}
	count, modelsErr, err := s.refresh(ctx, secret)
	if err != nil {
		return nil, connectionError(err)
	}
	return connect.NewResponse(&api.RefreshConnectionModelsResponse{Valid: modelsErr == "", ModelCount: int32(count),
		Error: modelsErr, CheckedAt: adminTime(time.Now())}), nil
}

// refreshModelsDaily keeps every API-key connection's model list at most a
// day old. It never logs secrets or provider bodies.
func (s *connectionService) refreshModelsDaily(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		due, err := s.store.DueModelRefresh(ctx, 24*time.Hour, 50)
		if err != nil && ctx.Err() == nil {
			slog.Warn("model refresh scan failed", "error", err)
		}
		for _, item := range due {
			secret, err := s.store.ReadConnectionSecret(ctx, item[0], item[1], s.secrets)
			if err == nil {
				_, _, err = s.refresh(ctx, secret)
			}
			if err != nil && ctx.Err() == nil {
				slog.Warn("model refresh failed", "connection", item[1], "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *connectionService) GrantConnection(ctx context.Context, req *connect.Request[api.GrantConnectionRequest]) (*connect.Response[api.GrantConnectionResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	grant, err := s.store.GrantConnectionAs(ctx, caller, req.Msg.ConnectionId, req.Msg.ProjectId, req.Msg.GranteeKind, req.Msg.GranteeId)
	if err != nil {
		return nil, connectionError(err)
	}
	return connect.NewResponse(&api.GrantConnectionResponse{Grant: grantMessage(grant)}), nil
}

func (s *connectionService) RevokeConnectionGrant(ctx context.Context, req *connect.Request[api.RevokeConnectionGrantRequest]) (*connect.Response[api.RevokeConnectionGrantResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if err := s.store.RevokeConnectionGrantAs(ctx, caller, req.Msg.GrantId); err != nil {
		return nil, connectionError(err)
	}
	return connect.NewResponse(&api.RevokeConnectionGrantResponse{}), nil
}

func (s *connectionService) AddConnectionUse(ctx context.Context, req *connect.Request[api.AddConnectionUseRequest]) (*connect.Response[api.AddConnectionUseResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	use, err := s.store.AddConnectionUseAs(ctx, caller, req.Msg.ConnectionId, req.Msg.ProjectId, req.Msg.Model)
	if err != nil {
		return nil, connectionError(err)
	}
	return connect.NewResponse(&api.AddConnectionUseResponse{Use: useMessage(use)}), nil
}

func (s *connectionService) RemoveConnectionUse(ctx context.Context, req *connect.Request[api.RemoveConnectionUseRequest]) (*connect.Response[api.RemoveConnectionUseResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if err := s.store.RemoveConnectionUseAs(ctx, caller, req.Msg.UseId); err != nil {
		return nil, connectionError(err)
	}
	return connect.NewResponse(&api.RemoveConnectionUseResponse{}), nil
}

func (s *connectionService) SetRecommendedModels(ctx context.Context, req *connect.Request[api.SetRecommendedModelsRequest]) (*connect.Response[api.SetRecommendedModelsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	models, err := s.store.SetRecommendedModelsAs(ctx, caller, req.Msg.ConnectionId, req.Msg.Models)
	if err != nil {
		return nil, connectionError(err)
	}
	return connect.NewResponse(&api.SetRecommendedModelsResponse{Models: models}), nil
}

func (s *connectionService) RevokeConnection(ctx context.Context, req *connect.Request[api.RevokeConnectionRequest]) (*connect.Response[api.RevokeConnectionResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if err := s.store.RevokeConnectionAs(ctx, caller, req.Msg.ConnectionId); err != nil {
		return nil, connectionError(err)
	}
	return connect.NewResponse(&api.RevokeConnectionResponse{}), nil
}

func (s *connectionService) gitSecret(ctx context.Context, header http.Header, connectionID string) (workflow.ConnectionSecret, error) {
	caller, err := s.guard.Caller(ctx, header, false)
	if err != nil {
		return workflow.ConnectionSecret{}, err
	}
	secret, err := s.store.ReadConnectionSecretAs(ctx, caller, connectionID, s.secrets)
	if err != nil {
		return workflow.ConnectionSecret{}, connectionError(err)
	}
	if secret.Kind != "git" {
		secret.Secret.Clear()
		return workflow.ConnectionSecret{}, connectionError(workflow.ErrInvalid)
	}
	return secret, nil
}

func gitAPIError(err error) error {
	if errors.Is(err, access.ErrKeyRejected) {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("the Git provider rejected this connection's token"))
	}
	if errors.Is(err, access.ErrDenied) {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid repository"))
	}
	return connect.NewError(connect.CodeUnavailable, errors.New("the Git provider is unavailable"))
}

func (s *connectionService) ListGitRepositories(ctx context.Context, req *connect.Request[api.ListGitRepositoriesRequest]) (*connect.Response[api.ListGitRepositoriesResponse], error) {
	secret, err := s.gitSecret(ctx, req.Header(), req.Msg.ConnectionId)
	if err != nil {
		return nil, err
	}
	defer secret.Secret.Clear()
	query := strings.TrimSpace(req.Msg.Query)
	if len(query) > 100 {
		return nil, connectionError(workflow.ErrInvalid)
	}
	repos, err := s.git.Repositories(ctx, secret.Host, secret.Secret.Bytes, query)
	if err != nil {
		return nil, gitAPIError(err)
	}
	out := &api.ListGitRepositoriesResponse{}
	for _, r := range repos {
		out.Repositories = append(out.Repositories, &api.GitRepository{FullName: r.FullName, CloneUrl: r.CloneURL,
			DefaultBranch: r.DefaultBranch, Private: r.Private})
	}
	return connect.NewResponse(out), nil
}

func (s *connectionService) ListGitBranches(ctx context.Context, req *connect.Request[api.ListGitBranchesRequest]) (*connect.Response[api.ListGitBranchesResponse], error) {
	secret, err := s.gitSecret(ctx, req.Header(), req.Msg.ConnectionId)
	if err != nil {
		return nil, err
	}
	defer secret.Secret.Clear()
	branches, err := s.git.Branches(ctx, secret.Host, secret.Secret.Bytes, req.Msg.FullName)
	if err != nil {
		return nil, gitAPIError(err)
	}
	return connect.NewResponse(&api.ListGitBranchesResponse{Branches: branches}), nil
}

func (s *connectionService) GetGitHubApp(ctx context.Context, req *connect.Request[api.GetGitHubAppRequest]) (*connect.Response[api.GetGitHubAppResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	app, err := s.store.GetGitHubApp(ctx, caller.OrganizationID)
	if err != nil {
		return nil, connectionError(err)
	}
	return connect.NewResponse(&api.GetGitHubAppResponse{ClientId: app.ClientID, Configured: app.Configured,
		CallbackUrl: s.origin + githubCallbackPath}), nil
}

func (s *connectionService) SetGitHubApp(ctx context.Context, req *connect.Request[api.SetGitHubAppRequest]) (*connect.Response[api.SetGitHubAppResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	secret := []byte(strings.TrimSpace(req.Msg.ClientSecret))
	defer clear(secret)
	app, err := s.store.SetGitHubAppAs(ctx, caller, strings.TrimSpace(req.Msg.ClientId), secret, s.secrets)
	if err != nil {
		return nil, connectionError(err)
	}
	return connect.NewResponse(&api.SetGitHubAppResponse{ClientId: app.ClientID, Configured: app.Configured}), nil
}

func safeReturn(path string) string {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "\\\r\n") || len(path) > 300 {
		return "/admin/connections"
	}
	return path
}

func (s *connectionService) StartGitHubConnect(ctx context.Context, req *connect.Request[api.StartGitHubConnectRequest]) (*connect.Response[api.StartGitHubConnectResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if req.Msg.Scope != workflow.ScopeOrganization && req.Msg.Scope != workflow.ScopeProject {
		return nil, connectionError(workflow.ErrInvalid)
	}
	// The callback creates the connection under the same rule; refuse before
	// the OAuth dance rather than after the user has approved on GitHub.
	if err := s.store.CheckConnectionScopeAs(ctx, caller, req.Msg.Scope, req.Msg.ProjectId); err != nil {
		return nil, connectionError(err)
	}
	app, err := s.store.GetGitHubApp(ctx, caller.OrganizationID)
	if err != nil {
		return nil, connectionError(err)
	}
	if !app.Configured {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("an administrator must register a GitHub OAuth App first"))
	}
	verifier, challenge, err := access.PKCE()
	if err != nil {
		return nil, err
	}
	state, err := randomID()
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(githubState{Verifier: verifier, Scope: req.Msg.Scope, ProjectID: req.Msg.ProjectId,
		ReturnTo: safeReturn(req.Msg.ReturnTo)})
	if err != nil {
		return nil, err
	}
	err = s.secrets.SavePending(ctx, pendingGitHub, state, pendingOwner(caller), payload, time.Now().Add(10*time.Minute))
	clear(payload)
	if err != nil {
		return nil, pendingError(err)
	}
	return connect.NewResponse(&api.StartGitHubConnectResponse{
		AuthorizeUrl: s.git.GitHubAuthorizeURL(app.ClientID, s.origin+githubCallbackPath, state, challenge)}), nil
}

// ServeHTTP is the GitHub OAuth callback. The state is single-use and bound
// to the session that started the flow; the PKCE verifier never left the
// server. The result is reported on the return page's query string.
func (s *connectionService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	query := r.URL.Query()
	var state githubState
	owner, payload, err := s.secrets.TakePending(r.Context(), pendingGitHub, query.Get("state"))
	ok := err == nil && json.Unmarshal(payload, &state) == nil
	clear(payload)
	redirect := func(path, key, value string) {
		q := url.Values{"github": {"error"}, "message": {value}}
		if key == "connected" {
			q = url.Values{"github": {"connected"}, "connection": {value}}
		}
		http.Redirect(w, r, path+"?"+q.Encode(), http.StatusSeeOther)
	}
	if !ok {
		redirect("/admin/connections", "github_error", "This GitHub sign-in expired; start again.")
		return
	}
	caller, err := s.guard.CallbackCaller(r.Context(), r.Header)
	if err != nil || pendingOwner(caller) != owner {
		redirect(state.ReturnTo, "github_error", "Your session changed during GitHub sign-in; start again.")
		return
	}
	if e := query.Get("error"); e != "" || query.Get("code") == "" {
		redirect(state.ReturnTo, "github_error", "GitHub sign-in was cancelled.")
		return
	}
	clientID, secret, err := s.store.GitHubAppSecret(r.Context(), caller.OrganizationID, s.secrets)
	if err != nil {
		redirect(state.ReturnTo, "github_error", "The GitHub OAuth App is not configured.")
		return
	}
	token, login, err := s.git.GitHubExchange(r.Context(), clientID, secret.Bytes, query.Get("code"), s.origin+githubCallbackPath, state.Verifier)
	secret.Clear()
	if err != nil {
		redirect(state.ReturnTo, "github_error", err.Error())
		return
	}
	defer clear(token)
	c, err := s.store.CreateGitTokenConnectionAs(r.Context(), caller, state.Scope, state.ProjectID, "github.com", login,
		"oauth_token", token, s.secrets)
	if err != nil {
		redirect(state.ReturnTo, "github_error", connectionError(err).Error())
		return
	}
	redirect(state.ReturnTo, "connected", c.ID)
}

func connectionModelMessage(m workflow.ConnectionModel) *api.ConnectionModel {
	return &api.ConnectionModel{Id: m.ID, DisplayName: m.DisplayName, CreatedAt: optionalTime(m.ReleasedAt),
		ContextTokens: m.ContextTokens, CapabilitiesJson: m.Capabilities, Harnesses: m.Harnesses,
		IsDefault: m.IsDefault, Legacy: m.Legacy, Badge: m.Badge, Efforts: m.Efforts, DefaultEffort: m.DefaultEffort,
		Recommended: m.Recommended}
}
