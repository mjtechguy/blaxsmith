package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/anvil"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/interact"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

// This application adapter compiles Anvil data, then uses the same preview and
// admission path as every other factory. The workflow store knows only the
// immutable goal/plan identity, frozen bytes and ordinary recipe stages.
func (s *workflowService) prepareGoalExecution(ctx context.Context, caller identity.Caller, projectID, scope string, options *api.GoalExecutionOptions) (*preparedLaunch, error) {
	if options.PlanVersion < 1 || options.PlanVersion == math.MaxInt64 || options.ExpectedGoalRevision < 1 || scope == "" {
		return nil, workflowError(workflow.ErrInvalid)
	}
	snapshot, _, err := s.interactions.SnapshotGoal(ctx, caller.OrganizationID, options.GoalId, options.ExpectedGoalRevision)
	if err != nil {
		return nil, goalError(err)
	}
	if snapshot.Goal.FactoryID != "anvil" || snapshot.Goal.FactoryVersion != anvil.Version {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("native execution requires a supported Anvil goal; external factories launch their own recipes"))
	}
	if snapshot.Goal.ProjectID != projectID {
		return nil, goalError(workflow.ErrNotFound)
	}
	plans, _, err := s.interactions.GoalPlans(ctx, caller.OrganizationID, options.GoalId, options.PlanVersion+1)
	if err != nil {
		return nil, goalError(err)
	}
	if len(plans) == 0 || plans[0].Version != options.PlanVersion {
		return nil, goalError(workflow.ErrNotFound)
	}
	plan := plans[0]
	if plan.GoalRevision != snapshot.Goal.Revision {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this plan uses earlier goal context; save a revised plan before implementation"))
	}
	sum := sha256.Sum256([]byte(plan.JSON))
	if hex.EncodeToString(sum[:]) != plan.SHA256 {
		return nil, goalError(workflow.ErrConflict)
	}
	refs := snapshot.References()
	var answers []interact.Record
	if plan.RunID != "" {
		records, err := s.interactions.ListInteractions(ctx, caller.OrganizationID, plan.RunID)
		if err != nil {
			return nil, goalError(err)
		}
		for _, q := range records {
			if q.State == "answered" {
				refs["interview:"+q.Interaction.ID] = true
				answers = append(answers, q)
			}
		}
	}
	contextData, err := json.Marshal(struct {
		Goal            interact.GoalSnapshot `json:"goal"`
		PlanningAnswers []interact.Record     `json:"planning_answers"`
	}{snapshot, answers})
	if err != nil {
		return nil, goalError(err)
	}
	execution := anvil.ExecutionInput{ReadinessMode: options.ReadinessMode, GoalID: options.GoalId, GoalRevision: snapshot.Goal.Revision, PlanVersion: plan.Version, PlanJSON: []byte(plan.JSON), Context: contextData, References: refs, Profile: recipe.Profile{Connection: options.ConnectionId, Harness: options.Harness, Model: options.Model, Effort: options.Effort, Instructions: options.InstructionFiles, Skills: options.SkillFiles, Inputs: options.InputsFile, Agent: options.AgentDefinition}, RuntimeSeconds: int(options.RuntimeSeconds), CorrectionCycles: int(options.CorrectionCycles), Acceptance: options.Acceptance}
	if options.Review != nil {
		execution.ReviewMode = options.Review.Mode
		execution.Reviewer = recipe.Profile{Connection: options.Review.ConnectionId, Harness: options.Review.Harness, Model: options.Review.Model, Effort: options.Review.Effort, Instructions: options.Review.InstructionFiles, Skills: options.Review.SkillFiles, Inputs: options.Review.InputsFile, Agent: options.Review.AgentDefinition}
	}
	body, files, packets, err := anvil.ExecutionRecipe(execution)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	verification, err := s.store.GetProjectVerification(ctx, caller.OrganizationID, projectID)
	if err != nil {
		return nil, workflowError(err)
	}
	source, url, fetched, err := s.fetchGoalSource(ctx, caller.OrganizationID, projectID, options.GoalId, options.CheckpointId)
	if err != nil {
		return nil, err
	}
	input := recipe.Input{CheckpointID: options.CheckpointId, Repo: fetched.Directory, Ref: fetched.Commit, Recipe: anvil.ExecutionRecipePath, RecipeData: body, PlatformFiles: files, Scope: scope}
	bundle, err := s.store.PreviewSource(ctx, caller, projectID, input)
	if err != nil {
		fetched.Close()
		return nil, workflowError(err)
	}
	// Keep room for prompt headers and handoffs under dispatch's 1 MiB ceiling.
	size := 0
	for _, artifact := range bundle.Artifacts {
		size += len(artifact.Data)
	}
	if size > 900<<10 {
		fetched.Close()
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("execution instructions exceed the prompt budget; split the goal or narrow its scope"))
	}
	prepared := &preparedLaunch{bundle: bundle, repositoryURL: url, hasGitConnection: source.GitConnectionID != "", close: fetched.Close, input: workflow.FrozenRunInput{CheckpointID: options.CheckpointId, OrganizationID: caller.OrganizationID, ProjectID: projectID, Source: input, Verification: verification.Policy, Caller: &caller, SourceRepositoryURL: source.RepositoryURL, SourceRef: source.Ref, VerificationVersion: verification.Version, GoalID: options.GoalId, GoalRevision: snapshot.Goal.Revision, GoalPlanVersion: plan.Version, GoalPlanSHA256: plan.SHA256}}
	for _, packet := range packets {
		prepared.packets = append(prepared.packets, &api.ExecutionTaskPacket{TaskId: packet.ID, Title: packet.Title, DependsOn: packet.DependsOn, Path: packet.Path, Sha256: packet.SHA256, ContentJson: string(packet.JSON)})
	}
	return prepared, nil
}

