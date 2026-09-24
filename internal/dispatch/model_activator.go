package dispatch

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/axbridge"
	"github.com/mjtechguy/blaxsmith/internal/bootstrap"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

// ModelActivator opens one attested AX bootstrap challenge for the selected
// model binding. Base contains only router transport/token/CA/signer settings.
type ModelActivator struct {
	DB        *pgxpool.Pool
	Secrets   *access.SecretStore
	ClusterID string
	LeaseTTL  time.Duration
	Actor     axbridge.Inspector
	Base      bootstrap.Connector
}

// Preflight checks local connector trust material, the database, and the
// projected service-account token before an attempt is reserved.
func (a *ModelActivator) Preflight(ctx context.Context) error {
	if !a.ready() || a.DB.Ping(ctx) != nil {
		return ErrNotReady
	}
	token, err := a.Base.Token(ctx)
	if err != nil || token == "" || len(token) > 16384 || strings.ContainsAny(token, " \t\r\n") {
		return ErrNotReady
	}
	return nil
}

func (a *ModelActivator) ready() bool {
	if a == nil || a.DB == nil || a.Secrets == nil || a.ClusterID == "" || a.LeaseTTL <= 0 || a.LeaseTTL > time.Hour ||
		a.Actor == nil || a.Base.Client == nil || a.Base.Client.Timeout <= 0 || a.Base.Client.Timeout > time.Minute ||
		a.Base.Token == nil || a.Base.Roots == nil || len(a.Base.Signer) != ed25519.PrivateKeySize ||
		a.Base.Authorize != nil || a.Base.Reserve != nil || a.Base.Delivered != nil ||
		a.Base.GitSetup != nil || a.Base.ModelCredential != nil {
		return false
	}
	router, err := url.Parse(a.Base.RouterURL)
	if err != nil || router.Scheme != "https" || router.Host == "" || router.User != nil ||
		router.RawQuery != "" || router.Fragment != "" || router.Path != "" {
		return false
	}
	transport, ok := a.Base.Client.Transport.(*http.Transport)
	return ok && transport.TLSClientConfig != nil && !transport.TLSClientConfig.InsecureSkipVerify &&
		transport.TLSClientConfig.RootCAs != nil && transport.TLSClientConfig.ServerName == router.Hostname() && transport.Proxy == nil
}

func (a *ModelActivator) Activate(ctx context.Context, attempt workflow.Attempt, runtime bootstrap.Runtime,
	binding workflow.RuntimeBinding, invoke access.ModelInvoke) error {
	connector, scope, ledger, err := a.attemptConnector(ctx, attempt, runtime, binding, invoke, true)
	if err != nil {
		return err
	}
	err = connector.Open(ctx, scope, runtime)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_, _ = ledger.Deactivate(cleanupCtx, scope)
		cancel()
	}
	return err
}

// ReleaseModel creates a fresh, actor-attested challenge only after the
// dispatcher has observed AX workspace readiness and moved the attempt to
// running.
func (a *ModelActivator) ReleaseModel(ctx context.Context, attempt workflow.Attempt, runtime bootstrap.Runtime,
	binding workflow.RuntimeBinding, invoke access.ModelInvoke) error {
	connector, scope, _, err := a.attemptConnector(ctx, attempt, runtime, binding, invoke, false)
	if err != nil {
		return err
	}
	return connector.OpenModel(ctx, scope, runtime)
}

func (a *ModelActivator) attemptConnector(ctx context.Context, attempt workflow.Attempt, runtime bootstrap.Runtime,
	binding workflow.RuntimeBinding, invoke access.ModelInvoke, assign bool) (bootstrap.Connector, bootstrap.Scope, *bootstrap.Ledger, error) {
	if !a.ready() ||
		attempt.OrganizationID == "" || attempt.RunID == "" || attempt.TaskID == "" || attempt.ID == "" ||
		attempt.OwnerGeneration <= 0 || attempt.FenceToken == "" ||
		!runtime.DataOnlySnapshots() || runtime.Actor.Atespace == "" || runtime.Actor.Name == "" || runtime.Actor.UID == "" ||
		binding.AXAtespace != runtime.Actor.Atespace || binding.AXTask != runtime.Actor.Name ||
		binding.ActorUID != runtime.Actor.UID || binding.TemplateUID != runtime.TemplateUID ||
		binding.Image != runtime.Image || binding.WorkerPool != runtime.WorkerPool ||
		invoke.OrganizationID != attempt.OrganizationID || invoke.AttemptID != attempt.ID || invoke.BindingID == "" ||
		invoke.ProjectID == "" || invoke.GranteeKind == "" || invoke.GranteeID == "" ||
		invoke.Provider == "" || invoke.Model == "" || invoke.PolicyVersion <= 0 ||
		runtime.BootstrapPublicKey != base64.StdEncoding.EncodeToString(a.Base.Signer.Public().(ed25519.PublicKey)) {
		return bootstrap.Connector{}, bootstrap.Scope{}, nil, bootstrap.ErrDenied
	}
	ledger := bootstrap.NewLedger(a.DB)
	var err error
	var scope bootstrap.Scope
	if assign {
		scope, err = ledger.Assign(ctx, a.ClusterID, attempt.ID, 0, runtime.Actor)
	} else {
		var actor bootstrap.Actor
		var active, exists bool
		scope, actor, active, exists, err = ledger.CurrentOwner(ctx, a.ClusterID, attempt.ID)
		if err == nil && (!exists || !active || actor != runtime.Actor) {
			err = bootstrap.ErrDenied
		}
	}
	if err != nil {
		return bootstrap.Connector{}, bootstrap.Scope{}, nil, err
	}
	base := a.Base
	base.Ledger = ledger
	base.Current = func(ctx context.Context) (bootstrap.Runtime, error) {
		return a.Actor.Current(ctx, runtime.Actor.Atespace, runtime.Actor.Name)
	}
	connector, err := bootstrap.NewModelAttemptConnector(base, a.DB, a.Secrets, bootstrap.ModelAttempt{
		Scope: scope, Attempt: attempt, Runtime: binding, Invoke: invoke, TTL: a.LeaseTTL,
	})
	if err != nil {
		if assign {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_, _ = ledger.Deactivate(cleanupCtx, scope) // No challenge or credential has been issued yet.
			cancel()
		}
		return bootstrap.Connector{}, bootstrap.Scope{}, nil, err
	}
	return connector, scope, ledger, nil
}

// RevokeOwner fences bootstrap delivery and all platform leases before the
// bridge deletes the actor. Deleting the actor is still required to end use of
// a raw provider credential already held by its process.
func (a *ModelActivator) RevokeOwner(ctx context.Context, attempt workflow.Attempt) error {
	if a == nil || a.DB == nil || a.ClusterID == "" || attempt.ID == "" {
		return bootstrap.ErrDenied
	}
	ledger := bootstrap.NewLedger(a.DB)
	scope, _, active, exists, err := ledger.CurrentOwner(ctx, a.ClusterID, attempt.ID)
	if err != nil {
		return err
	}
	if exists && active {
		if _, err := ledger.Deactivate(ctx, scope); err != nil {
			return err
		}
	}
	return access.RevokeAttemptLeases(ctx, a.DB, a.ClusterID, attempt.ID)
}
