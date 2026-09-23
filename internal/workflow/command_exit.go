package workflow

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/runnerexit"
)

// CommandExitCollector accepts only the pinned platform connector's signed
// readback. An exit receipt is an observation, never a verified task result.
type CommandExitCollector struct {
	Store      *Store
	SignerID   string
	PublicKey  ed25519.PublicKey
	WorkerPool string
}

// CommandExitObservation is a signed connector observation, not a verified
// task result or artifact. The receipt remains visible if the attempt is later
// fenced, stopped, or retried.
type CommandExitObservation struct {
	EventID       int64
	TaskID        string
	AttemptID     string
	ActorUID      string
	SignerID      string
	ReceiptSHA256 string
	ExitCode      int32
	Signal        int32
	Interrupted   bool
	ObservedAt    int64 // runner-reported Unix nanoseconds
	ReceivedAt    time.Time
}

// ListCommandExits pages by the durable run event cursor, which also gives
// clients a stable ordering across retries and reconnects.
func (s *Store) ListCommandExits(ctx context.Context, orgID, runID string, after int64, limit int) ([]CommandExitObservation, error) {
	if !ids(orgID, runID) || after < 0 || limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT e.id,c.task_id,c.attempt_id,r.actor_uid,c.signer_id,c.report_sha256,
		c.exit_code,(c.report_json->>'signal')::integer,c.report_json,c.received_at
		FROM workflow_events e JOIN workflow_command_exits c
		ON c.organization_id=e.organization_id AND c.run_id=e.run_id AND c.task_id=e.task_id AND c.attempt_id=e.attempt_id
		JOIN workflow_attempt_runtime r ON r.organization_id=c.organization_id AND r.attempt_id=c.attempt_id
		WHERE e.organization_id=$1 AND e.run_id=$2 AND e.kind='attempt.command_exited' AND e.id>$3
		ORDER BY e.id LIMIT $4`, orgID, runID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	observations := []CommandExitObservation{}
	for rows.Next() {
		var o CommandExitObservation
		var body []byte
		if err := rows.Scan(&o.EventID, &o.TaskID, &o.AttemptID, &o.ActorUID, &o.SignerID,
			&o.ReceiptSHA256, &o.ExitCode, &o.Signal, &body, &o.ReceivedAt); err != nil {
			return nil, err
		}
		var report runnerexit.ExitReport
		if err := json.Unmarshal(body, &report); err != nil {
			return nil, err
		}
		if report.OrganizationID != orgID || report.RunID != runID || report.TaskID != o.TaskID ||
			report.AttemptID != o.AttemptID || report.ActorUID != o.ActorUID ||
			report.ExitCode != int(o.ExitCode) || report.Signal != int(o.Signal) {
			return nil, ErrConflict
		}
		o.Interrupted, o.ObservedAt = report.Interrupted, report.ObservedAt
		observations = append(observations, o)
	}
	return observations, rows.Err()
}

func (c CommandExitCollector) Record(ctx context.Context, signed runnerexit.SignedReport) error {
	if c.Store == nil || c.SignerID == "" || len(c.SignerID) > 128 || c.WorkerPool == "" ||
		!ids(signed.Report.OrganizationID, signed.Report.RunID, signed.Report.TaskID, signed.Report.AttemptID) {
		return ErrInvalid
	}
	if err := runnerexit.Verify(signed, c.PublicKey); err != nil {
		return ErrInvalid
	}
	canonical, err := runnerexit.Canonical(signed.Report)
	if err != nil {
		return ErrInvalid
	}
	encoded, err := json.Marshal(signed.Report)
	if err != nil {
		return err
	}
	signature, err := base64.StdEncoding.DecodeString(signed.Signature)
	if err != nil {
		return ErrInvalid
	}
	digest := sha256.Sum256(canonical)
	hash := hex.EncodeToString(digest[:])
	r := signed.Report
	tx, err := c.Store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var runState string
	var sealed bool
	if err := tx.QueryRow(ctx, `SELECT state,graph_sealed FROM workflow_runs
		WHERE organization_id=$1 AND id=$2 FOR UPDATE`, r.OrganizationID, r.RunID).
		Scan(&runState, &sealed); errors.Is(err, pgx.ErrNoRows) {
		return ErrFenced
	} else if err != nil {
		return err
	}
	var taskState, attemptState string
	var activeID *string
	var generation int64
	if err := tx.QueryRow(ctx, `SELECT t.state,t.active_attempt_id,a.state,a.generation
		FROM workflow_tasks t JOIN workflow_attempts a
		ON a.organization_id=t.organization_id AND a.task_id=t.id
		WHERE t.organization_id=$1 AND t.run_id=$2 AND t.id=$3 AND a.id=$4
		FOR UPDATE OF t,a`, r.OrganizationID, r.RunID, r.TaskID, r.AttemptID).
		Scan(&taskState, &activeID, &attemptState, &generation); errors.Is(err, pgx.ErrNoRows) {
		return ErrFenced
	} else if err != nil {
		return err
	}
	var binding RuntimeBinding
	if err := tx.QueryRow(ctx, `SELECT ax_atespace,ax_task,actor_uid,template_uid,image,worker_pool,command_sha256
		FROM workflow_attempt_runtime WHERE organization_id=$1 AND run_id=$2 AND task_id=$3 AND attempt_id=$4`,
		r.OrganizationID, r.RunID, r.TaskID, r.AttemptID).
		Scan(&binding.AXAtespace, &binding.AXTask, &binding.ActorUID, &binding.TemplateUID,
			&binding.Image, &binding.WorkerPool, &binding.CommandSHA256); errors.Is(err, pgx.ErrNoRows) {
		return ErrFenced
	} else if err != nil {
		return err
	}
	if generation != r.OwnerGeneration || binding.AXAtespace != r.AXAtespace ||
		binding.AXTask != r.AXTask || binding.ActorUID != r.ActorUID ||
		binding.TemplateUID != r.TemplateUID || binding.WorkerPool != c.WorkerPool ||
		binding.CommandSHA256 != r.CommandSHA256 {
		return ErrFenced
	}
	var oldSigner, oldHash string
	var oldSignature []byte
	err = tx.QueryRow(ctx, `SELECT signer_id,report_sha256,signature FROM workflow_command_exits
		WHERE organization_id=$1 AND attempt_id=$2`, r.OrganizationID, r.AttemptID).
		Scan(&oldSigner, &oldHash, &oldSignature)
	if err == nil {
		if oldSigner != c.SignerID || oldHash != hash || !ed25519.Verify(c.PublicKey, canonical, oldSignature) {
			return ErrConflict
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if runState != "active" || !sealed || activeID == nil || *activeID != r.AttemptID ||
		taskState != "running" || attemptState != "running" {
		return ErrFenced
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workflow_command_exits
		(organization_id,run_id,task_id,attempt_id,signer_id,report_json,report_sha256,
		 signature,exit_code,evidence_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, r.OrganizationID, r.RunID, r.TaskID,
		r.AttemptID, c.SignerID, encoded, hash, signature, r.ExitCode, r.EvidenceSHA256); err != nil {
		return err
	}
	if err := event(ctx, tx, r.OrganizationID, r.RunID, r.TaskID, r.AttemptID, "attempt.command_exited"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
