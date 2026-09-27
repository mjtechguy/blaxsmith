package workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/evidence"
	"github.com/mjtechguy/blaxsmith/internal/extension"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// EvidenceRecord carries metadata only; content is fetched separately on demand.
type EvidenceRecord struct {
	ID, AttemptID, Stage, Kind, Key, Revision, SHA256, Metadata string
	Current                                                     bool
}

type CheckObservation struct {
	Mode     string `json:"mode,omitempty"`
	Check    string `json:"check"`
	ExitCode int    `json:"exit_code"`
	Verdict  string `json:"verdict"`
	Summary  string `json:"summary"`
}

type GateObservation struct {
	evidence.Gate
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason,omitempty"`
	Required bool   `json:"required"`
}

func lockEvidenceOwner(ctx context.Context, tx pgx.Tx, a Attempt) (string, error) {
	var policy string
	err := tx.QueryRow(ctx, `SELECT r.verification_sha256 FROM workflow_runs r
 JOIN workflow_tasks t ON t.organization_id=r.organization_id AND t.run_id=r.id
 JOIN workflow_attempts a ON a.organization_id=t.organization_id AND a.task_id=t.id
 WHERE r.organization_id=$1 AND r.id=$2 AND t.id=$3 AND a.id=$4
 AND r.state='active' AND t.state='running' AND a.state='running' AND t.active_attempt_id=a.id
 AND a.fence_token=$5 AND a.generation=$6 FOR UPDATE OF r,t,a`,
		a.OrganizationID, a.RunID, a.TaskID, a.ID, a.FenceToken, a.OwnerGeneration).Scan(&policy)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrFenced
	}
	return policy, err
}

