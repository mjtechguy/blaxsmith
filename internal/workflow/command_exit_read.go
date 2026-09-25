package workflow

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/runnerexit"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// Load returns an already recorded connector receipt for the exact current
// owner. Re-verifying its signature lets reconciliation resume after AX delete
// without trusting an unsigned database row or a stale attempt.
func (c CommandExitCollector) Load(ctx context.Context, a Attempt) (runnerexit.ExitReport, RuntimeBinding, error) {
	ctx = tenant.Org(ctx, a.OrganizationID)
	if c.Store == nil || c.SignerID == "" || c.WorkerPool == "" ||
		len(c.PublicKey) != ed25519.PublicKeySize || !validAttempt(a) {
		return runnerexit.ExitReport{}, RuntimeBinding{}, ErrInvalid
	}
	current := func() error {
		run, state, sealed, err := c.Store.CurrentAttempt(ctx, a)
		if err != nil {
			return err
		}
		if run != "active" || state != "running" || !sealed {
			return ErrFenced
		}
		return nil
	}
	if err := current(); err != nil {
		return runnerexit.ExitReport{}, RuntimeBinding{}, err
	}
	var body, signature []byte
	var signerID, digest, evidenceSHA string
	var binding RuntimeBinding
	var exitCode int
	err := c.Store.pool.QueryRow(ctx, `SELECT c.report_json,c.signature,c.signer_id,c.report_sha256,
		b.ax_atespace,b.ax_task,b.actor_uid,b.template_uid,b.image,b.worker_pool,b.command_sha256,
		c.evidence_sha256,c.exit_code FROM workflow_command_exits c
		JOIN workflow_attempt_runtime b ON b.organization_id=c.organization_id AND b.attempt_id=c.attempt_id
		WHERE c.organization_id=$1 AND c.run_id=$2 AND c.task_id=$3 AND c.attempt_id=$4`,
		a.OrganizationID, a.RunID, a.TaskID, a.ID).
		Scan(&body, &signature, &signerID, &digest, &binding.AXAtespace, &binding.AXTask,
			&binding.ActorUID, &binding.TemplateUID, &binding.Image, &binding.WorkerPool,
			&binding.CommandSHA256, &evidenceSHA, &exitCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return runnerexit.ExitReport{}, RuntimeBinding{}, ErrNotFound
	}
	if err != nil {
		return runnerexit.ExitReport{}, RuntimeBinding{}, err
	}
	var report runnerexit.ExitReport
	if json.Unmarshal(body, &report) != nil || signerID != c.SignerID || binding.WorkerPool != c.WorkerPool ||
		report.OrganizationID != a.OrganizationID || report.RunID != a.RunID || report.TaskID != a.TaskID ||
		report.AttemptID != a.ID || report.OwnerGeneration != a.OwnerGeneration ||
		report.AXAtespace != binding.AXAtespace || report.AXTask != binding.AXTask || report.ActorUID != binding.ActorUID ||
		report.TemplateUID != binding.TemplateUID || report.CommandSHA256 != binding.CommandSHA256 ||
		report.EvidenceSHA256 != evidenceSHA || report.ExitCode != exitCode ||
		runnerexit.Verify(runnerexit.SignedReport{Report: report, Signature: base64.StdEncoding.EncodeToString(signature)}, c.PublicKey) != nil {
		return runnerexit.ExitReport{}, RuntimeBinding{}, ErrConflict
	}
	canonical, err := runnerexit.Canonical(report)
	if err != nil {
		return runnerexit.ExitReport{}, RuntimeBinding{}, ErrConflict
	}
	hash := sha256.Sum256(canonical)
	if digest != hex.EncodeToString(hash[:]) {
		return runnerexit.ExitReport{}, RuntimeBinding{}, ErrConflict
	}
	if err := current(); err != nil {
		return runnerexit.ExitReport{}, RuntimeBinding{}, err
	}
	return report, binding, nil
}