// A neutral association freezes context under declared platform document paths.
// Recipes must declare these documents; their original bytes remain unchanged.
func (s *workflowService) resolveFactoryGoal(ctx context.Context, caller identity.Caller, project string, in *api.GoalContext) (workflow.FrozenRunInput, *recipe.Factory, map[string]recipe.PlatformFile, error) {
	var binding workflow.FrozenRunInput
	if in.ExpectedRevision < 1 || in.PlanVersion < 0 || in.PlanVersion == math.MaxInt64 {
		return binding, nil, nil, workflowError(workflow.ErrInvalid)
	}
	snapshot, data, err := s.interactions.SnapshotGoal(ctx, caller.OrganizationID, in.GoalId, in.ExpectedRevision)
	if err != nil {
		return binding, nil, nil, goalError(err)
	}
	if snapshot.Goal.ProjectID != project {
		return binding, nil, nil, goalError(workflow.ErrNotFound)
	}
	binding.CheckpointID = in.CheckpointId
	binding.GoalID = in.GoalId
	binding.GoalRevision = in.ExpectedRevision
	binding.GoalPlanVersion = in.PlanVersion
	files := map[string]recipe.PlatformFile{".blaxsmith/platform/goal.json": {Data: data, Source: fmt.Sprintf("goal:%s@%d", in.GoalId, in.ExpectedRevision)}}
	if in.PlanVersion > 0 {
		plans, _, err := s.interactions.GoalPlans(ctx, caller.OrganizationID, in.GoalId, in.PlanVersion+1)
		if err != nil {
			return binding, nil, nil, goalError(err)
		}
		if len(plans) == 0 || plans[0].Version != in.PlanVersion {
			return binding, nil, nil, goalError(workflow.ErrNotFound)
		}
		plan := plans[0]
		if plan.GoalRevision != in.ExpectedRevision {
			return binding, nil, nil, goalError(workflow.ErrConflict)
		}
		digest := sha256.Sum256([]byte(plan.JSON))
		if hex.EncodeToString(digest[:]) != plan.SHA256 {
			return binding, nil, nil, goalError(workflow.ErrConflict)
		}
		binding.GoalPlanSHA256 = plan.SHA256
		files[".blaxsmith/platform/selected-plan.json"] = recipe.PlatformFile{Data: []byte(plan.JSON), Source: fmt.Sprintf("goal:%s@%d/plan:%d#%s", in.GoalId, in.ExpectedRevision, plan.Version, plan.SHA256)}
	}
	return binding, &recipe.Factory{ID: snapshot.Goal.FactoryID, Version: snapshot.Goal.FactoryVersion}, files, nil
}
