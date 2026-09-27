package interact

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/evidence"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

// ErrDenied means the caller lacks run-control permission.
var ErrDenied = errors.New("run control denied")

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func ids(values ...string) bool {
	for _, v := range values {
		if !uuidPattern.MatchString(v) {
			return false
		}
	}
	return true
}

// EscalationAnswerFunc receives answers to platform-originated escalations.
// Returning nil marks the answer delivered; an error leaves it for redelivery.
type EscalationAnswerFunc func(ctx context.Context, orgID, runID, stageKey, interactionID string, answer Answer) error

// EscalationSink is what the flow engine uses to ask a human about a stage
// that has no live guest.
type EscalationSink interface {
	Raise(ctx context.Context, orgID, runID, stageKey string, ix Interaction) (string, error)
}

type Store struct {
	pool     *pgxpool.Pool
	mu       sync.RWMutex
	onAnswer EscalationAnswerFunc
	wakeMu   sync.Mutex
	wake     map[string]chan struct{} // attempt ID -> delivery wake-up
}

var _ EscalationSink = (*Store)(nil)

func New(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, workflow.ErrInvalid
	}
	return &Store{pool: pool, wake: map[string]chan struct{}{}}, nil
}

// OnEscalationAnswer registers the engine callback for platform escalations.
func (s *Store) OnEscalationAnswer(fn EscalationAnswerFunc) {
	s.mu.Lock()
	s.onAnswer = fn
	s.mu.Unlock()
}

// AppendEvent writes one workflow_events row under the run's event counter
// with an optional JSON object payload (one implementation: the workflow store's).
func AppendEvent(ctx context.Context, tx pgx.Tx, orgID, runID, taskID, attemptID, kind string, payload []byte) error {
	return workflow.AppendEvent(ctx, tx, orgID, runID, taskID, attemptID, kind, payload)
}

type watchLine struct {
	Usage       *json.RawMessage `json:"usage"`
	Seq         int64            `json:"seq"`
	Interaction *json.RawMessage `json:"interaction"`
	Event       *json.RawMessage `json:"event"`
	Cancelled   *string          `json:"cancelled"`
}

// Cursor is the last persisted guest watch sequence for an attempt.
func (s *Store) Cursor(ctx context.Context, a workflow.Attempt) (int64, error) {
	ctx = tenant.Org(ctx, a.OrganizationID)
	var seq int64
	err := s.pool.QueryRow(ctx, `SELECT seq FROM workflow_interaction_cursors WHERE organization_id=$1 AND attempt_id=$2`,
		a.OrganizationID, a.ID).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return seq, err
}

