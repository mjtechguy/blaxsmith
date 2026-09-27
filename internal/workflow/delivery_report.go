package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

var ErrReportTooLarge = errors.New("delivery report exceeds export limits; use paginated evidence reads")

type DeliveryReport struct {
	Markdown, SHA256 string
	GeneratedAt      time.Time
}

// DeliveryReport reads one database snapshot. No credentials, runtime configuration,
// command argv, raw logs, or artifact bodies enter the export.
func (s *Store) DeliveryReport(ctx context.Context, org, runID string) (DeliveryReport, error) {
	if !ids(org, runID) {
		return DeliveryReport{}, ErrInvalid
	}
	ctx = tenant.Org(ctx, org)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return DeliveryReport{}, err
	}
	defer tx.Rollback(ctx)
	var project, state, source, bundleSHA, policySHA string
	var generated time.Time
	var bundleJSON, policyJSON []byte
	err = tx.QueryRow(ctx, `SELECT r.project_id,r.state,r.source_commit,r.bundle_sha256,r.verification_sha256,b.bundle_json,b.verification_json,transaction_timestamp() FROM workflow_runs r LEFT JOIN workflow_run_bundles b ON b.organization_id=r.organization_id AND b.run_id=r.id WHERE r.organization_id=$1 AND r.id=$2`, org, runID).Scan(&project, &state, &source, &bundleSHA, &policySHA, &bundleJSON, &policyJSON, &generated)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeliveryReport{}, ErrNotFound
	}
	if err != nil {
		return DeliveryReport{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Blaxsmith delivery snapshot\n\nGenerated: %s\n\n- Project: `%s`\n- Run: `%s`\n- Execution state: **%s**\n- Source revision: `%s`\n- Input bundle SHA-256: `%s`\n- Verification policy SHA-256: `%s`\n\n", generated.UTC().Format(time.RFC3339Nano), project, runID, state, source, bundleSHA, policySHA)
	b.WriteString("Execution success, engineering acceptance, merge and deployment are separate facts. This export records the database snapshot above; later decisions can supersede it.\n\n")
	p, err := readCurrentReview(ctx, tx, org, runID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return DeliveryReport{}, err
	}
	b.WriteString("## Acceptance\n\n")
	if errors.Is(err, ErrNotFound) {
		b.WriteString("No current acceptance package. No candidate is claimed as accepted.\n\n")
	} else {
		status := "Awaiting human acceptance"
		if p.AcceptanceMode == "policy" {
			status = "Accepted under the frozen policy"
		} else if p.Decision != nil {
			if p.Decision.Action == "approve" {
				status = "Accepted by human decision"
			} else {
				status = "Changes requested"
			}
		}
		fmt.Fprintf(&b, "%s.\n\n- Candidate revision: `%s`\n- Package: `%s` (revision %d)\n- Evidence digest: `%s`\n- Acceptance mode: `%s`\n\n", status, p.IntegratedCommit, p.ID, p.Revision, p.EvidenceSHA256, p.AcceptanceMode)
		if p.Decision != nil {
			fmt.Fprintf(&b, "Decision `%s`: %s at %s.\n\n", p.Decision.ID, p.Decision.Action, p.Decision.DecidedAt.UTC().Format(time.RFC3339Nano))
		}
	}
	b.WriteString("## Frozen checks\n\n")
	if len(bundleJSON) == 0 {
		b.WriteString("No frozen verification policy is available. No checks are claimed.\n\n")
	} else {
		bundle, policy, err := decodeFrozenBundle(source, bundleSHA, policySHA, bundleJSON, policyJSON)
		if err != nil {
			return DeliveryReport{}, err
		}
		if bundle.Source.CheckpointID != "" {
			fmt.Fprintf(&b, "Source checkpoint: `%s`. Acceptance was checked at admission; this run requires its own evidence and acceptance.\n\n", reportInline(bundle.Source.CheckpointID))
		}
		if bundle.Recipe.Factory != nil {
			fmt.Fprintf(&b, "Factory: `%s` / `%s`.\n\n", bundle.Recipe.Factory.ID, reportInline(bundle.Recipe.Factory.Version))
		}
		fmt.Fprintf(&b, "Check preset: `%s`.\n\n", policy.Preset)
		if len(policy.Checks) == 0 {
			b.WriteString("No automated checks selected.\n\n")
		}
		for _, c := range policy.Checks {
			mode := c.Mode
			if mode == "" {
				mode = "required"
			}
			fmt.Fprintf(&b, "- `%s`: **%s**", c.ID, mode)
			if mode == "off" {
				b.WriteString(" — not run by policy; no pass claimed")
			}
			b.WriteString("\n")
		}

		for _, stage := range bundle.Recipe.Stages {
			if stage.Kind == "review" || stage.Kind == "architect_review" || stage.Kind == "ui_review" {
				mode := stage.Mode
				if mode == "" {
					mode = "required"
				}
				fmt.Fprintf(&b, "- Independent review `%s`: **%s**; report artifact `%s`.\n", stage.ID, mode, stage.ReviewReport)
			}
		}
		b.WriteString("\nRequired checks must pass; advisory findings remain visible. Current project settings may differ from this run’s frozen contract.\n\n")
	}
	// Goal plans are bounded, user-authored project content. Keep them separate
	// from platform acceptance facts; assignment coverage is never test evidence.
	var goalID, title, brief, factory, version string
	var revision, planVersion int64
	var plan []byte
	err = tx.QueryRow(ctx, `SELECT g.id,g.title,g.brief,g.factory_id,g.factory_version,gr.goal_revision,COALESCE(gr.plan_version,0),p.content FROM workflow_goal_runs gr JOIN workflow_goals g ON g.organization_id=gr.organization_id AND g.id=gr.goal_id LEFT JOIN workflow_goal_plans p ON p.organization_id=gr.organization_id AND p.goal_id=gr.goal_id AND p.version=gr.plan_version WHERE gr.organization_id=$1 AND gr.run_id=$2`, org, runID).Scan(&goalID, &title, &brief, &factory, &version, &revision, &planVersion, &plan)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return DeliveryReport{}, err
	}
	if err == nil {
		fmt.Fprintf(&b, "## Goal and plan\n\nGoal `%s`, context revision %d, plan version %d. Factory `%s` / `%s`.\n\n", goalID, revision, planVersion, factory, reportInline(version))
		b.WriteString("The following is project-authored intent, including requirements, assumptions and proposed validation. It does not certify individual task completion.\n\n")
		reportBlock(&b, "text", title+"\n\n"+brief)
		if len(plan) > 0 {
			reportBlock(&b, "json", string(plan))
		}
	}
	b.WriteString("## Stages\n\n")
	rows, err := tx.Query(ctx, `SELECT t.task_key,t.state,t.generation,COALESCE(r.verdict,'') FROM workflow_tasks t LEFT JOIN workflow_attempts a ON a.organization_id=t.organization_id AND a.task_id=t.id AND a.generation=t.generation LEFT JOIN workflow_attempt_results r ON r.organization_id=a.organization_id AND r.attempt_id=a.id WHERE t.organization_id=$1 AND t.run_id=$2 ORDER BY t.created_at,t.id`, org, runID)
	if err != nil {
		return DeliveryReport{}, err
	}
	for rows.Next() {
		var key, status, verdict string
		var generation int64
		if err := rows.Scan(&key, &status, &generation, &verdict); err != nil {
			rows.Close()
			return DeliveryReport{}, err
		}
		fmt.Fprintf(&b, "- `%s`: %s (generation %d); reported verdict: %s\n", key, status, generation, verdict)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return DeliveryReport{}, err
	}
	b.WriteString("\n## Evidence index\n\nReferences require authenticated access to the originating Blaxsmith project and remain subject to its retention policy. Artifact contents and command output are excluded. A prior result stays historical even if a later attempt succeeds.\n\n")
	rows, err = tx.Query(ctx, `SELECT e.id,t.task_key,e.kind,e.origin_key,e.revision,e.policy_sha256,e.sha256,e.metadata,(a.generation=t.generation AND t.state IN ('running','succeeded')) FROM workflow_evidence e JOIN workflow_tasks t ON t.organization_id=e.organization_id AND t.id=e.task_id JOIN workflow_attempts a ON a.organization_id=e.organization_id AND a.id=e.attempt_id WHERE e.organization_id=$1 AND e.run_id=$2 ORDER BY e.id LIMIT 5001`, org, runID)
	if err != nil {
		return DeliveryReport{}, err
	}
	count := 0
	for rows.Next() {
		count++
		if count > 5000 {
			rows.Close()
			return DeliveryReport{}, ErrReportTooLarge
		}
		var id, stage, kind, key, commit, policy, digest string
		var metadata []byte
		var current bool
		if err := rows.Scan(&id, &stage, &kind, &key, &commit, &policy, &digest, &metadata, &current); err != nil {
			rows.Close()
			return DeliveryReport{}, err
		}
		verdict := "collected"
		if kind == "verification" {
			var c CheckObservation
			if json.Unmarshal(metadata, &c) != nil {
				rows.Close()
				return DeliveryReport{}, ErrInvalid
			}
			verdict = c.Verdict
		} else if kind == "gate" {
			var g GateObservation
			if json.Unmarshal(metadata, &g) != nil {
				rows.Close()
				return DeliveryReport{}, ErrInvalid
			}
			if !g.Accepted {
				verdict = "refused"
			} else {
				verdict = g.Verdict
			}
		}
		freshness := "historical attempt"
		if current {
			freshness = "current generation"
		}
		fmt.Fprintf(&b, "- `%s` · %s `%s` · stage `%s` · %s · %s\n  - Revision `%s`; policy `%s`; content `%s`.\n", id, kind, key, stage, verdict, freshness, commit, policy, digest)
		if b.Len() > 2<<20 {
			rows.Close()
			return DeliveryReport{}, ErrReportTooLarge
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return DeliveryReport{}, err
	}
	if count == 0 {
		b.WriteString("No evidence recorded.\n")
	}
	usage, err := usageSummary(ctx, tx, org, runID, false)
	if err != nil {
		return DeliveryReport{}, err
	}
	b.WriteString("\n## Usage, delivery and limits\n\n")
	if usage.Reports == 0 {
		b.WriteString("- Token usage and cost: unknown; no harness reports were collected. Unavailable is not zero.\n")
	} else {
		fmt.Fprintf(&b, "- Harness-reported subtotal: %s input tokens (including cache), %s output tokens (including reasoning). Reports cover %d of %d attempts; this does not establish complete measurement.\n", usage.InputTokens, usage.OutputTokens, usage.ReportedAttempts, usage.Attempts)
		if usage.CostReports == 0 {
			b.WriteString("- Reported cost: unknown.\n")
		} else {
			fmt.Fprintf(&b, "- Harness-estimated cost subtotal: %s micro-USD from %d of %d reports. This is not verified provider billing, subscription usage or a spending cap.\n", usage.CostMicrosUSD, usage.CostReports, usage.Reports)
		}
	}
	b.WriteString("- Interrupted work, human takeover and provider overhead may be missing. Retries and cancelled attempts remain included when reported.\n- Merge / deployment: not established by this snapshot. Consult authorized provider delivery records.\n- Open findings: inspect current and historical evidence above; absence of a required check is not proof of safety.\n- Raw logs, artifact bodies, account credentials, session tokens and runtime configuration are excluded. Goal text and plans are project content; review them before sharing this export.\n")
	if b.Len() > 2<<20 {
		return DeliveryReport{}, ErrReportTooLarge
	}
	result := DeliveryReport{Markdown: b.String(), GeneratedAt: generated}
	result.SHA256 = sha([]byte(result.Markdown))
	return result, tx.Commit(ctx)
}

func reportBlock(b *strings.Builder, language, text string) {
	longest, run := 2, 0
	for _, character := range text {
		if character == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	fmt.Fprintf(b, "%s%s\n%s\n%s\n\n", fence, language, text, fence)
}

func reportInline(value string) string {
	return strings.NewReplacer("&", "&amp;", "`", "&#96;", "<", "&lt;", ">", "&gt;", "\n", " ", "\r", " ").Replace(value)
}
