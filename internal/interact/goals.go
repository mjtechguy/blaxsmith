package interact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

// Goals own conversation before execution. They neither start runs nor change
// frozen run inputs; factories supply questions through this neutral contract.
type Goal struct {
	ID, ProjectID, Title, Brief, FactoryID, FactoryVersion, CreatedBy string
	Revision                                                          int64
	CreatedAt, UpdatedAt                                              time.Time
	Questions                                                         []Record
}
type GoalEntry struct {
	Sequence                            int64
	Kind, QuestionID, Text, PrincipalID string
	OptionIDs                           []string
	CreatedAt                           time.Time
}
type GoalInput struct {
	ProjectID, RequestKey, Title, Brief, FactoryID, FactoryVersion string
	Questions                                                      []Interaction
}
type GoalReply struct {
	GoalID, RequestKey, Kind, QuestionID, Text string
	OptionIDs                                  []string
	ExpectedRevision                           int64
}

func goalText(s string, min, max int) bool { return runes(s, min, max) && !strings.ContainsRune(s, 0) }

func (s *Store) CreateGoal(ctx context.Context, caller identity.Caller, in GoalInput) (string, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	in.Title, in.Brief = strings.TrimSpace(in.Title), strings.TrimSpace(in.Brief)
	if !ids(in.ProjectID) || !goalText(in.RequestKey, 1, 128) || !goalText(in.Title, 1, 160) || !goalText(in.Brief, 1, 16000) || !keyPattern.MatchString(in.FactoryID) || !goalText(in.FactoryVersion, 1, 64) || len(in.Questions) > 16 {
		return "", workflow.ErrInvalid
	}
	seen := map[string]bool{}
	for i := range in.Questions {
		q := &in.Questions[i]
		if q.validate() != nil || q.Kind != "question" || seen[q.ID] {
			return "", workflow.ErrInvalid
		}
		seen[q.ID] = true
	}
	if in.Questions == nil {
		in.Questions = []Interaction{}
	}
	payload, _ := json.Marshal(in)
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	questions, _ := json.Marshal(in.Questions)
	if len(questions) > 256*1024 {
		return "", workflow.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if err = lockRunControl(ctx, tx, caller); err != nil {
		return "", err
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_projects WHERE organization_id=$1 AND id=$2)`, caller.OrganizationID, in.ProjectID).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		return "", workflow.ErrNotFound
	}
	var id, storedDigest string
	err = tx.QueryRow(ctx, `INSERT INTO workflow_goals(organization_id,project_id,request_key,creation_sha256,title,brief,factory_id,factory_version,questions,created_by)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10)
 ON CONFLICT(organization_id,project_id,created_by,request_key) DO NOTHING RETURNING id`, caller.OrganizationID, in.ProjectID, in.RequestKey, digest, in.Title, in.Brief, in.FactoryID, in.FactoryVersion, string(questions), caller.PrincipalID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT id,creation_sha256 FROM workflow_goals WHERE organization_id=$1 AND project_id=$2 AND created_by=$3 AND request_key=$4`, caller.OrganizationID, in.ProjectID, caller.PrincipalID, in.RequestKey).Scan(&id, &storedDigest)
		if err != nil {
			return "", err
		}
		if storedDigest != digest {
			return "", workflow.ErrConflict
		}
	} else if err != nil {
		return "", err
	} else if err = goalAudit(ctx, tx, caller, id, "created"); err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}

const goalColumns = `id::text,project_id::text,title,brief,factory_id,factory_version,revision,created_by::text,created_at,updated_at,questions`

