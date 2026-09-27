package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/anvil"
	"github.com/mjtechguy/blaxsmith/internal/evidence"

	"github.com/mjtechguy/blaxsmith/internal/interact"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

func (s *workflowService) StartGoalPlanning(ctx context.Context, req *connect.Request[api.StartGoalPlanningRequest]) (*connect.Response[api.StartGoalPlanningResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if !workflow.CanLaunch(caller) {
		return nil, goalError(interact.ErrDenied)
	}
	if !evidence.ID.MatchString(req.Msg.RequestKey) {
		return nil, workflowError(workflow.ErrInvalid)
	}
	snapshot, contextData, err := s.interactions.SnapshotGoal(ctx, caller.OrganizationID, req.Msg.GoalId, req.Msg.ExpectedRevision)
	if err != nil {
		return nil, goalError(err)
	}
	if snapshot.Goal.FactoryID != "anvil" || snapshot.Goal.FactoryVersion != anvil.Version {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this goal belongs to an external or unsupported factory; use its planning adapter"))
	}
	data, files, err := anvil.PlanningRecipe(req.Msg.GoalId, snapshot.Goal.Revision, contextData, recipe.Profile{Connection: req.Msg.ConnectionId, Harness: req.Msg.Harness, Model: req.Msg.Model, Effort: req.Msg.Effort, Instructions: req.Msg.InstructionFiles, Skills: req.Msg.SkillFiles, Inputs: req.Msg.InputsFile, Agent: req.Msg.AgentDefinition}, int(req.Msg.RuntimeSeconds))
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err = s.requireDispatch(ctx); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the run dispatcher is not connected"))
	}
	project := snapshot.Goal.ProjectID
	verification, err := s.store.GetProjectVerification(ctx, caller.OrganizationID, project)
	if err != nil {
		return nil, workflowError(err)
	}
	source, url, fetched, err := s.fetchProjectSource(ctx, caller.OrganizationID, project)
	if err != nil {
		return nil, err
	}
	defer fetched.Close()
	input := recipe.Input{Repo: fetched.Directory, Ref: fetched.Commit, Recipe: anvil.PlanningRecipePath, RecipeData: data, PlatformFiles: files, Scope: req.Msg.Scope}
	bundle, err := s.store.PreviewSource(ctx, caller, project, input)
	if err != nil {
		return nil, workflowError(err)
	}
	prepared := &preparedLaunch{bundle: bundle, repositoryURL: url, hasGitConnection: source.GitConnectionID != "", input: workflow.FrozenRunInput{GoalID: req.Msg.GoalId, OrganizationID: caller.OrganizationID, ProjectID: project, Caller: &caller, SourceRef: source.Ref}}
	if blockers := s.launchBlockers(ctx, prepared); len(blockers) > 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(strings.Join(blockers, " ")))
	}
	run, err := s.store.CreateFrozenRun(ctx, workflow.FrozenRunInput{OrganizationID: caller.OrganizationID, ProjectID: project, LaunchKey: "goal-plan-" + req.Msg.GoalId + "-" + req.Msg.RequestKey,
		GoalID: req.Msg.GoalId, GoalRevision: snapshot.Goal.Revision, Source: input, Verification: verification.Policy, Caller: &caller, SourceRepositoryURL: source.RepositoryURL, SourceRef: source.Ref, VerificationVersion: verification.Version})
	if err != nil {
		return nil, workflowError(err)
	}
	return connect.NewResponse(&api.StartGoalPlanningResponse{Run: runMessage(run)}), nil
}
func (s *workflowService) GetGoalPlans(ctx context.Context, req *connect.Request[api.GetGoalPlansRequest]) (*connect.Response[api.GetGoalPlansResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	if _, _, _, err = s.interactions.GetGoal(ctx, caller.OrganizationID, req.Msg.GoalId, 0); err != nil {
		return nil, goalError(err)
	}
	plans, more, err := s.interactions.GoalPlans(ctx, caller.OrganizationID, req.Msg.GoalId, req.Msg.BeforeVersion)
	if err != nil {
		return nil, goalError(err)
	}
	runs, err := s.interactions.GoalRuns(ctx, caller.OrganizationID, req.Msg.GoalId)
	if err != nil {
		return nil, goalError(err)
	}
	result := &api.GetGoalPlansResponse{}
	for _, p := range plans {
		result.Plans = append(result.Plans, &api.GoalPlan{Version: p.Version, GoalRevision: p.GoalRevision, ContentJson: p.JSON, Sha256: p.SHA256, SourceRunId: p.RunID, SourceEvidenceId: p.EvidenceID, CreatedAt: p.CreatedAt})
	}
	for _, r := range runs {
		result.Runs = append(result.Runs, &api.GoalRun{Id: r.ID, State: r.State, GoalRevision: r.GoalRevision, PlanVersion: r.PlanVersion, CheckpointId: r.CheckpointID})
	}
	if more {
		result.NextBeforeVersion = plans[len(plans)-1].Version
	}
	return connect.NewResponse(result), nil
}
func (s *workflowService) SaveGoalPlan(ctx context.Context, req *connect.Request[api.SaveGoalPlanRequest]) (*connect.Response[api.SaveGoalPlanResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if !workflow.CanLaunch(caller) {
		return nil, goalError(interact.ErrDenied)
	}
	if (req.Msg.ContentJson == "") == (req.Msg.EvidenceId == "") {
		return nil, workflowError(workflow.ErrInvalid)
	}
	snapshot, _, err := s.interactions.SnapshotGoal(ctx, caller.OrganizationID, req.Msg.GoalId, req.Msg.ExpectedGoalRevision)
	if err != nil {
		return nil, goalError(err)
	}
	artifactKey := "factory-plan"
	if snapshot.Goal.FactoryID == "anvil" {
		artifactKey = "anvil-plan"
	}
	if snapshot.Goal.FactoryID == "anvil" && snapshot.Goal.FactoryVersion != anvil.Version {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("unsupported Anvil version"))
	}
	data := []byte(req.Msg.ContentJson)
	runID, evidenceID := "", req.Msg.EvidenceId
	if evidenceID != "" {
		var revision int64
		data, runID, revision, err = s.interactions.PlanEvidence(ctx, caller.OrganizationID, req.Msg.GoalId, evidenceID, artifactKey)
		if err != nil {
			return nil, goalError(err)
		}
		if revision != snapshot.Goal.Revision {
			return nil, goalError(workflow.ErrConflict)
		}
	} else {
		// Edits preserve the source run as provenance, not a claim that the model
		// wrote the edited bytes. Both versions retain their own content digests.
		plans, _, err := s.interactions.GoalPlans(ctx, caller.OrganizationID, req.Msg.GoalId, req.Msg.ExpectedPlanVersion+1)
		if err != nil {
			return nil, goalError(err)
		}
		if len(plans) > 0 && plans[0].Version == req.Msg.ExpectedPlanVersion && plans[0].GoalRevision == snapshot.Goal.Revision {
			runID, evidenceID = plans[0].RunID, plans[0].EvidenceID
		}
	}
	refs := snapshot.References()
	if runID != "" {
		questions, err := s.interactions.ListInteractions(ctx, caller.OrganizationID, runID)
		if err != nil {
			return nil, goalError(err)
		}
		for _, q := range questions {
			if q.State == "answered" {
				refs["interview:"+q.Interaction.ID] = true
			}
		}
	}
	var canonical []byte
	if snapshot.Goal.FactoryID == "anvil" {
		_, canonical, err = anvil.ValidatePlan(data, refs)
	} else {
		canonical, err = canonicalExternalPlan(snapshot.Goal.FactoryID, data)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	version, err := s.interactions.SaveGoalPlan(ctx, caller, interact.GoalPlanInput{GoalID: req.Msg.GoalId, RequestKey: req.Msg.RequestKey, GoalRevision: snapshot.Goal.Revision, ExpectedVersion: req.Msg.ExpectedPlanVersion, Content: canonical, RunID: runID, EvidenceID: evidenceID, ArtifactKey: artifactKey})
	if err != nil {
		return nil, goalError(err)
	}
	return connect.NewResponse(&api.SaveGoalPlanResponse{Version: version}), nil
}

// External factories own semantic validation. The platform checks a bounded,
// namespaced JSON document and binds immutable bytes to exact goal context.
func canonicalExternalPlan(factoryID string, data []byte) ([]byte, error) {
	if len(data) == 0 || len(data) > 256<<10 {
		return nil, workflow.ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var document map[string]any
	if decoder.Decode(&document) != nil || decoder.Decode(new(any)) != io.EOF || document == nil {
		return nil, workflow.ErrInvalid
	}
	schema, ok := document["schema_version"].(string)
	if !ok || len(schema) > 128 || !strings.HasPrefix(schema, factoryID+"/") || len(schema) <= len(factoryID)+1 || strings.ContainsRune(schema, 0) {
		return nil, errors.New("external plan schema_version must start with the goal factory ID followed by /")
	}
	return json.Marshal(document)
}
