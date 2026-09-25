package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/runbranch"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
	"github.com/mjtechguy/blaxsmith/internal/terminal"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const attemptBundlePath = "/tmp/blaxsmith/result.bundle"

// runBranchDelivery pushes a code-producing stage's commit to the run branch
// before completion records its result (docs/interactive-sessions.md).
type runBranchDelivery struct {
	store *workflow.Store
	// readBundle streams the guest's result.bundle; os.ErrNotExist = none.
	readBundle func(context.Context, workflow.Attempt, io.Writer) error
	// remote resolves the project's platform-held write credential.
	remote func(ctx context.Context, orgID, projectID, repoURL string) (runbranch.Remote, error)
}

// Deliver sets result.Revision to the accepted revision: the pushed commit,
// or the input commit when the stage changed nothing or does not produce code.
// A bundle that fails verification or a lease conflict discards the result
// (workflow.ErrInvalid), so the attempt takes the normal retry path.
func (d *runBranchDelivery) Deliver(ctx context.Context, a workflow.Attempt, result *workflow.AttemptResult) error {
	target, err := d.store.GetRunBranchTarget(ctx, a)
	if err != nil {
		return err
	}
	if !target.CodeProducing || result.Revision == "" || result.Revision == target.Input {
		// ponytail: review/verify stages never push; their edits are dropped.
		result.Revision = target.Input
		return nil
	}
	file, err := os.CreateTemp("", "blaxsmith-bundle-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := d.readBundle(ctx, a, file); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: result names a new revision but no bundle", workflow.ErrInvalid)
		}
		return err
	}
	remote, err := d.remote(ctx, a.OrganizationID, target.ProjectID, target.RepositoryURL)
	if err != nil {
		return err
	}
	defer clear(remote.Token)
	err = runbranch.Deliver(ctx, remote, runbranch.Push{Bundle: file.Name(), Ref: runbranch.Ref(a.RunID),
		Input: target.Input, Revision: result.Revision, Expected: target.Expected})
	if errors.Is(err, runbranch.ErrRejected) {
		return fmt.Errorf("%w: %v", workflow.ErrInvalid, err)
	}
	if err != nil {
		return err
	}
	return d.store.RecordBranchTip(ctx, a.OrganizationID, a.RunID, result.Revision)
}

// guestBundleReader reads result.bundle from the attempt's pinned actor.
func guestBundleReader(store *workflow.Store, router *terminal.Router) func(context.Context, workflow.Attempt, io.Writer) error {
	return func(ctx context.Context, a workflow.Attempt, w io.Writer) error {
		binding, err := store.GetRuntimeBinding(ctx, a)
		if err != nil {
			return err
		}
		guest, err := router.Guest(binding.AXAtespace, binding.AXTask)
		if err != nil {
			return err
		}
		err = guest.ReadFileTo(ctx, attemptBundlePath, w, runbranch.MaxBundle)
		if status.Code(err) == codes.NotFound {
			return os.ErrNotExist
		}
		return err
	}
}

// gitWriteRemote resolves the project's git.write grant through the same
// access tables and secret custody as private-Git bootstrap.
func gitWriteRemote(pool *pgxpool.Pool, secrets *access.SecretStore) func(context.Context, string, string, string) (runbranch.Remote, error) {
	return func(ctx context.Context, orgID, projectID, repoURL string) (runbranch.Remote, error) {
		ctx = tenant.Org(ctx, orgID)
		tx, err := pool.Begin(ctx)
		if err != nil {
			return runbranch.Remote{}, err
		}
		defer tx.Rollback(ctx)
		decision, err := access.AuthorizeGitWrite(ctx, tx, orgID, projectID, repoURL)
		if err != nil {
			return runbranch.Remote{}, fmt.Errorf("run branch push has no git.write connection: %w", err)
		}
		secret, err := secrets.ReadCurrent(ctx, tx, orgID, decision.ConnectionID)
		if err != nil {
			return runbranch.Remote{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			secret.Clear()
			return runbranch.Remote{}, err
		}
		return runbranch.Remote{URL: repoURL, Username: decision.ExternalAccountID, Token: secret.Bytes}, nil
	}
}