func insertEvidence(ctx context.Context, tx pgx.Tx, a Attempt, kind, key, revision, policy string, metadata any, content []byte) error {
	if !commitPattern.MatchString(revision) || !evidence.ID.MatchString(key) || len(content) > evidence.MaxArtifact {
		return ErrInvalid
	}
	if content == nil {
		content = []byte{}
	}
	data, err := json.Marshal(metadata)
	if err != nil || len(data) > 64<<10 {
		return ErrInvalid
	}
	var oldRevision, oldPolicy, oldSHA string
	var oldJSON []byte
	err = tx.QueryRow(ctx, `SELECT revision,policy_sha256,sha256,metadata FROM workflow_evidence
 WHERE organization_id=$1 AND attempt_id=$2 AND kind=$3 AND origin_key=$4`, a.OrganizationID, a.ID, kind, key).Scan(&oldRevision, &oldPolicy, &oldSHA, &oldJSON)
	if err == nil {
		var old, new any
		_ = json.Unmarshal(oldJSON, &old)
		_ = json.Unmarshal(data, &new)
		oldBytes, _ := json.Marshal(old)
		newBytes, _ := json.Marshal(new)
		if oldRevision != revision || oldPolicy != policy || oldSHA != sha(content) || !bytes.Equal(oldBytes, newBytes) {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM workflow_evidence WHERE organization_id=$1 AND attempt_id=$2 AND kind=$3`, a.OrganizationID, a.ID, kind).Scan(&count); err != nil {
		return err
	}
	cap := 64
	if kind == "artifact" {
		cap = evidence.MaxArtifacts
	}
	if count >= cap {
		return ErrInvalid
	}
	_, err = tx.Exec(ctx, `INSERT INTO workflow_evidence(organization_id,run_id,task_id,attempt_id,kind,origin_key,revision,policy_sha256,metadata,content,sha256)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, a.OrganizationID, a.RunID, a.TaskID, a.ID, kind, key, revision, policy, data, content, sha(content))
	if err != nil {
		return err
	}
	if err = event(ctx, tx, a.OrganizationID, a.RunID, a.TaskID, a.ID, kind+".recorded"); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO identity_audit_events(organization_id,actor_kind,action,subject_id) VALUES($1,'system',$2,$3)`, a.OrganizationID, "workflow."+kind+".recorded", a.ID)
	return err
}

func ArtifactMIME(a evidence.Artifact, content []byte) (string, error) {
	if a.Validate() != nil || len(content) > evidence.MaxArtifact || sha(content) != a.SHA256 {
		return "", ErrInvalid
	}
	if a.Renderer == "image" {
		cfg, format, err := image.DecodeConfig(bytes.NewReader(content))
		if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 40_000_000 {
			return "", ErrInvalid
		}
		return "image/" + format, nil
	}
	if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		return "", ErrInvalid
	}
	if (a.Renderer == "json" || a.Renderer == "table") && !json.Valid(content) {
		return "", ErrInvalid
	}
	return "text/plain; charset=utf-8", nil
}
func (s *Store) RecordArtifact(ctx context.Context, a Attempt, revision string, artifact evidence.Artifact, content []byte) error {
	if _, err := ArtifactMIME(artifact, content); err != nil {
		return err
	}
	ctx = tenant.Org(ctx, a.OrganizationID)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	policy, err := lockEvidenceOwner(ctx, tx, a)
	if err != nil {
		return err
	}
	if err = insertEvidence(ctx, tx, a, "artifact", artifact.ID, revision, policy, artifact, content); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// GateDeclaration derives authority from the stage's frozen extension, not a
// worker's claim. All declared checks named by the recipe are required.
func GateDeclaration(f FrozenTask, check, verdict string) (bool, error) {
	id, version, _, ok := extension.ParseTemplateRef(f.Stage.Template)
	if !ok {
		return false, ErrInvalid
	}
	for _, ext := range f.Bundle.Extensions {
		if ext.ID != id || ext.Version != version {
			continue
		}
		m, err := ext.Load()
		if err != nil {
			return false, err
		}
		for _, c := range m.Checks {
			if c.ID == check {
				required := slices.Contains(f.Bundle.Recipe.RequiredChecks, check)
				if !slices.Contains(c.Verdicts, verdict) {
					return required, ErrInvalid
				}
				return required, nil
			}
		}
	}
	return false, ErrInvalid
}
func (s *Store) RecordGate(ctx context.Context, a Attempt, revision string, g evidence.Gate, reason string) (evidence.Receipt, error) {
	receipt := evidence.Receipt{GateID: g.ID}
	if g.Validate() != nil {
		return receipt, ErrInvalid
	}
	f, err := s.LoadFrozenTask(ctx, a.OrganizationID, a.RunID, a.TaskID)
	if err != nil {
		return receipt, err
	}
	required, declared := GateDeclaration(f, g.Check, g.Verdict)
	if declared != nil {
		reason = "Check or verdict is not declared by this stage's frozen extension."
	}
	receipt.Accepted, receipt.Reason = reason == "", reason
	ctx = tenant.Org(ctx, a.OrganizationID)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return receipt, err
	}
	defer tx.Rollback(ctx)
	policy, err := lockEvidenceOwner(ctx, tx, a)
	if err != nil {
		return receipt, err
	}
	observation := GateObservation{Gate: g, Accepted: receipt.Accepted, Reason: reason, Required: required}
	if err = insertEvidence(ctx, tx, a, "gate", g.ID, revision, policy, observation, nil); err != nil {
		return receipt, err
	}
	return receipt, tx.Commit(ctx)
}

func (s *Store) ListEvidence(ctx context.Context, org, run string) ([]EvidenceRecord, error) {
	return s.ListEvidencePage(ctx, org, run, "", 100)
}
func (s *Store) ListEvidencePage(ctx context.Context, org, run, after string, limit int) ([]EvidenceRecord, error) {
	if after == "" {
		after = zeroUUID
	}
	if !ids(after) || limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	if !ids(org, run) {
		return nil, ErrInvalid
	}
	ctx = tenant.Org(ctx, org)
	rows, err := s.pool.Query(ctx, `SELECT e.id,e.attempt_id,t.task_key,e.kind,e.origin_key,e.revision,e.sha256,e.metadata::text,
 (a.generation=t.generation AND t.state IN ('running','succeeded'))
 FROM workflow_evidence e JOIN workflow_tasks t ON t.organization_id=e.organization_id AND t.id=e.task_id
 JOIN workflow_attempts a ON a.organization_id=e.organization_id AND a.id=e.attempt_id
 WHERE e.organization_id=$1 AND e.run_id=$2 AND e.id>$3 ORDER BY e.id LIMIT $4`, org, run, after, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EvidenceRecord, error) {
		var e EvidenceRecord
		err := r.Scan(&e.ID, &e.AttemptID, &e.Stage, &e.Kind, &e.Key, &e.Revision, &e.SHA256, &e.Metadata, &e.Current)
		return e, err
	})
}
func (s *Store) EvidenceContent(ctx context.Context, org, run, id string) ([]byte, string, error) {
	if !ids(org, run, id) {
		return nil, "", ErrInvalid
	}
	ctx = tenant.Org(ctx, org)
	var content, metadata []byte
	var kind string
	err := s.pool.QueryRow(ctx, `SELECT kind,content,metadata FROM workflow_evidence WHERE organization_id=$1 AND run_id=$2 AND id=$3`, org, run, id).Scan(&kind, &content, &metadata)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	mime := "text/plain; charset=utf-8"
	if kind == "artifact" {
		var a evidence.Artifact
		if json.Unmarshal(metadata, &a) != nil {
			return nil, "", ErrInvalid
		}
		mime, err = ArtifactMIME(a, content)
	}
	return content, mime, err
}

// RecordVerification is platform-only. It atomically records the complete set
// of checks and a stage result. Guests cannot invoke this through bx or RPC.
func (s *Store) RecordVerification(ctx context.Context, a Attempt, revision string, checks []CheckObservation, outputs [][]byte) error {
	f, err := s.LoadFrozenTask(ctx, a.OrganizationID, a.RunID, a.TaskID)
	if err != nil {
		return err
	}
	active := f.Verification.ActiveChecks()
	if f.Stage.Kind != "verify" || len(checks) != len(active) || len(outputs) != len(checks) {
		return ErrInvalid
	}
	input, err := s.LoadInputCommit(ctx, a)
	if err != nil {
		return err
	}
	if revision != input {
		return ErrConflict
	}
	verdict := "pass"
	for i, c := range checks {
		if c.Check != active[i].ID || (c.Verdict != "pass" && c.Verdict != "fail" && c.Verdict != "blocked") ||
			(c.Verdict == "pass" && c.ExitCode != 0) || len(outputs[i]) > 64<<10 {
			return ErrInvalid
		}
		if c.Verdict != "pass" && active[i].Required() {
			verdict = "fail"
		}
	}
	ctx = tenant.Org(ctx, a.OrganizationID)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	policy, err := lockEvidenceOwner(ctx, tx, a)
	if err != nil {
		return err
	}
	var started bool
	if err = tx.QueryRow(ctx, `SELECT verification_started AND control_generation=0 AND NOT EXISTS(SELECT 1 FROM access_leases l
 WHERE l.organization_id=$1::uuid::text AND l.attempt_id=$2::uuid::text AND l.capability='model.invoke') FROM workflow_attempts WHERE organization_id=$1 AND id=$2`, a.OrganizationID, a.ID).Scan(&started); err != nil {
		return err
	}
	if !started {
		return ErrConflict
	}
	for i, c := range checks {
		c.Mode = active[i].Mode
		if err = insertEvidence(ctx, tx, a, "verification", c.Check, revision, policy, c, outputs[i]); err != nil {
			return err
		}
	}
	result := AttemptResult{Summary: verificationSummary(revision, active, checks, outputs), Revision: revision, Verdict: verdict}
	body, _ := json.Marshal(result)
	_, err = tx.Exec(ctx, `INSERT INTO workflow_attempt_results(organization_id,attempt_id,run_id,task_id,summary,revision,verdict,result_sha256)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(organization_id,attempt_id) DO NOTHING`, a.OrganizationID, a.ID, a.RunID, a.TaskID, result.Summary, revision, verdict, sha(body))
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Every check stays visible, with the remaining handoff budget shared among
// failures. Raw output remains in immutable evidence; this is repair context.
func verificationSummary(revision string, policy []VerificationCheck, checks []CheckObservation, outputs [][]byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Platform verification at revision %s\n", revision)
	failed := 0
	for _, c := range checks {
		fmt.Fprintf(&b, "%s: %s (exit %d)\n", c.Check, c.Verdict, c.ExitCode)
		if c.Verdict != "pass" {
			failed++
		}
	}
	if failed == 0 {
		return b.String()
	}
	budget := min(8<<10, (maxSummaryBytes-b.Len())/failed)
	for i, c := range checks {
		if c.Verdict == "pass" {
			continue
		}
		command, _ := json.Marshal(policy[i].Command)
		details := fmt.Sprintf("\n%s\nCommand argv: %s\n%s\nUntrusted command output:\n%s\n", c.Check, command, c.Summary, outputs[i])
		b.WriteString(diagnosticExcerpt(details, budget))
	}
	return b.String()
}

// Keep both the setup error and the final diagnostic; normalize bytes only in
// the prompt copy, never in the stored command output.
func diagnosticExcerpt(text string, limit int) string {
	text = strings.ReplaceAll(strings.ToValidUTF8(text, "�"), "\x00", "�")
	if len(text) <= limit {
		return text
	}
	const marker = "\n[...truncated; full output in review evidence...]\n"
	if limit <= len(marker) {
		return truncateUTF8(text, max(0, limit))
	}
	head := (limit - len(marker)) / 2
	tail := len(text) - (limit - len(marker) - head)
	for tail < len(text) && !utf8.RuneStart(text[tail]) {
		tail++
	}
	return truncateUTF8(text, head) + marker + text[tail:]
}

var ErrEvidencePending = errors.New("required evidence is missing, failed, or stale")

// checkReviewEvidence is also called while approving, so legacy review packages
// cannot bypass the new acceptance boundary.
func checkReviewEvidence(ctx context.Context, tx pgx.Tx, org, run, revision, policy string) error {
	var data []byte
	err := tx.QueryRow(ctx, `SELECT verification_json FROM workflow_run_bundles WHERE organization_id=$1 AND run_id=$2`, org, run).Scan(&data)
	if err != nil {
		return ErrEvidencePending
	}
	var p VerificationPolicy
	if json.Unmarshal(data, &p) != nil {
		return ErrEvidencePending
	}
	canonical, validationErr := validateVerification(p, nil)
	if validationErr != nil || sha(canonical) != policy {
		return ErrEvidencePending
	}
	var bundleData []byte
	if err = tx.QueryRow(ctx, `SELECT bundle_json FROM workflow_run_bundles WHERE organization_id=$1 AND run_id=$2`, org, run).Scan(&bundleData); err != nil {
		return err
	}
	var bundle recipe.Bundle
	if json.Unmarshal(bundleData, &bundle) != nil {
		return ErrEvidencePending
	}
	for _, stage := range bundle.Recipe.Stages {
		if stage.ReviewReport != "" && stage.Mode != "advisory" {
			var report []byte
			err := tx.QueryRow(ctx, `SELECT e.content FROM workflow_tasks t
    JOIN workflow_attempts a ON a.organization_id=t.organization_id AND a.task_id=t.id AND a.generation=t.generation
    JOIN workflow_attempt_results r ON r.organization_id=a.organization_id AND r.attempt_id=a.id
    JOIN workflow_evidence e ON e.organization_id=a.organization_id AND e.attempt_id=a.id
    WHERE t.organization_id=$1 AND t.run_id=$2 AND t.task_key=$3 AND t.state='succeeded' AND a.state='succeeded'
    AND r.verdict='pass' AND r.revision=$4 AND e.kind='artifact' AND e.origin_key=$5 AND e.revision=$4 AND e.policy_sha256=$6`, org, run, stage.ID, revision, stage.ReviewReport, policy).Scan(&report)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrEvidencePending
			}
			if err != nil {
				return err
			}
			if _, err := evidence.ParseReview(report, revision, "pass"); err != nil {
				return ErrEvidencePending
			}
		}
		if stage.Template == "" {
			continue
		}
		f := FrozenTask{Bundle: &bundle, Stage: stage}
		for _, check := range bundle.Recipe.RequiredChecks {
			required, _ := GateDeclaration(f, check, "pass")
			if !required {
				continue
			}
			var passed bool
			err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_tasks t JOIN workflow_attempts a
    ON a.organization_id=t.organization_id AND a.task_id=t.id AND a.generation=t.generation
    JOIN workflow_attempt_results r ON r.organization_id=a.organization_id AND r.attempt_id=a.id
    WHERE t.organization_id=$1 AND t.run_id=$2 AND t.task_key=$3 AND t.state='succeeded' AND a.state='succeeded'
    AND EXISTS(SELECT 1 FROM workflow_evidence e WHERE e.organization_id=t.organization_id AND e.attempt_id=a.id
     AND e.kind='gate' AND e.revision=r.revision AND e.metadata->>'check'=$4 AND e.metadata->>'accepted'='true' AND e.metadata->>'verdict'='pass')
    AND NOT EXISTS(SELECT 1 FROM workflow_evidence e WHERE e.organization_id=t.organization_id AND e.attempt_id=a.id
     AND e.kind='gate' AND e.revision=r.revision AND e.metadata->>'check'=$4 AND e.metadata->>'accepted'='true' AND e.metadata->>'verdict'<>'pass'))`, org, run, stage.ID, check).Scan(&passed)
			if err != nil {
				return err
			}
			if !passed {
				return fmt.Errorf("%w: %s/%s", ErrEvidencePending, stage.ID, check)
			}
		}
	}
	for _, c := range p.Checks {
		if !c.Required() {
			continue
		}
		var passed bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_evidence e
   JOIN workflow_tasks t ON t.organization_id=e.organization_id AND t.id=e.task_id
   JOIN workflow_attempts a ON a.organization_id=e.organization_id AND a.id=e.attempt_id
   WHERE e.organization_id=$1 AND e.run_id=$2 AND e.kind='verification' AND e.origin_key=$3
   AND e.revision=$4 AND e.policy_sha256=$5 AND e.metadata->>'verdict'='pass'
   AND a.generation=t.generation AND a.state='succeeded' AND t.state='succeeded')`, org, run, c.ID, revision, policy).Scan(&passed)
		if err != nil {
			return err
		}
		if !passed {
			return fmt.Errorf("%w: %s", ErrEvidencePending, c.ID)
		}
	}
	return nil
}

func (s *Store) GateReceipt(ctx context.Context, a Attempt, g evidence.Gate) (evidence.Receipt, bool, error) {
	ctx = tenant.Org(ctx, a.OrganizationID)
	var data []byte
	err := s.pool.QueryRow(ctx, `SELECT metadata FROM workflow_evidence WHERE organization_id=$1 AND attempt_id=$2 AND kind='gate' AND origin_key=$3`, a.OrganizationID, a.ID, g.ID).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return evidence.Receipt{}, false, nil
	}
	if err != nil {
		return evidence.Receipt{}, false, err
	}
	var old GateObservation
	if json.Unmarshal(data, &old) != nil {
		return evidence.Receipt{}, false, ErrInvalid
	}
	x, _ := json.Marshal(old.Gate)
	y, _ := json.Marshal(g)
	if !bytes.Equal(x, y) {
		return evidence.Receipt{GateID: g.ID, Reason: "Gate id was already used for a different submission."}, true, nil
	}
	return evidence.Receipt{GateID: g.ID, Accepted: old.Accepted, Reason: old.Reason}, true, nil
}

// requiredGates checks each required declaration on this stage; a verdict from
// an earlier attempt or another revision never satisfies it.
func requiredGates(ctx context.Context, tx pgx.Tx, a Attempt, revision string) error {
	bundle, err := runBundle(ctx, tx, a.OrganizationID, a.RunID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var key string
	if err = tx.QueryRow(ctx, `SELECT task_key FROM workflow_tasks WHERE organization_id=$1 AND id=$2`, a.OrganizationID, a.TaskID).Scan(&key); err != nil {
		return err
	}
	stage, ok := findStage(bundle, key)
	if !ok || stage.Template == "" {
		return nil
	}
	f := FrozenTask{Bundle: bundle, Stage: stage}
	for _, check := range bundle.Recipe.RequiredChecks {
		required, _ := GateDeclaration(f, check, "pass")
		if !required {
			continue
		}
		var passed bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_evidence WHERE organization_id=$1 AND attempt_id=$2
   AND kind='gate' AND metadata->>'check'=$3 AND revision=$4 AND (metadata->>'accepted')::boolean
   AND metadata->>'verdict'='pass') AND NOT EXISTS(SELECT 1 FROM workflow_evidence WHERE organization_id=$1 AND attempt_id=$2
   AND kind='gate' AND metadata->>'check'=$3 AND revision=$4 AND (metadata->>'accepted')::boolean
   AND metadata->>'verdict'<>'pass')`, a.OrganizationID, a.ID, check, revision).Scan(&passed)
		if err != nil {
			return err
		}
		if !passed {
			return fmt.Errorf("%w: %s", ErrEvidencePending, check)
		}
	}
	return nil
}

