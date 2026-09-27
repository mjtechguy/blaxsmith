package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/axbridge"
	"github.com/mjtechguy/blaxsmith/internal/dispatch"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

func TestLaunchBlockersValidateFrozenWorkerInputsPostgres(t *testing.T) {
	pool := terminalTestPool(t)
	ctx := tenant.System(t.Context())
	store, err := workflow.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	var org string
	if err := pool.QueryRow(ctx, `INSERT INTO identity_organizations(id,slug,name) VALUES(gen_random_uuid(),'preflight','Preflight') RETURNING id`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, org, "preflight", "Preflight")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_tool_runtime_approvals
		(organization_id,harness,model,effort,image,binary_path,binary_sha256,version,worker_pool,max_timeout_seconds,max_output_bytes,approved_by)
		VALUES ($1,'codex','gpt-6-luna','xhigh',$2,'/opt/blaxsmith/bin/codex',$3,'0.156.1','pool-a',1800,1048576,'operator')`,
		org, "runner@sha256:"+strings.Repeat("a", 64), strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	s := workflowService{store: store, launchEnabled: true, dispatchReady: func(context.Context) error { return nil },
		dispatcher: &dispatch.Dispatcher{Bridge: &axbridge.Bridge{}}}
	for _, tc := range []struct{ name, prompt, skill, want string }{
		{"oversized", strings.Repeat("x", 130<<10), "", "limit is 122880 bytes"},
		{"malformed skill", "Build", "Missing metadata", "missing skill frontmatter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(tc.prompt)
			sum := sha256.Sum256(data)
			profile := recipe.Profile{Harness: "codex", Model: "gpt-6-luna", Effort: "xhigh"}
			artifacts := []recipe.Artifact{{Path: "task.md", Data: data, SHA256: hex.EncodeToString(sum[:])}}
			if tc.skill != "" {
				data := []byte(tc.skill)
				sum := sha256.Sum256(data)
				profile.Skills = []string{"skills/review/SKILL.md"}
				artifacts = append(artifacts, recipe.Artifact{Path: profile.Skills[0], Data: data, SHA256: hex.EncodeToString(sum[:])})
			}
			prepared := &preparedLaunch{hasGitConnection: true, repositoryURL: "https://github.com/owner/repo",
				input: workflow.FrozenRunInput{OrganizationID: org, ProjectID: project, SourceRef: "main", Caller: &identity.Caller{}},
				bundle: &recipe.Bundle{Source: recipe.Source{Commit: strings.Repeat("a", 40)}, Artifacts: artifacts,
					Recipe: recipe.Recipe{Profiles: map[string]recipe.Profile{"worker": profile}, Limits: recipe.Limits{TimeoutSeconds: 60},
						Stages: []recipe.Stage{{ID: "implement", Kind: "implement", Profile: "worker", Prompt: "task.md"}}}}}
			blockers := s.launchBlockers(ctx, prepared)
			if len(blockers) != 1 || !strings.Contains(blockers[0], `stage "implement"`) || !strings.Contains(blockers[0], tc.want) {
				t.Fatalf("expected actionable %q blocker, got %v", tc.want, blockers)
			}
		})
	}
	var runs, attempts int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM workflow_runs), (SELECT count(*) FROM workflow_attempts)`).Scan(&runs, &attempts); err != nil || runs != 0 || attempts != 0 {
		t.Fatalf("preflight must not admit work: runs=%d attempts=%d err=%v", runs, attempts, err)
	}
}