// Persist stores one `bx watch` line. Lines at or below the attempt's cursor
// are replays and are skipped; the cursor advances in the same transaction.
// A malformed line returns ErrInvalid; the caller logs and continues.
func (s *Store) Persist(ctx context.Context, a workflow.Attempt, raw []byte) error {
	ctx = tenant.Org(ctx, a.OrganizationID)
	var line watchLine
	if len(raw) > 128<<10 || json.Unmarshal(raw, &line) != nil || line.Seq < 1 ||
		!ids(a.OrganizationID, a.RunID, a.TaskID, a.ID) {
		return workflow.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO workflow_interaction_cursors (organization_id,attempt_id,run_id,task_id)
		VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`, a.OrganizationID, a.ID, a.RunID, a.TaskID); err != nil {
		return err
	}
	var cursor int64
	if err := tx.QueryRow(ctx, `SELECT seq FROM workflow_interaction_cursors
		WHERE organization_id=$1 AND attempt_id=$2 FOR UPDATE`, a.OrganizationID, a.ID).Scan(&cursor); err != nil {
		return err
	}
	if line.Seq <= cursor {
		return nil
	}
	// Advance first so a bad line is skipped rather than retried forever.
	if _, err := tx.Exec(ctx, `UPDATE workflow_interaction_cursors SET seq=$3
		WHERE organization_id=$1 AND attempt_id=$2`, a.OrganizationID, a.ID, line.Seq); err != nil {
		return err
	}
	var invalid bool
	switch {
	case line.Usage != nil:
		var u evidence.Usage
		if json.Unmarshal(*line.Usage, &u) != nil || u.Validate() != nil {
			invalid = true
			break
		}
		if err := workflow.RecordUsage(ctx, tx, a, u); err != nil {
			if errors.Is(err, workflow.ErrInvalid) || errors.Is(err, workflow.ErrConflict) {
				invalid = true
				break
			}
			return err
		}
	case line.Interaction != nil:
		var ix Interaction
		if json.Unmarshal(*line.Interaction, &ix) != nil || ix.validate() != nil {
			invalid = true
			break
		}
		payload, _ := json.Marshal(ix)
		tag, err := tx.Exec(ctx, `INSERT INTO workflow_interactions
			(organization_id,run_id,task_id,attempt_id,origin,origin_key,kind,payload)
			VALUES ($1,$2,$3,$4,'guest',$5,$6,$7::jsonb)
			ON CONFLICT (organization_id,attempt_id,origin_key) WHERE origin='guest' DO NOTHING`,
			a.OrganizationID, a.RunID, a.TaskID, a.ID, ix.ID, ix.Kind, string(payload))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			if err := AppendEvent(ctx, tx, a.OrganizationID, a.RunID, a.TaskID, a.ID, "interaction.opened", nil); err != nil {
				return err
			}
		}
	case line.Event != nil:
		payload, err := progressPayload(*line.Event)
		if err != nil {
			invalid = true
			break
		}
		if err := AppendEvent(ctx, tx, a.OrganizationID, a.RunID, a.TaskID, a.ID, "attempt.progress", payload); err != nil {
			return err
		}
	case line.Cancelled != nil:
		tag, err := tx.Exec(ctx, `UPDATE workflow_interactions SET state='cancelled',closed_at=clock_timestamp()
			WHERE organization_id=$1 AND attempt_id=$2 AND origin='guest' AND origin_key=$3 AND state='open'`,
			a.OrganizationID, a.ID, *line.Cancelled)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			// ponytail: no separate "closed" kind in the settled wire shapes;
			// clients refetch on interaction.answered and see state=cancelled.
			if err := AppendEvent(ctx, tx, a.OrganizationID, a.RunID, a.TaskID, a.ID, "interaction.answered", nil); err != nil {
				return err
			}
		}
	default:
		invalid = true
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if invalid {
		return workflow.ErrInvalid
	}
	return nil
}

// CancelOpen closes an attempt's open interactions once the attempt stops.
func (s *Store) CancelOpen(ctx context.Context, a workflow.Attempt) error {
	ctx = tenant.Org(ctx, a.OrganizationID)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE workflow_interactions i SET state='cancelled',closed_at=clock_timestamp()
		FROM workflow_attempts a WHERE a.organization_id=i.organization_id AND a.id=i.attempt_id
		AND i.organization_id=$1 AND i.attempt_id=$2 AND i.state='open' AND a.state<>'running'`, a.OrganizationID, a.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		if err := AppendEvent(ctx, tx, a.OrganizationID, a.RunID, a.TaskID, a.ID, "interaction.answered", nil); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

const recordColumns = `i.id,i.run_id,COALESCE(i.attempt_id::text,''),t.task_key,i.state,i.payload,i.answer,i.created_at`

func scanRecord(row pgx.Row, extra ...any) (Record, error) {
	var r Record
	var payload, answer []byte
	if err := row.Scan(append([]any{&r.ID, &r.RunID, &r.AttemptID, &r.Stage, &r.State, &payload, &answer, &r.CreatedAt}, extra...)...); err != nil {
		return r, err
	}
	if err := json.Unmarshal(payload, &r.Interaction); err != nil {
		return r, err
	}
	if answer != nil {
		r.Answer = &Answer{}
		if err := json.Unmarshal(answer, r.Answer); err != nil {
			return r, err
		}
	}
	return r, nil
}

// ListInteractions returns a run's interactions oldest first. The caller must
// have authorized view access to the run in orgID.
func (s *Store) ListInteractions(ctx context.Context, orgID, runID string) ([]Record, error) {
	ctx = tenant.Org(ctx, orgID)
	if !ids(orgID, runID) {
		return nil, workflow.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT `+recordColumns+` FROM workflow_interactions i
		JOIN workflow_tasks t ON t.organization_id=i.organization_id AND t.id=i.task_id
		WHERE i.organization_id=$1 AND i.run_id=$2 ORDER BY i.created_at,i.id LIMIT 1000`, orgID, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// lockRunControl rechecks the live session and a run-control role (owner,
// admin, member) under database locks, like the model-access admin fence.
func lockRunControl(ctx context.Context, tx pgx.Tx, caller identity.Caller) error {
	if caller.Role != "owner" && caller.Role != "admin" && caller.Role != "member" {
		return ErrDenied
	}
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID) {
		return workflow.ErrFenced
	}
	var role string
	err := tx.QueryRow(ctx, `SELECT m.role FROM identity_sessions s
		JOIN identity_memberships m ON m.organization_id=s.organization_id AND m.principal_id=s.principal_id
		JOIN identity_principals p ON p.id=s.principal_id
		JOIN identity_organizations o ON o.id=s.organization_id
		WHERE s.organization_id=$1 AND s.id=$2 AND s.principal_id=$3
		AND m.role=$4 AND m.role IN ('owner','admin','member') AND $5::timestamptz>clock_timestamp()
		AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
		AND m.state='active' AND p.state='active'
		AND (s.auth_method<>'local' OR o.login_policy IN ('local','mixed'))
		AND (s.credential_kind='service' OR o.mfa_policy<>'required' OR s.mfa_level='totp')
		FOR SHARE OF s,m,p,o`, caller.OrganizationID, caller.SessionID, caller.PrincipalID,
		caller.Role, caller.AccessExpires).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return workflow.ErrFenced
	}
	return err
}

// Answer records the first answer to an open interaction. The answer is
// committed before any delivery; later answers get ErrConflict.
func (s *Store) Answer(ctx context.Context, caller identity.Caller, interactionID string, optionIDs []string, text string) (Record, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if !ids(interactionID) {
		return Record{}, workflow.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Record{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockRunControl(ctx, tx, caller); err != nil {
		return Record{}, err
	}
	var origin, attemptState, taskID string
	current, err := scanRecord(tx.QueryRow(ctx, `SELECT `+recordColumns+`,i.origin,i.task_id,COALESCE(a.state,'')
		FROM workflow_interactions i
		JOIN workflow_tasks t ON t.organization_id=i.organization_id AND t.id=i.task_id
		LEFT JOIN workflow_attempts a ON a.organization_id=i.organization_id AND a.id=i.attempt_id
		WHERE i.organization_id=$1 AND i.id=$2 FOR UPDATE OF i`, caller.OrganizationID, interactionID), &origin, &taskID, &attemptState)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, workflow.ErrNotFound
	}
	if err != nil {
		return Record{}, err
	}
	if current.State != "open" || (origin == "guest" && attemptState != "running") {
		return Record{}, workflow.ErrConflict
	}
	text, err = current.checkAnswer(optionIDs, text)
	if err != nil {
		return Record{}, err
	}
	if optionIDs == nil {
		optionIDs = []string{}
	}
	answer := Answer{InteractionID: current.Interaction.ID, OptionIDs: optionIDs, Text: text,
		AnsweredBy: caller.PrincipalID, At: time.Now().UTC()}
	body, _ := json.Marshal(answer)
	if _, err := tx.Exec(ctx, `UPDATE workflow_interactions SET state='answered',answer=$3::jsonb,answered_by=$4,
		answered_session_id=$5,answered_at=clock_timestamp(),closed_at=clock_timestamp()
		WHERE organization_id=$1 AND id=$2`, caller.OrganizationID, interactionID, string(body),
		caller.PrincipalID, caller.SessionID); err != nil {
		return Record{}, err
	}
	if err := AppendEvent(ctx, tx, caller.OrganizationID, current.RunID, taskID, current.AttemptID, "interaction.answered", nil); err != nil {
		return Record{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events (organization_id,actor_kind,actor_id,action,subject_id)
		VALUES ($1,'principal',$2,'workflow.interaction.answered',$3)`,
		caller.OrganizationID, caller.PrincipalID, interactionID); err != nil {
		return Record{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Record{}, err
	}
	current.State, current.Answer = "answered", &answer
	if current.AttemptID != "" {
		s.Wake(current.AttemptID)
	} else {
		go s.DeliverEscalations(context.WithoutCancel(ctx))
	}
	return current, nil
}

// Steer persists a steering message for a running attempt; the watcher
// delivers it via `bx steer` and redelivers until acknowledged.
func (s *Store) Steer(ctx context.Context, caller identity.Caller, attemptID, kind, text, reason string, n int32) (string, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	text, reason = strings.TrimSpace(text), strings.TrimSpace(reason)
	msg := map[string]any{"kind": kind}
	switch {
	case kind == "instruction" && runes(text, 1, MaxAnswerText) && reason == "" && n == 0:
		msg["text"] = text
	case kind == "pause" && text == "" && reason == "" && n == 0:
	case kind == "halt" && runes(reason, 1, 1000) && text == "" && n == 0:
		msg["reason"] = reason
	case kind == "set_max_cycles" && n >= 1 && n <= 100 && text == "" && reason == "":
		msg["n"] = n
	default:
		return "", workflow.ErrInvalid
	}
	if !ids(attemptID) {
		return "", workflow.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if err := lockRunControl(ctx, tx, caller); err != nil {
		return "", err
	}
	var runID, taskID, state string
	err = tx.QueryRow(ctx, `SELECT run_id,task_id,state FROM workflow_attempts
		WHERE organization_id=$1 AND id=$2 FOR SHARE`, caller.OrganizationID, attemptID).Scan(&runID, &taskID, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", workflow.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if state != "running" {
		return "", workflow.ErrConflict
	}
	var id string
	if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()`).Scan(&id); err != nil {
		return "", err
	}
	msg["id"] = id
	body, _ := json.Marshal(msg)
	if _, err := tx.Exec(ctx, `INSERT INTO workflow_attempt_steers
		(organization_id,id,run_id,task_id,attempt_id,body,principal_id,session_id)
		VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,$8)`, caller.OrganizationID, id, runID, taskID, attemptID,
		string(body), caller.PrincipalID, caller.SessionID); err != nil {
		return "", err
	}
	if err := AppendEvent(ctx, tx, caller.OrganizationID, runID, taskID, attemptID, "attempt.control", nil); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events (organization_id,actor_kind,actor_id,action,subject_id)
		VALUES ($1,'principal',$2,'workflow.attempt.steered',$3)`, caller.OrganizationID, caller.PrincipalID, attemptID); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	s.Wake(attemptID)
	return id, nil
}

// Delivery is one persisted-but-undelivered answer or steer for a guest.
type Delivery struct {
	ID   string // interaction or steer row id
	Argv []string
}

// Pending lists undelivered answers then steers for an attempt, oldest first.
func (s *Store) Pending(ctx context.Context, a workflow.Attempt) ([]Delivery, []Delivery, error) {
	ctx = tenant.Org(ctx, a.OrganizationID)
	var answers, steers []Delivery
	rows, err := s.pool.Query(ctx, `SELECT id,origin_key,answer FROM workflow_interactions
		WHERE organization_id=$1 AND attempt_id=$2 AND state='answered' AND delivered_at IS NULL
		ORDER BY answered_at LIMIT 50`, a.OrganizationID, a.ID)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id, key, answer string
		if err := rows.Scan(&id, &key, &answer); err != nil {
			rows.Close()
			return nil, nil, err
		}
		answers = append(answers, Delivery{ID: id, Argv: []string{"bx", "answer", key, "--json", answer}})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	rows, err = s.pool.Query(ctx, `SELECT id,body FROM workflow_attempt_steers
		WHERE organization_id=$1 AND attempt_id=$2 AND delivered_at IS NULL
		ORDER BY created_at LIMIT 50`, a.OrganizationID, a.ID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, body string
		if err := rows.Scan(&id, &body); err != nil {
			return nil, nil, err
		}
		steers = append(steers, Delivery{ID: id, Argv: []string{"bx", "steer", "--json", body}})
	}
	return answers, steers, rows.Err()
}

func (s *Store) MarkAnswerDelivered(ctx context.Context, orgID, id string) error {
	ctx = tenant.Org(ctx, orgID)
	_, err := s.pool.Exec(ctx, `UPDATE workflow_interactions SET delivered_at=clock_timestamp()
		WHERE organization_id=$1 AND id=$2 AND state='answered' AND delivered_at IS NULL`, orgID, id)
	return err
}

func (s *Store) MarkSteerDelivered(ctx context.Context, orgID, id string) error {
	ctx = tenant.Org(ctx, orgID)
	_, err := s.pool.Exec(ctx, `UPDATE workflow_attempt_steers SET delivered_at=clock_timestamp()
		WHERE organization_id=$1 AND id=$2 AND delivered_at IS NULL`, orgID, id)
	return err
}

// Wake nudges this process's watcher for an attempt to deliver now. Watchers
// also poll, so a wake lost to another replica only adds latency.
func (s *Store) Wake(attemptID string) {
	s.wakeMu.Lock()
	ch := s.wake[attemptID]
	s.wakeMu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (s *Store) subscribe(attemptID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.wakeMu.Lock()
	s.wake[attemptID] = ch
	s.wakeMu.Unlock()
	return ch, func() {
		s.wakeMu.Lock()
		if s.wake[attemptID] == ch {
			delete(s.wake, attemptID)
		}
		s.wakeMu.Unlock()
	}
}

// Raise opens a platform escalation for a stage. A non-empty ix.ID makes the
// call idempotent per (run, stage, id); the existing interaction is returned.
func (s *Store) Raise(ctx context.Context, orgID, runID, stageKey string, ix Interaction) (string, error) {
	ctx = tenant.Org(ctx, orgID)
	ix.Kind = "escalation"
	if ix.ID == "" {
		var b [12]byte
		_, _ = rand.Read(b[:])
		ix.ID = "esc-" + hex.EncodeToString(b[:])
	}
	if !ids(orgID, runID) || ix.validate() != nil {
		return "", workflow.ErrInvalid
	}
	payload, _ := json.Marshal(ix)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var taskID string
	err = tx.QueryRow(ctx, `SELECT id FROM workflow_tasks WHERE organization_id=$1 AND run_id=$2 AND task_key=$3`,
		orgID, runID, stageKey).Scan(&taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", workflow.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO workflow_interactions (organization_id,run_id,task_id,origin,origin_key,kind,payload)
		VALUES ($1,$2,$3,'platform',$4,'escalation',$5::jsonb)
		ON CONFLICT (organization_id,run_id,task_id,origin_key) WHERE origin='platform' DO NOTHING RETURNING id`,
		orgID, runID, taskID, ix.ID, string(payload)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT id FROM workflow_interactions WHERE organization_id=$1 AND run_id=$2
			AND task_id=$3 AND origin='platform' AND origin_key=$4`, orgID, runID, taskID, ix.ID).Scan(&id)
		if err != nil {
			return "", err
		}
		return id, tx.Commit(ctx)
	}
	if err != nil {
		return "", err
	}
	if err := AppendEvent(ctx, tx, orgID, runID, taskID, "", "interaction.opened", nil); err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}

// DeliverEscalations hands answered, undelivered platform escalations to the
// registered engine callback. It is safe to call concurrently: each row is
// claimed with SKIP LOCKED for the duration of its callback.
func (s *Store) DeliverEscalations(ctx context.Context) {
	ctx = tenant.System(ctx)
	s.mu.RLock()
	fn := s.onAnswer
	s.mu.RUnlock()
	if fn == nil {
		return
	}
	for range 50 {
		if !s.deliverOneEscalation(ctx, fn) {
			return
		}
	}
}

func (s *Store) deliverOneEscalation(ctx context.Context, fn EscalationAnswerFunc) bool {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false
	}
	defer tx.Rollback(ctx)
	var orgID, id, runID, stage string
	var body []byte
	err = tx.QueryRow(ctx, `SELECT i.organization_id,i.id,i.run_id,t.task_key,i.answer FROM workflow_interactions i
		JOIN workflow_tasks t ON t.organization_id=i.organization_id AND t.id=i.task_id
		WHERE i.origin='platform' AND i.state='answered' AND i.delivered_at IS NULL
		ORDER BY i.answered_at LIMIT 1 FOR UPDATE OF i SKIP LOCKED`).Scan(&orgID, &id, &runID, &stage, &body)
	if err != nil {
		return false
	}
	var answer Answer
	if json.Unmarshal(body, &answer) != nil {
		return false
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = fn(callCtx, orgID, runID, stage, id, answer)
	cancel()
	if err != nil {
		return false
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_interactions SET delivered_at=clock_timestamp()
		WHERE organization_id=$1 AND id=$2`, orgID, id); err != nil {
		return false
	}
	return tx.Commit(ctx) == nil
}
