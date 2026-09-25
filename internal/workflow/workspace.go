package workflow

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// Workspace read models for the application shell: Home, Inbox, and all runs.
// Every organization role reads them; CanAct follows what the server lets the
// caller do. Every query is scoped to the caller's organization and bounded.
type WorkspaceRun struct {
	ID, ProjectID, ProjectName, LaunchKey, SourceCommit, State, Status string
	CreatedAt                                                          time.Time
	OpenInteractions, StageCount, StagesSucceeded                      int32
	ReviewWaiting                                                      bool
}

type InboxItem struct {
	ID, Kind, RunID, ProjectID, ProjectName, LaunchKey, Stage, Title string
	Blocking, CanAct                                                 bool
	CreatedAt                                                        time.Time
	// Target is where a budget_alert opens for this caller: my_usage (the
	// budget is the caller's own), project_usage, or admin_alerts.
	Target string
}

type WorkspaceHome struct {
	OrganizationName, OrganizationSlug, Username, DisplayName, Email               string
	WaitingOnYou, OpenItems, RunningAgents, ActiveRuns, RunsLast24h, FailedLast24h int32
	Waiting                                                                        []InboxItem
	Agents                                                                         []AdminLiveAttempt
	RecentRuns                                                                     []WorkspaceRun
}

// InboxFilter and RunFilter page with 1-based Page; zero values take defaults.
type InboxFilter struct {
	Page, PageSize    int
	Kinds             []string
	ProjectID, Search string
	ActionableOnly    bool
}

type RunFilter struct {
	Page, PageSize                   int
	Search                           string
	States                           []string
	ProjectID, SortBy, SortDirection string
}

const workspaceMaxOffset = 10000

var (
	inboxKinds = []string{"question", "approval", "escalation", "interview_round", "review", "budget_alert"}
	runStates  = []string{"queued", "active", "cancel_requested", "cancelled", "failed", "succeeded"}
	runSorts   = map[string]string{"created_at": "r.created_at", "launch_key": `r.launch_key COLLATE "C"`,
		"state": "r.state", "project": `p.name COLLATE "C"`}
)

// workspaceCaller fences a malformed session. Owners, admins, and members
// answer interactions (interact.lockRunControl); owners and admins decide
// reviews (DecideReview); viewers act on nothing.
func workspaceCaller(caller identity.Caller) (answer, review bool, err error) {
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID) {
		return false, false, ErrFenced
	}
	review = caller.Role == "owner" || caller.Role == "admin"
	return review || caller.Role == "member", review, nil
}

// workspacePage validates 1-based paging and returns limit and offset.
func workspacePage(page, size int) (int, int, error) {
	if page == 0 {
		page = 1
	}
	if size == 0 {
		size = 25
	}
	if page < 1 || size < 1 || size > 100 || (page-1)*size > workspaceMaxOffset {
		return 0, 0, ErrInvalid
	}
	return size, (page - 1) * size, nil
}

func validSearch(search string) (string, bool) {
	search = strings.TrimSpace(search)
	return search, len(search) <= 120
}

func validChoices(values, allowed []string) bool {
	if len(values) > len(allowed) {
		return false
	}
	for _, v := range values {
		if !slices.Contains(allowed, v) {
			return false
		}
	}
	return true
}

