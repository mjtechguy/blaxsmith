package workflow

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// Code flow between stages (docs/interactive-sessions.md): implement stages
// push their commit to the run branch; every attempt freezes its input commit.

// AttemptInput is the commit a new attempt of taskID starts from: the latest
// accepted revision of its code-producing upstream (the nearest implement
// ancestor), or the run's source commit when there is none yet.
// ponytail: the first proof is linear; with several implement ancestors the
// first in recipe order wins, and parallel code-producing stages are deferred.
func (s *Store) AttemptInput(ctx context.Context, orgID, runID, taskID string) (string, error) {
	ctx = tenant.Org(ctx, orgID)
	if !ids(orgID, runID, taskID) {
		return "", ErrInvalid
	}
	var source, key string
	if err := s.pool.QueryRow(ctx, `SELECT r.source_commit,t.task_key FROM workflow_runs r
		JOIN workflow_tasks t ON t.organization_id=r.organization_id AND t.run_id=r.id
		WHERE r.organization_id=$1 AND r.id=$2 AND t.id=$3`, orgID, runID, taskID).Scan(&source, &key); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	bundle, err := runBundle(ctx, s.pool, orgID, runID)
	if errors.Is(err, ErrNotFound) {
		return source, nil // a run without a frozen recipe has one stage
	}
	if err != nil {
		return "", err
	}
	ancestors := map[string]bool{}
	var walk func(string)
	walk = func(id string) {
		stage, ok := findStage(bundle, id)
		if !ok {
			return
		}
		for _, parent := range stage.DependsOn {
			if !ancestors[parent] {
				ancestors[parent] = true
				walk(parent)
			}
		}
	}
	walk(key)
	i := slices.IndexFunc(bundle.Recipe.Stages, func(st recipe.Stage) bool { return ancestors[st.ID] && st.Kind == "implement" })
	if i < 0 {
		return source, nil
	}
	var revision string
	err = s.pool.QueryRow(ctx, `SELECT COALESCE(res.revision,'') FROM workflow_tasks t
		JOIN LATERAL (SELECT id FROM workflow_attempts WHERE organization_id=t.organization_id AND task_id=t.id
			AND state='succeeded' ORDER BY generation DESC LIMIT 1) a ON true
		JOIN workflow_attempt_results res ON res.organization_id=t.organization_id AND res.attempt_id=a.id
		WHERE t.organization_id=$1 AND t.run_id=$2 AND t.task_key=$3`, orgID, runID, bundle.Recipe.Stages[i].ID).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) || revision == "" {
		return source, nil
	}
	return revision, err
}

// RecordInputCommit freezes an attempt's input commit inside its reservation
// transaction. The attempt trigger keeps it immutable once set.
func RecordInputCommit(ctx context.Context, tx pgx.Tx, a Attempt, commit string) error {
	if !commitPattern.MatchString(commit) {
		return ErrInvalid
	}
	_, err := tx.Exec(ctx, `UPDATE workflow_attempts SET input_commit=$3 WHERE organization_id=$1 AND id=$2`,
		a.OrganizationID, a.ID, commit)
	return err
}

// RunBranchTarget is what completion needs to deliver an attempt's commit.
type RunBranchTarget struct {
	ProjectID, RepositoryURL string
	Input                    string // frozen input commit
	Expected                 string // last run-branch tip pushed, "" if none
	CodeProducing            bool   // implement stages push; others never do
}

func (s *Store) GetRunBranchTarget(ctx context.Context, a Attempt) (RunBranchTarget, error) {
	ctx = tenant.Org(ctx, a.OrganizationID)
	if !validAttempt(a) {
		return RunBranchTarget{}, ErrInvalid
	}
	var t RunBranchTarget
	var key string
	err := s.pool.QueryRow(ctx, `SELECT r.project_id,COALESCE(p.repository_url,''),COALESCE(a.input_commit,r.source_commit),
		COALESCE(r.branch_tip,''),tk.task_key
		FROM workflow_attempts a JOIN workflow_runs r ON r.organization_id=a.organization_id AND r.id=a.run_id
		JOIN workflow_tasks tk ON tk.organization_id=a.organization_id AND tk.id=a.task_id
		LEFT JOIN workflow_run_bundles p ON p.organization_id=r.organization_id AND p.run_id=r.id
		WHERE a.organization_id=$1 AND a.id=$2`, a.OrganizationID, a.ID).
		Scan(&t.ProjectID, &t.RepositoryURL, &t.Input, &t.Expected, &key)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, err
	}
	if bundle, err := runBundle(ctx, s.pool, a.OrganizationID, a.RunID); err == nil {
		stage, ok := findStage(bundle, key)
		t.CodeProducing = ok && stage.Kind == "implement"
	} else if !errors.Is(err, ErrNotFound) {
		return t, err
	}
	return t, nil
}

// LoadInputCommit returns an attempt's frozen input commit (the run's source
// commit for attempts reserved before code flow existed).
func (s *Store) LoadInputCommit(ctx context.Context, a Attempt) (string, error) {
	ctx = tenant.Org(ctx, a.OrganizationID)
	if !validAttempt(a) {
		return "", ErrInvalid
	}
	var commit string
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(a.input_commit,r.source_commit) FROM workflow_attempts a
		JOIN workflow_runs r ON r.organization_id=a.organization_id AND r.id=a.run_id
		WHERE a.organization_id=$1 AND a.id=$2`, a.OrganizationID, a.ID).Scan(&commit)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return commit, err
}

// RecordBranchTip records the run-branch tip right after a successful push,
// independent of attempt fencing, so the next push's lease expects it.
// An unconditional overwrite is safe: the push itself was force-with-lease
// against the previous tip, so only the winner of that race reaches here, and
// the remote now holds exactly this tip.
func (s *Store) RecordBranchTip(ctx context.Context, orgID, runID, tip string) error {
	ctx = tenant.Org(ctx, orgID)
	if !ids(orgID, runID) || !commitPattern.MatchString(tip) {
		return ErrInvalid
	}
	_, err := s.pool.Exec(ctx, `UPDATE workflow_runs SET branch_tip=$3 WHERE organization_id=$1 AND id=$2`, orgID, runID, tip)
	return err
}