func scanGoal(row pgx.Row) (Goal, error) {
	var g Goal
	var data []byte
	err := row.Scan(&g.ID, &g.ProjectID, &g.Title, &g.Brief, &g.FactoryID, &g.FactoryVersion, &g.Revision, &g.CreatedBy, &g.CreatedAt, &g.UpdatedAt, &data)
	if errors.Is(err, pgx.ErrNoRows) {
		return g, workflow.ErrNotFound
	}
	if err != nil {
		return g, err
	}
	var questions []Interaction
	if err = json.Unmarshal(data, &questions); err != nil {
		return g, err
	}
	for _, q := range questions {
		g.Questions = append(g.Questions, Record{ID: q.ID, State: "open", Interaction: q, CreatedAt: g.CreatedAt})
	}
	return g, nil
}

// ListGoals uses a stable creation cursor. Every page is tenant/project scoped.
func (s *Store) ListGoals(ctx context.Context, org, project, before string) ([]Goal, bool, error) {
	ctx = tenant.Org(ctx, org)
	if !ids(org, project) || (before != "" && !ids(before)) {
		return nil, false, workflow.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT `+goalColumns+` FROM workflow_goals WHERE organization_id=$1 AND project_id=$2
 AND ($3='' OR (created_at,id)<(SELECT created_at,id FROM workflow_goals WHERE organization_id=$1 AND project_id=$2 AND id=NULLIF($3,'')::uuid))
 ORDER BY created_at DESC,id DESC LIMIT 51`, org, project, before)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	goals := []Goal{}
	for rows.Next() {
		g, err := scanGoal(rows)
		if err != nil {
			return nil, false, err
		}
		g.Brief = ""
		g.Questions = nil
		goals = append(goals, g)
	}
	if err = rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(goals) > 50
	if more {
		goals = goals[:50]
	}
	return goals, more, nil
}

const goalEntryColumns = `sequence,kind,question_id,body,principal_id::text,option_ids,created_at`

func scanGoalEntry(row pgx.Row) (GoalEntry, error) {
	var e GoalEntry
	var data []byte
	err := row.Scan(&e.Sequence, &e.Kind, &e.QuestionID, &e.Text, &e.PrincipalID, &data, &e.CreatedAt)
	if err == nil {
		err = json.Unmarshal(data, &e.OptionIDs)
	}
	return e, err
}

// GetGoal returns one consistent revision, current decisions, and a bounded
// history page. Older answers remain immutable and can be paged back to.
func (s *Store) GetGoal(ctx context.Context, org, id string, before int64) (Goal, []GoalEntry, bool, error) {
	ctx = tenant.Org(ctx, org)
	if !ids(org, id) || before < 0 {
		return Goal{}, nil, false, workflow.ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Goal{}, nil, false, err
	}
	defer tx.Rollback(ctx)
	g, err := scanGoal(tx.QueryRow(ctx, `SELECT `+goalColumns+` FROM workflow_goals WHERE organization_id=$1 AND id=$2`, org, id))
	if err != nil {
		return g, nil, false, err
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT ON(question_id) `+goalEntryColumns+` FROM workflow_goal_entries
 WHERE organization_id=$1 AND goal_id=$2 AND kind IN ('answer','deferred') ORDER BY question_id,sequence DESC`, org, id)
	if err != nil {
		return g, nil, false, err
	}
	for rows.Next() {
		e, err := scanGoalEntry(rows)
		if err != nil {
			rows.Close()
			return g, nil, false, err
		}
		for i := range g.Questions {
			q := &g.Questions[i]
			if q.ID != e.QuestionID {
				continue
			}
			q.State = "deferred"
			if e.Kind == "answer" {
				q.State = "answered"
				q.Answer = &Answer{InteractionID: q.ID, OptionIDs: e.OptionIDs, Text: e.Text, AnsweredBy: e.PrincipalID, At: e.CreatedAt}
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return g, nil, false, err
	}
	rows, err = tx.Query(ctx, `SELECT `+goalEntryColumns+` FROM workflow_goal_entries WHERE organization_id=$1 AND goal_id=$2 AND ($3::bigint=0 OR sequence<$3) ORDER BY sequence DESC LIMIT 51`, org, id, before)
	if err != nil {
		return g, nil, false, err
	}
	entries := []GoalEntry{}
	for rows.Next() {
		e, err := scanGoalEntry(rows)
		if err != nil {
			rows.Close()
			return g, nil, false, err
		}
		entries = append(entries, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return g, nil, false, err
	}
	more := len(entries) > 50
	if more {
		entries = entries[:50]
	}
	slices.Reverse(entries)
	return g, entries, more, tx.Commit(ctx)
}

// ReplyGoal appends context or a versioned decision. An old tab cannot overwrite
// a newer decision; a retry with the same key and exact payload is harmless.
func (s *Store) ReplyGoal(ctx context.Context, caller identity.Caller, in GoalReply) error {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	in.Text = strings.TrimSpace(in.Text)
	if !ids(in.GoalID) || !goalText(in.RequestKey, 1, 128) || in.ExpectedRevision < 1 || !goalText(in.Text, 0, MaxAnswerText) || len(in.OptionIDs) > maxOptions || !slices.Contains([]string{"message", "answer", "deferred"}, in.Kind) {
		return workflow.ErrInvalid
	}
	if in.OptionIDs == nil {
		in.OptionIDs = []string{}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockRunControl(ctx, tx, caller); err != nil {
		return err
	}
	g, err := scanGoal(tx.QueryRow(ctx, `SELECT `+goalColumns+` FROM workflow_goals WHERE organization_id=$1 AND id=$2 FOR UPDATE`, caller.OrganizationID, in.GoalID))
	if err != nil {
		return err
	}
	previous, err := scanGoalEntry(tx.QueryRow(ctx, `SELECT `+goalEntryColumns+` FROM workflow_goal_entries WHERE organization_id=$1 AND goal_id=$2 AND principal_id=$3 AND request_key=$4`, caller.OrganizationID, in.GoalID, caller.PrincipalID, in.RequestKey))
	if err == nil {
		if previous.Kind != in.Kind || previous.QuestionID != in.QuestionID || previous.Text != in.Text || !slices.Equal(previous.OptionIDs, in.OptionIDs) {
			return workflow.ErrConflict
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if g.Revision != in.ExpectedRevision {
		return workflow.ErrConflict
	}
	if in.Kind == "message" {
		if in.QuestionID != "" || len(in.OptionIDs) != 0 || in.Text == "" {
			return workflow.ErrInvalid
		}
	} else {
		found := false
		for _, q := range g.Questions {
			if q.ID != in.QuestionID {
				continue
			}
			found = true
			if in.Kind == "deferred" {
				if q.Blocking || len(in.OptionIDs) != 0 || in.Text != "" {
					return workflow.ErrInvalid
				}
			} else {
				if _, err = q.checkAnswer(in.OptionIDs, in.Text); err != nil {
					return err
				}
			}
		}
		if !found {
			return workflow.ErrInvalid
		}
	}
	options, _ := json.Marshal(in.OptionIDs)
	if _, err = tx.Exec(ctx, `INSERT INTO workflow_goal_entries(organization_id,goal_id,sequence,request_key,principal_id,kind,question_id,option_ids,body) VALUES($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9)`, caller.OrganizationID, in.GoalID, g.Revision+1, in.RequestKey, caller.PrincipalID, in.Kind, in.QuestionID, string(options), in.Text); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE workflow_goals SET revision=revision+1,updated_at=clock_timestamp() WHERE organization_id=$1 AND id=$2`, caller.OrganizationID, in.GoalID); err != nil {
		return err
	}
	if err = goalAudit(ctx, tx, caller, in.GoalID, in.Kind); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func goalAudit(ctx context.Context, tx pgx.Tx, caller identity.Caller, id, action string) error {
	_, err := tx.Exec(ctx, `INSERT INTO identity_audit_events(organization_id,actor_kind,actor_id,action,subject_id) VALUES($1,'principal',$2,$3,$4)`, caller.OrganizationID, caller.PrincipalID, "workflow.goal."+action, id)
	return err
}