// inboxItems is the union of open interactions on queued or active runs,
// undecided current review packages on succeeded runs, and open, unsnoozed
// model-gateway budget alerts the caller receives ($8 principal, $9 org owner
// or admin; see alertRecipient). A review blocks the run's completion, so it
// counts as blocking; a budget alert never blocks.
const inboxItems = `WITH items AS (
	SELECT i.id::text AS id,i.kind,i.run_id,r.project_id,p.name AS project_name,r.launch_key,t.task_key AS stage,
		left(COALESCE(i.payload->>'title',''),300) AS title,COALESCE((i.payload->>'blocking')::boolean,false) AS blocking,
		i.created_at,$2::boolean AS can_act,'' AS target
	FROM workflow_interactions i
	JOIN workflow_tasks t ON t.organization_id=i.organization_id AND t.id=i.task_id
	JOIN workflow_runs r ON r.organization_id=i.organization_id AND r.id=i.run_id
	JOIN workflow_projects p ON p.organization_id=r.organization_id AND p.id=r.project_id
	WHERE i.organization_id=$1 AND i.state='open' AND r.state IN ('queued','active')
	UNION ALL
	SELECT k.id::text,'review',r.id,r.project_id,p.name,r.launch_key,'','Review package revision '||k.revision,true,
		k.presented_at,$3::boolean,''
	FROM workflow_runs r
	JOIN workflow_review_packages k ON k.organization_id=r.organization_id AND k.run_id=r.id AND k.id=r.review_package_id
	JOIN workflow_projects p ON p.organization_id=r.organization_id AND p.id=r.project_id
	WHERE r.organization_id=$1 AND r.state='succeeded' AND NOT EXISTS (SELECT 1 FROM workflow_review_decisions d
		WHERE d.organization_id=r.organization_id AND d.run_id=r.id AND d.package_id=r.review_package_id)
	UNION ALL
	SELECT a.id::text,'budget_alert',NULL::uuid,a.project_id,COALESCE(p.name,''),'',a.scope,
		left(b.name||' passed '||a.threshold_pct||'% of its monthly budget',300),false,a.created_at,$2::boolean,
		CASE WHEN a.scope='project' THEN 'project_usage' WHEN a.scope='user' AND a.principal_id::text=$8::text THEN 'my_usage'
			ELSE 'admin_alerts' END
	FROM gateway_alerts a
	JOIN gateway_budgets b ON b.organization_id=a.organization_id AND b.id=a.budget_id
	LEFT JOIN workflow_projects p ON p.organization_id=a.organization_id AND p.id=a.project_id
	WHERE a.organization_id=$1 AND a.acknowledged_at IS NULL AND (a.snoozed_until IS NULL OR a.snoozed_until<=clock_timestamp())
	AND ($9::boolean OR (a.scope='user' AND a.principal_id::text=$8::text) OR (a.scope='project' AND EXISTS (
		SELECT 1 FROM workflow_project_admins pa WHERE pa.organization_id=a.organization_id AND pa.project_id=a.project_id
		AND pa.principal_id=$8::text)))
), filtered AS (
	SELECT * FROM items WHERE (cardinality($4::text[])=0 OR kind=ANY($4::text[]))
	AND ($5='' OR project_id=NULLIF($5,'')::uuid)
	AND ($6='' OR position(lower($6) in lower(title||' '||project_name||' '||launch_key||' '||stage))>0)
	AND (NOT $7::boolean OR can_act)
)`

// ListInbox pages the items a person may need to handle, blocking and oldest
// first, with the total count under the same filters.
func (s *Store) ListInbox(ctx context.Context, caller identity.Caller, f InboxFilter) ([]InboxItem, int32, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	limit, offset, err := workspacePage(f.Page, f.PageSize)
	if err != nil {
		return nil, 0, err
	}
	return s.inbox(ctx, caller, f, limit, offset)
}