func (s *Store) ClaimVerification(ctx context.Context, a Attempt) (bool, error) {
	ctx = tenant.Org(ctx, a.OrganizationID)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err = lockEvidenceOwner(ctx, tx, a); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `UPDATE workflow_attempts SET verification_started=true WHERE organization_id=$1 AND id=$2 AND NOT verification_started AND control_generation=0
 AND NOT EXISTS(SELECT 1 FROM access_leases l WHERE l.organization_id=$1::uuid::text AND l.attempt_id=$2::uuid::text AND l.capability='model.invoke')`, a.OrganizationID, a.ID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, tx.Commit(ctx)
}

// evidenceDigest includes every durable record, independent of UI pagination.
func (s *Store) evidenceDigest(ctx context.Context, org, run string) (string, error) {
	ctx = tenant.Org(ctx, org)
	rows, err := s.pool.Query(ctx, `SELECT id,attempt_id,kind,origin_key,revision,policy_sha256,metadata::text,sha256 FROM workflow_evidence WHERE organization_id=$1 AND run_id=$2 ORDER BY id`, org, run)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	h := sha256.New()
	encoder := json.NewEncoder(h)
	for rows.Next() {
		var fields [8]string
		if err = rows.Scan(&fields[0], &fields[1], &fields[2], &fields[3], &fields[4], &fields[5], &fields[6], &fields[7]); err != nil {
			return "", err
		}
		if err = encoder.Encode(fields); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), rows.Err()
}

func platformRequiredChecks(bundle *recipe.Bundle) []string {
	var checks []string
	for _, check := range bundle.Recipe.RequiredChecks {
		gate := false
		for _, stage := range bundle.Recipe.Stages {
			required, _ := GateDeclaration(FrozenTask{Bundle: bundle, Stage: stage}, check, "pass")
			gate = gate || required
		}
		if !gate {
			checks = append(checks, check)
		}
	}
	return checks
}