// inbox skips the page read when limit is zero and returns only the count.
func (s *Store) inbox(ctx context.Context, caller identity.Caller, f InboxFilter, limit, offset int) ([]InboxItem, int32, error) {
	answer, review, err := workspaceCaller(caller)
	if err != nil {
		return nil, 0, err
	}
	search, ok := validSearch(f.Search)
	if !ok || !validChoices(f.Kinds, inboxKinds) || (f.ProjectID != "" && !ids(f.ProjectID)) {
		return nil, 0, ErrInvalid
	}
	kinds := f.Kinds
	if kinds == nil {
		kinds = []string{}
	}
	args := []any{caller.OrganizationID, answer, review, kinds, f.ProjectID, search, f.ActionableOnly,
		caller.PrincipalID, review}
	var total int32
	if err := s.pool.QueryRow(ctx, inboxItems+` SELECT count(*)::integer FROM filtered`, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	items := []InboxItem{}
	if limit == 0 || total == 0 {
		return items, total, nil
	}
	rows, err := s.pool.Query(ctx, inboxItems+` SELECT id,kind,COALESCE(run_id::text,''),COALESCE(project_id::text,''),
		project_name,launch_key,stage,title,blocking,created_at,can_act,target FROM filtered ORDER BY blocking DESC,created_at,id LIMIT $10 OFFSET $11`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	items, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (InboxItem, error) {
		var i InboxItem
		err := row.Scan(&i.ID, &i.Kind, &i.RunID, &i.ProjectID, &i.ProjectName, &i.LaunchKey, &i.Stage, &i.Title,
			&i.Blocking, &i.CreatedAt, &i.CanAct, &i.Target)
		return i, err
	})
	return items, total, err
}

// ListWorkspaceRuns pages every run in the organization with its project and
// a status rollup that mirrors the browser's runStatus.
func (s *Store) ListWorkspaceRuns(ctx context.Context, caller identity.Caller, f RunFilter) ([]WorkspaceRun, int32, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if _, _, err := workspaceCaller(caller); err != nil {
		return nil, 0, err
	}
	limit, offset, err := workspacePage(f.Page, f.PageSize)
	if err != nil {
		return nil, 0, err
	}
	search, ok := validSearch(f.Search)
	if f.SortBy == "" {
		f.SortBy = "created_at"
	}
	if f.SortDirection == "" {
		f.SortDirection = "desc"
	}
	order, sortOK := runSorts[f.SortBy]
	if !ok || !sortOK || (f.SortDirection != "asc" && f.SortDirection != "desc") ||
		!validChoices(f.States, runStates) || (f.ProjectID != "" && !ids(f.ProjectID)) {
		return nil, 0, ErrInvalid
	}
	states := f.States
	if states == nil {
		states = []string{}
	}
	const where = `FROM workflow_runs r JOIN workflow_projects p ON p.organization_id=r.organization_id AND p.id=r.project_id
		WHERE r.organization_id=$1 AND (cardinality($2::text[])=0 OR r.state=ANY($2::text[]))
		AND ($3='' OR r.project_id=NULLIF($3,'')::uuid)
		AND ($4='' OR position(lower($4) in lower(r.launch_key||' '||r.source_commit||' '||p.name))>0)`
	args := []any{caller.OrganizationID, states, f.ProjectID, search}
	var total int32
	if err := s.pool.QueryRow(ctx, `SELECT count(*)::integer `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []WorkspaceRun{}, 0, nil
	}
	order = fmt.Sprintf("%s %s,r.id %s", order, f.SortDirection, f.SortDirection)
	runs, err := s.workspaceRuns(ctx, fmt.Sprintf(`SELECT r.id,r.project_id,p.name,r.launch_key,r.source_commit,r.state,
		r.created_at,r.review_package_id,r.organization_id,row_number() OVER (ORDER BY %s) AS ord %s
		ORDER BY %s LIMIT $5 OFFSET $6`, order, where, order), append(args, limit, offset)...)
	return runs, total, err
}

// workspaceRuns adds the rollup to a page of runs selected by the inner query
// (id, project_id, name, launch_key, source_commit, state, created_at,
// review_package_id, organization_id, ord); ord keeps the inner order.
func (s *Store) workspaceRuns(ctx context.Context, page string, args ...any) ([]WorkspaceRun, error) {
	rows, err := s.pool.Query(ctx, `SELECT r.id,r.project_id,r.name,r.launch_key,r.source_commit,r.state,r.created_at,
		CASE WHEN COALESCE(i.approval,false) OR rv.waiting THEN 'needs_approval'
			WHEN COALESCE(i.open,0)>0 OR COALESCE(t.escalated,false) THEN 'awaiting_input'
			WHEN COALESCE(t.working,false) OR r.state='active' THEN 'working'
			WHEN r.state='failed' OR COALESCE(t.blocked,false) THEN 'failed'
			WHEN r.state='succeeded' THEN 'done'
			WHEN r.state IN ('queued','cancel_requested','cancelled') THEN r.state ELSE '' END,
		COALESCE(i.open,0),rv.waiting,COALESCE(t.stages,0),COALESCE(t.succeeded,0)
		FROM (`+page+`) r
		LEFT JOIN LATERAL (SELECT count(*)::integer AS stages,count(*) FILTER (WHERE x.state='succeeded')::integer AS succeeded,
			bool_or(x.state='escalated') AS escalated,bool_or(x.state='blocked') AS blocked,
			bool_or(x.state IN ('reserved','starting','running','reconciling')) AS working
			FROM workflow_tasks x WHERE x.organization_id=r.organization_id AND x.run_id=r.id) t ON true
		LEFT JOIN LATERAL (SELECT count(*)::integer AS open,bool_or(x.kind='approval') AS approval
			FROM workflow_interactions x WHERE x.organization_id=r.organization_id AND x.run_id=r.id AND x.state='open') i ON true
		CROSS JOIN LATERAL (SELECT r.state='succeeded' AND r.review_package_id IS NOT NULL AND NOT EXISTS (
			SELECT 1 FROM workflow_review_decisions d WHERE d.organization_id=r.organization_id AND d.run_id=r.id
			AND d.package_id=r.review_package_id) AS waiting) rv
		ORDER BY r.ord`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (WorkspaceRun, error) {
		var r WorkspaceRun
		err := row.Scan(&r.ID, &r.ProjectID, &r.ProjectName, &r.LaunchKey, &r.SourceCommit, &r.State, &r.CreatedAt,
			&r.Status, &r.OpenInteractions, &r.ReviewWaiting, &r.StageCount, &r.StagesSucceeded)
		return r, err
	})
}

// WorkspaceHome reads the caller's landing summary: what waits on them, live
// agents, and recent runs.
func (s *Store) WorkspaceHome(ctx context.Context, caller identity.Caller) (WorkspaceHome, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if _, _, err := workspaceCaller(caller); err != nil {
		return WorkspaceHome{}, err
	}
	org := caller.OrganizationID
	var out WorkspaceHome
	if err := s.pool.QueryRow(ctx, `SELECT o.name,o.slug,p.username,p.display_name,COALESCE(p.email,'')
		FROM identity_organizations o JOIN identity_memberships m ON m.organization_id=o.id
		JOIN identity_principals p ON p.id=m.principal_id WHERE o.id=$1 AND p.id=$2`, org, caller.PrincipalID).
		Scan(&out.OrganizationName, &out.OrganizationSlug, &out.Username, &out.DisplayName, &out.Email); errors.Is(err, pgx.ErrNoRows) {
		return WorkspaceHome{}, ErrFenced
	} else if err != nil {
		return WorkspaceHome{}, err
	}
	var err error
	if out.Waiting, out.WaitingOnYou, err = s.inbox(ctx, caller, InboxFilter{ActionableOnly: true}, 5, 0); err != nil {
		return WorkspaceHome{}, err
	}
	if _, out.OpenItems, err = s.inbox(ctx, caller, InboxFilter{}, 0, 0); err != nil {
		return WorkspaceHome{}, err
	}
	if out.Agents, err = s.liveAttempts(ctx, org, 10); err != nil {
		return WorkspaceHome{}, err
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*)::integer FROM workflow_attempts
		WHERE organization_id=$1 AND state IN ('reserved','starting','running','reconciling')`, org).
		Scan(&out.RunningAgents); err != nil {
		return WorkspaceHome{}, err
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state IN ('queued','active','cancel_requested'))::integer,
		count(*) FILTER (WHERE created_at>clock_timestamp()-interval '24 hours')::integer,
		count(*) FILTER (WHERE created_at>clock_timestamp()-interval '24 hours' AND state='failed')::integer
		FROM workflow_runs WHERE organization_id=$1
		AND (created_at>clock_timestamp()-interval '24 hours' OR state IN ('queued','active','cancel_requested'))`, org).
		Scan(&out.ActiveRuns, &out.RunsLast24h, &out.FailedLast24h); err != nil {
		return WorkspaceHome{}, err
	}
	out.RecentRuns, err = s.workspaceRuns(ctx, `SELECT r.id,r.project_id,p.name,r.launch_key,r.source_commit,r.state,
		r.created_at,r.review_package_id,r.organization_id,row_number() OVER (ORDER BY r.created_at DESC,r.id DESC) AS ord
		FROM workflow_runs r JOIN workflow_projects p ON p.organization_id=r.organization_id AND p.id=r.project_id
		WHERE r.organization_id=$1 ORDER BY r.created_at DESC,r.id DESC LIMIT 8`, org)
	if err != nil {
		return WorkspaceHome{}, err
	}
	return out, nil
}
