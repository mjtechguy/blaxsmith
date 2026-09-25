package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/identity"
)

// Model gateway G3 (docs/model-gateway-plan.md §8, §9): soft budgets and
// threshold alerts. Budgets are estimated USD per UTC calendar month. Alerts
// go to the inbox and the audit log; nothing is ever blocked.

var ErrBudgetExists = errors.New("an active budget already exists for this scope")

// DefaultBudgetThresholds are the §8 defaults, in percent.
var DefaultBudgetThresholds = []int32{50, 80, 100}

const maxBudgetMicros = 100_000_000_000_000 // $100M.

type BudgetInput struct {
	Name, Scope, ProjectID, PrincipalID string
	AmountUSDMicros                     int64
	Thresholds                          []int32
}

type Budget struct {
	ID, Name, Scope, ProjectID, ProjectName, PrincipalID, PrincipalName string
	AmountUSDMicros                                                     int64
	Thresholds, FiredThresholds                                         []int32
	Version                                                             int64
	SpendUSDMicros, ForecastUSDMicros                                   int64
	PeriodStart, PeriodEnd, CreatedAt                                   time.Time
}

type BudgetAlert struct {
	ID, BudgetID, BudgetName, Scope, ProjectID, ProjectName, PrincipalID, PrincipalName string
	ThresholdPct                                                                        int32
	SpendUSDMicros, AmountUSDMicros, ForecastUSDMicros                                  int64
	PeriodStart, CreatedAt                                                              time.Time
	AcknowledgedAt, SnoozedUntil                                                        *time.Time
	AcknowledgedBy                                                                      string
}

// BudgetPeriod is the UTC calendar month containing now: [start, end).
func BudgetPeriod(now time.Time) (time.Time, time.Time) {
	now = now.UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

// BudgetForecast projects month-to-date spend linearly to the end of the
// month (§8 "at this rate"). The first day counts as a whole day so a single
// early request does not extrapolate wildly.
func BudgetForecast(spend int64, now time.Time) int64 {
	start, end := BudgetPeriod(now)
	elapsed := max(now.Sub(start).Hours()/24, 1)
	days := end.Sub(start).Hours() / 24
	return int64(float64(spend)*days/elapsed + 0.5)
}

// crossedThreshold is the highest threshold spend has reached, or 0.
func crossedThreshold(spend, amount int64, thresholds []int32) int32 {
	var out int32
	for _, t := range thresholds {
		// spend/amount >= t/100 without overflow for any sane amount.
		if amount > 0 && float64(spend)*100 >= float64(amount)*float64(t) && t > out {
			out = t
		}
	}
	return out
}

func normalizeThresholds(in []int32) ([]int32, bool) {
	if len(in) == 0 {
		return slices.Clone(DefaultBudgetThresholds), true
	}
	out := slices.Clone(in)
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) != len(in) || len(out) > 6 || out[0] < 1 || out[len(out)-1] > 200 {
		return nil, false
	}
	return out, true
}

// budgetSpend is month-to-date spend for budget row b; $2 and $3 bound the period.
const budgetSpend = `COALESCE((SELECT sum(d.cost_usd_micros) FROM gateway_usage_daily d
	WHERE d.organization_id=b.organization_id AND d.day>=$2::date AND d.day<$3::date
	AND (b.scope<>'project' OR d.project_id=b.project_id)
	AND (b.scope<>'user' OR d.principal_id=b.principal_id::text)),0)::bigint`

const budgetColumns = `b.id::text,b.name,b.scope,COALESCE(b.project_id::text,''),COALESCE(p.name,''),
	COALESCE(b.principal_id::text,''),COALESCE(NULLIF(u.display_name,''),u.username,''),b.amount_usd_micros,b.thresholds,
	b.version,b.created_at,` + budgetSpend + `,
	COALESCE((SELECT array_agg(a.threshold_pct ORDER BY a.threshold_pct) FROM gateway_alerts a
		WHERE a.organization_id=b.organization_id AND a.budget_id=b.id AND a.period_start=$2::date),'{}')
	FROM gateway_budgets b
	LEFT JOIN workflow_projects p ON p.organization_id=b.organization_id AND p.id=b.project_id
	LEFT JOIN identity_principals u ON u.id=b.principal_id`

func scanBudgets(rows pgx.Rows, now time.Time) ([]Budget, error) {
	start, end := BudgetPeriod(now)
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Budget, error) {
		var b Budget
		err := row.Scan(&b.ID, &b.Name, &b.Scope, &b.ProjectID, &b.ProjectName, &b.PrincipalID, &b.PrincipalName,
			&b.AmountUSDMicros, &b.Thresholds, &b.Version, &b.CreatedAt, &b.SpendUSDMicros, &b.FiredThresholds)
		b.ForecastUSDMicros = BudgetForecast(b.SpendUSDMicros, now)
		b.PeriodStart, b.PeriodEnd = start, end
		return b, err
	})
}

func (s *Store) budgetsWhere(ctx context.Context, tx pgx.Tx, org, where string, args ...any) ([]Budget, error) {
	now := time.Now()
	start, end := BudgetPeriod(now)
	rows, err := tx.Query(ctx, `SELECT `+budgetColumns+` WHERE b.organization_id=$1 AND b.archived_at IS NULL `+where+`
		ORDER BY CASE b.scope WHEN 'organization' THEN 0 WHEN 'project' THEN 1 ELSE 2 END, lower(b.name), b.id`,
		append([]any{org, start, end}, args...)...)
	if err != nil {
		return nil, err
	}
	return scanBudgets(rows, now)
}

func budgetsEnabled(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, org string) (bool, error) {
	var enabled bool
	err := q.QueryRow(ctx, `SELECT enabled FROM gateway_budget_settings WHERE organization_id=$1`, org).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return enabled, err
}

type BudgetsView struct {
	GatewayEnabled, BudgetsEnabled bool
	Budgets                        []Budget
}

// BudgetsAs is Admin → Budgets (owners and admins).
func (s *Store) BudgetsAs(ctx context.Context, caller identity.Caller) (BudgetsView, error) {
	tx, err := s.beginScoped(ctx, caller, ScopeOrganization, "")
	if err != nil {
		return BudgetsView{}, adminScopeError(err)
	}
	defer tx.Rollback(ctx)
	var out BudgetsView
	if out.GatewayEnabled, err = s.GatewayEnabled(ctx, caller.OrganizationID); err != nil {
		return out, err
	}
	if out.BudgetsEnabled, err = budgetsEnabled(ctx, tx, caller.OrganizationID); err != nil {
		return out, err
	}
	out.Budgets, err = s.budgetsWhere(ctx, tx, caller.OrganizationID, "")
	return out, err
}

func auditGateway(ctx context.Context, tx pgx.Tx, caller identity.Caller, action, subject, projectID string, detail any) error {
	body, _ := json.Marshal(detail)
	_, err := tx.Exec(ctx, `INSERT INTO identity_audit_events (organization_id,actor_kind,actor_id,action,subject_id,project_id,detail)
		VALUES ($1,'principal',$2,$3,$4,NULLIF($5,'')::uuid,$6)`, caller.OrganizationID, caller.PrincipalID, action, subject, projectID, body)
	return err
}

// SetBudgetsEnabledAs flips the §15.1 "Budgets & alerts" switch (audited old → new).
func (s *Store) SetBudgetsEnabledAs(ctx context.Context, caller identity.Caller, enabled bool) (bool, error) {
	tx, err := s.beginScoped(ctx, caller, ScopeOrganization, "")
	if err != nil {
		return false, adminScopeError(err)
	}
	defer tx.Rollback(ctx)
	var before bool
	err = tx.QueryRow(ctx, `SELECT enabled FROM gateway_budget_settings WHERE organization_id=$1 FOR UPDATE`, caller.OrganizationID).Scan(&before)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if before == enabled {
		return enabled, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO gateway_budget_settings (organization_id,enabled,updated_by) VALUES ($1,$2,$3)
		ON CONFLICT (organization_id) DO UPDATE SET enabled=EXCLUDED.enabled,updated_by=EXCLUDED.updated_by,updated_at=clock_timestamp()`,
		caller.OrganizationID, enabled, caller.PrincipalID); err != nil {
		return false, err
	}
	if err := auditGateway(ctx, tx, caller, "gateway.budgets.settings.updated", caller.OrganizationID, "",
		map[string]any{"old": map[string]bool{"enabled": before}, "new": map[string]bool{"enabled": enabled}}); err != nil {
		return false, err
	}
	return enabled, tx.Commit(ctx)
}

func validBudget(in BudgetInput) (BudgetInput, bool) {
	in.Name = strings.TrimSpace(in.Name)
	thresholds, ok := normalizeThresholds(in.Thresholds)
	in.Thresholds = thresholds
	if !ok || in.Name == "" || len([]rune(in.Name)) > 120 || strings.ContainsAny(in.Name, "\x00\r\n") ||
		in.AmountUSDMicros <= 0 || in.AmountUSDMicros > maxBudgetMicros {
		return in, false
	}
	switch in.Scope {
	case "organization":
		return in, in.ProjectID == "" && in.PrincipalID == ""
	case "project":
		return in, ids(in.ProjectID) && in.PrincipalID == ""
	case "user":
		return in, ids(in.PrincipalID) && in.ProjectID == ""
	}
	return in, false
}

func (s *Store) budgetByID(ctx context.Context, tx pgx.Tx, org, id string) (Budget, error) {
	list, err := s.budgetsWhere(ctx, tx, org, `AND b.id=$4`, id)
	if err != nil {
		return Budget{}, err
	}
	if len(list) == 0 {
		return Budget{}, ErrNotFound
	}
	return list[0], nil
}

func uniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// CreateBudgetAs adds a budget (owners and admins, audited). One active
// budget per organization, project or user.
func (s *Store) CreateBudgetAs(ctx context.Context, caller identity.Caller, in BudgetInput) (Budget, error) {
	in, ok := validBudget(in)
	if !ok {
		return Budget{}, ErrInvalid
	}
	tx, err := s.beginScoped(ctx, caller, ScopeOrganization, "")
	if err != nil {
		return Budget{}, adminScopeError(err)
	}
	defer tx.Rollback(ctx)
	org := caller.OrganizationID
	var exists bool
	switch in.Scope {
	case "project":
		err = tx.QueryRow(ctx, `SELECT true FROM workflow_projects WHERE organization_id=$1 AND id=$2`, org, in.ProjectID).Scan(&exists)
	case "user":
		err = tx.QueryRow(ctx, `SELECT true FROM identity_memberships WHERE organization_id=$1 AND principal_id=$2`, org, in.PrincipalID).Scan(&exists)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Budget{}, ErrNotFound
	} else if err != nil {
		return Budget{}, err
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO gateway_budgets (organization_id,name,scope,project_id,principal_id,amount_usd_micros,thresholds,created_by)
		VALUES ($1,$2,$3,NULLIF($4,'')::uuid,NULLIF($5,'')::uuid,$6,$7,$8) RETURNING id::text`,
		org, in.Name, in.Scope, in.ProjectID, in.PrincipalID, in.AmountUSDMicros, in.Thresholds, caller.PrincipalID).Scan(&id)
	if uniqueViolation(err) {
		return Budget{}, ErrBudgetExists
	} else if err != nil {
		return Budget{}, err
	}
	if err := auditGateway(ctx, tx, caller, "gateway.budget.created", id, in.ProjectID, map[string]any{"name": in.Name,
		"scope": in.Scope, "project_id": in.ProjectID, "principal_id": in.PrincipalID, "amount_usd_micros": in.AmountUSDMicros,
		"thresholds": in.Thresholds}); err != nil {
		return Budget{}, err
	}
	b, err := s.budgetByID(ctx, tx, org, id)
	if err != nil {
		return Budget{}, err
	}
	return b, tx.Commit(ctx)
}

// UpdateBudgetAs changes a budget's name, amount and thresholds with
// optimistic concurrency. Scope and target are fixed; archive and recreate
// to move a budget. Raising the amount does not re-fire thresholds already
// alerted this period.
func (s *Store) UpdateBudgetAs(ctx context.Context, caller identity.Caller, id string, in BudgetInput, expectedVersion int64) (Budget, error) {
	if !ids(id) {
		return Budget{}, ErrInvalid
	}
	tx, err := s.beginScoped(ctx, caller, ScopeOrganization, "")
	if err != nil {
		return Budget{}, adminScopeError(err)
	}
	defer tx.Rollback(ctx)
	org := caller.OrganizationID
	var current BudgetInput
	var version int64
	err = tx.QueryRow(ctx, `SELECT name,scope,COALESCE(project_id::text,''),COALESCE(principal_id::text,''),amount_usd_micros,thresholds,version
		FROM gateway_budgets WHERE organization_id=$1 AND id=$2 AND archived_at IS NULL FOR UPDATE`, org, id).
		Scan(&current.Name, &current.Scope, &current.ProjectID, &current.PrincipalID, &current.AmountUSDMicros, &current.Thresholds, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Budget{}, ErrNotFound
	} else if err != nil {
		return Budget{}, err
	}
	in.Scope, in.ProjectID, in.PrincipalID = current.Scope, current.ProjectID, current.PrincipalID
	in, ok := validBudget(in)
	if !ok {
		return Budget{}, ErrInvalid
	}
	if version != expectedVersion {
		return Budget{}, ErrConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE gateway_budgets SET name=$3,amount_usd_micros=$4,thresholds=$5,version=version+1,updated_at=clock_timestamp()
		WHERE organization_id=$1 AND id=$2`, org, id, in.Name, in.AmountUSDMicros, in.Thresholds); err != nil {
		return Budget{}, err
	}
	old := map[string]any{"name": current.Name, "amount_usd_micros": current.AmountUSDMicros, "thresholds": current.Thresholds}
	next := map[string]any{"name": in.Name, "amount_usd_micros": in.AmountUSDMicros, "thresholds": in.Thresholds}
	if err := auditGateway(ctx, tx, caller, "gateway.budget.updated", id, current.ProjectID, map[string]any{"old": old, "new": next}); err != nil {
		return Budget{}, err
	}
	b, err := s.budgetByID(ctx, tx, org, id)
	if err != nil {
		return Budget{}, err
	}
	return b, tx.Commit(ctx)
}

// ArchiveBudgetAs retires a budget (audited). Its alerts stay in the feed.
func (s *Store) ArchiveBudgetAs(ctx context.Context, caller identity.Caller, id string) error {
	if !ids(id) {
		return ErrInvalid
	}
	tx, err := s.beginScoped(ctx, caller, ScopeOrganization, "")
	if err != nil {
		return adminScopeError(err)
	}
	defer tx.Rollback(ctx)
	var name, projectID string
	err = tx.QueryRow(ctx, `UPDATE gateway_budgets SET archived_at=clock_timestamp(),updated_at=clock_timestamp()
		WHERE organization_id=$1 AND id=$2 AND archived_at IS NULL RETURNING name,COALESCE(project_id::text,'')`,
		caller.OrganizationID, id).Scan(&name, &projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if err := auditGateway(ctx, tx, caller, "gateway.budget.archived", id, projectID, map[string]string{"name": name}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const alertColumns = `a.id::text,a.budget_id::text,b.name,a.scope,COALESCE(a.project_id::text,''),COALESCE(p.name,''),
	COALESCE(a.principal_id::text,''),COALESCE(NULLIF(u.display_name,''),u.username,''),a.threshold_pct,a.spend_usd_micros,
	a.amount_usd_micros,a.forecast_usd_micros,a.period_start,a.created_at,a.acknowledged_at,COALESCE(k.username,''),a.snoozed_until
	FROM gateway_alerts a
	JOIN gateway_budgets b ON b.organization_id=a.organization_id AND b.id=a.budget_id
	LEFT JOIN workflow_projects p ON p.organization_id=a.organization_id AND p.id=a.project_id
	LEFT JOIN identity_principals u ON u.id=a.principal_id
	LEFT JOIN identity_principals k ON k.id=a.acknowledged_by`

func (s *Store) alerts(ctx context.Context, tx pgx.Tx, where string, args ...any) ([]BudgetAlert, error) {
	rows, err := tx.Query(ctx, `SELECT `+alertColumns+` WHERE `+where+` ORDER BY a.created_at DESC, a.id LIMIT 200`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (BudgetAlert, error) {
		var a BudgetAlert
		err := row.Scan(&a.ID, &a.BudgetID, &a.BudgetName, &a.Scope, &a.ProjectID, &a.ProjectName, &a.PrincipalID, &a.PrincipalName,
			&a.ThresholdPct, &a.SpendUSDMicros, &a.AmountUSDMicros, &a.ForecastUSDMicros, &a.PeriodStart, &a.CreatedAt,
			&a.AcknowledgedAt, &a.AcknowledgedBy, &a.SnoozedUntil)
		return a, err
	})
}

// BudgetAlertsAs is Admin → Alerts: every alert in the organization.
func (s *Store) BudgetAlertsAs(ctx context.Context, caller identity.Caller, openOnly bool) ([]BudgetAlert, error) {
	tx, err := s.beginScoped(ctx, caller, ScopeOrganization, "")
	if err != nil {
		return nil, adminScopeError(err)
	}
	defer tx.Rollback(ctx)
	return s.alerts(ctx, tx, `a.organization_id=$1 AND (NOT $2::boolean OR a.acknowledged_at IS NULL)`, caller.OrganizationID, openOnly)
}

// alertRecipient is the SQL test that the caller ($2 principal, $3 org
// owner or admin) receives alert a: owners and admins receive every alert;
// the budget's user their own; project administrators their project's.
const alertRecipient = `($3::boolean OR (a.scope='user' AND a.principal_id::text=$2::text) OR (a.scope='project' AND EXISTS (
	SELECT 1 FROM workflow_project_admins pa WHERE pa.organization_id=a.organization_id AND pa.project_id=a.project_id
	AND pa.principal_id=$2::text)))`

func (s *Store) lockAlertForRecipient(ctx context.Context, tx pgx.Tx, caller identity.Caller, id string) (string, error) {
	if !ids(id) {
		return "", ErrInvalid
	}
	if err := lockCallerSession(ctx, tx, caller, true); err != nil {
		return "", err
	}
	if caller.Role == "viewer" {
		return "", ErrNotFound
	}
	var projectID string
	err := tx.QueryRow(ctx, `SELECT COALESCE(a.project_id::text,'') FROM gateway_alerts a WHERE a.organization_id=$1 AND a.id=$4
		AND `+alertRecipient+` FOR UPDATE OF a`, caller.OrganizationID, caller.PrincipalID, isOrgAdmin(caller), id).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return projectID, err
}

func isOrgAdmin(caller identity.Caller) bool { return caller.Role == "owner" || caller.Role == "admin" }

// AcknowledgeBudgetAlertAs closes an alert for every recipient (audited).
func (s *Store) AcknowledgeBudgetAlertAs(ctx context.Context, caller identity.Caller, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	projectID, err := s.lockAlertForRecipient(ctx, tx, caller, id)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE gateway_alerts SET acknowledged_at=clock_timestamp(),acknowledged_by=$3
		WHERE organization_id=$1 AND id=$2 AND acknowledged_at IS NULL`, caller.OrganizationID, id, caller.PrincipalID)
	if err != nil || tag.RowsAffected() == 0 {
		return err // Already acknowledged: nothing to do.
	}
	if err := auditGateway(ctx, tx, caller, "gateway.budget_alert.acknowledged", id, projectID, map[string]string{}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SnoozeBudgetAlertAs hides an open alert from the inbox for 1–720 hours.
func (s *Store) SnoozeBudgetAlertAs(ctx context.Context, caller identity.Caller, id string, hours int) (time.Time, error) {
	if hours < 1 || hours > 720 {
		return time.Time{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback(ctx)
	projectID, err := s.lockAlertForRecipient(ctx, tx, caller, id)
	if err != nil {
		return time.Time{}, err
	}
	var until time.Time
	err = tx.QueryRow(ctx, `UPDATE gateway_alerts SET snoozed_until=clock_timestamp()+make_interval(hours => $3)
		WHERE organization_id=$1 AND id=$2 AND acknowledged_at IS NULL RETURNING snoozed_until`, caller.OrganizationID, id, hours).Scan(&until)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrConflict
	} else if err != nil {
		return time.Time{}, err
	}
	if err := auditGateway(ctx, tx, caller, "gateway.budget_alert.snoozed", id, projectID, map[string]int{"hours": hours}); err != nil {
		return time.Time{}, err
	}
	return until, tx.Commit(ctx)
}

type ProjectUsage struct {
	Enabled                           bool
	From, To                          time.Time
	Totals                            UsageTotals
	Budget                            *Budget
	Series                            []UsagePoint
	ByStage, ByModel, ByUser, TopRuns []UsageSlice
	ByUserHidden                      bool
	Alerts                            []BudgetAlert
}

// ProjectUsageAs is Project → Usage for this month to date: any member of
// the organization. Spend by user is shown to project administrators only.
func (s *Store) ProjectUsageAs(ctx context.Context, caller identity.Caller, projectID string) (ProjectUsage, error) {
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID, projectID) {
		return ProjectUsage{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProjectUsage{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockCallerSession(ctx, tx, caller, true); err != nil {
		return ProjectUsage{}, err
	}
	org := caller.OrganizationID
	if _, err := s.GetProject(ctx, org, projectID); err != nil {
		return ProjectUsage{}, err
	}
	var out ProjectUsage
	if out.Enabled, err = s.GatewayEnabled(ctx, org); err != nil {
		return out, err
	}
	now := time.Now()
	start, end := BudgetPeriod(now)
	out.From, out.To = start, now.UTC().Truncate(24*time.Hour)
	if out.Totals, err = scanTotals(tx.QueryRow(ctx, `SELECT `+totalsColumns+` FROM gateway_usage_daily
		WHERE organization_id=$1 AND project_id=$2 AND day>=$3 AND day<$4`, org, projectID, start, end)); err != nil {
		return out, err
	}
	budgets, err := s.budgetsWhere(ctx, tx, org, `AND b.scope='project' AND b.project_id=$4`, projectID)
	if err != nil {
		return out, err
	}
	if len(budgets) > 0 {
		out.Budget = &budgets[0]
	}
	rows, err := tx.Query(ctx, `SELECT to_char(day,'YYYY-MM-DD'),model,sum(cost_usd_micros),
		sum(input_tokens+output_tokens+cache_read_tokens+cache_write_tokens)
		FROM gateway_usage_daily WHERE organization_id=$1 AND project_id=$2 AND day>=$3 AND day<$4 GROUP BY 1,2 ORDER BY 1,2`,
		org, projectID, start, end)
	if err != nil {
		return out, err
	}
	out.Series, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (UsagePoint, error) {
		var p UsagePoint
		return p, row.Scan(&p.Day, &p.Key, &p.Cost, &p.Tokens)
	})
	if err != nil {
		return out, err
	}
	// Stage kinds come from raw events (kept 90 days, longer than a month).
	if out.ByStage, err = s.slices(ctx, tx, `SELECT stage_key,stage_key,'',count(*),count(*) FILTER (WHERE status<>'ok'),
		count(*) FILTER (WHERE http_status=429),sum(input_tokens),sum(output_tokens),sum(cache_read_tokens),
		sum(cache_write_tokens),sum(reasoning_tokens),sum(cost_usd_micros)
		FROM gateway_usage_events WHERE organization_id=$1 AND project_id=$2 AND started_at>=$3 AND started_at<$4
		GROUP BY 1 ORDER BY sum(cost_usd_micros) DESC, 1 LIMIT 50`, org, projectID, start, end); err != nil {
		return out, err
	}
	if out.ByModel, err = s.slices(ctx, tx, `SELECT d.model,d.model,min(d.route_kind),`+totalsColumns+`
		FROM gateway_usage_daily d WHERE d.organization_id=$1 AND d.project_id=$2 AND d.day>=$3 AND d.day<$4
		GROUP BY 1,2 ORDER BY sum(d.cost_usd_micros) DESC, 1 LIMIT 50`, org, projectID, start, end); err != nil {
		return out, err
	}
	admin, err := s.authz.CanAdministerProject(ctx, tx, caller, projectID)
	if err != nil {
		return out, err
	}
	out.ByUser, out.ByUserHidden = []UsageSlice{}, !admin
	if admin {
		if out.ByUser, err = s.slices(ctx, tx, `SELECT d.principal_id,COALESCE(NULLIF(u.display_name,''),u.username,'Unknown user'),
			COALESCE(u.username,''),`+totalsColumns+`
			FROM gateway_usage_daily d LEFT JOIN identity_principals u ON u.id::text=d.principal_id
			WHERE d.organization_id=$1 AND d.project_id=$2 AND d.day>=$3 AND d.day<$4
			GROUP BY 1,2,3 ORDER BY sum(d.cost_usd_micros) DESC, 1 LIMIT 50`, org, projectID, start, end); err != nil {
			return out, err
		}
	}
	if out.TopRuns, err = s.projectTopRuns(ctx, tx, org, projectID, start); err != nil {
		return out, err
	}
	out.Alerts, err = s.alerts(ctx, tx, `a.organization_id=$1 AND a.scope='project' AND a.project_id=$2`, org, projectID)
	return out, err
}

func (s *Store) projectTopRuns(ctx context.Context, tx pgx.Tx, org, projectID string, since time.Time) ([]UsageSlice, error) {
	rows, err := tx.Query(ctx, `SELECT r.project_id::text,u.run_id::text,r.launch_key,COALESCE(p.name,''),
		u.requests,u.errors,0,u.input_tokens,u.output_tokens,u.cache_read_tokens,u.cache_write_tokens,u.reasoning_tokens,u.cost_usd_micros
		FROM gateway_run_usage u JOIN workflow_runs r ON r.organization_id=u.organization_id AND r.id=u.run_id
		LEFT JOIN workflow_projects p ON p.organization_id=r.organization_id AND p.id=r.project_id
		WHERE u.organization_id=$1 AND u.project_id=$2 AND u.last_request_at>=$3
		ORDER BY u.cost_usd_micros DESC, u.run_id LIMIT 10`, org, projectID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageSlice{}
	for rows.Next() {
		var item UsageSlice
		if item.Totals, err = scanTotals(rows, &item.ProjectID, &item.Key, &item.Label, &item.Detail); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// EvaluateBudgets checks every active budget of organizations with the
// gateway and budgets switched on against month-to-date spend from the
// rollups, and records a crossing as one alert plus one audit event in the
// same transaction. Only the highest newly crossed threshold fires (a jump
// from 40% to 90% alerts once, at 80%). The alert's unique key (budget,
// period, threshold) makes this exactly-once under concurrent rollup ticks
// from several gateway replicas: a losing insert does nothing and writes no
// audit event. It returns the number of alerts created.
func EvaluateBudgets(ctx context.Context, pool *pgxpool.Pool, now time.Time) (int, error) {
	start, end := BudgetPeriod(now)
	rows, err := pool.Query(ctx, `SELECT b.organization_id::text,b.id::text,b.name,b.scope,COALESCE(b.project_id::text,''),
		COALESCE(b.principal_id::text,''),b.amount_usd_micros,b.thresholds,`+budgetSpend+`
		FROM gateway_budgets b
		JOIN gateway_org_settings g ON g.organization_id=b.organization_id AND g.enabled
		JOIN gateway_budget_settings s ON s.organization_id=b.organization_id AND s.enabled
		WHERE b.archived_at IS NULL AND b.created_at<=$1::timestamptz`, now, start, end)
	if err != nil {
		return 0, err
	}
	type due struct {
		org, id, name, scope, project, principal string
		amount, spend                            int64
		threshold                                int32
	}
	var pending []due
	for rows.Next() {
		var d due
		var thresholds []int32
		if err := rows.Scan(&d.org, &d.id, &d.name, &d.scope, &d.project, &d.principal, &d.amount, &thresholds, &d.spend); err != nil {
			rows.Close()
			return 0, err
		}
		if d.threshold = crossedThreshold(d.spend, d.amount, thresholds); d.threshold > 0 {
			pending = append(pending, d)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	fired := 0
	for _, d := range pending {
		ok, err := fireBudgetAlert(ctx, pool, d.org, d.id, d.name, d.scope, d.project, d.principal, d.threshold, d.spend, d.amount,
			BudgetForecast(d.spend, now), start)
		if err != nil {
			return fired, fmt.Errorf("budget %s: %w", d.id, err)
		}
		if ok {
			fired++
		}
	}
	return fired, nil
}

func fireBudgetAlert(ctx context.Context, pool *pgxpool.Pool, org, budgetID, name, scope, projectID, principalID string,
	threshold int32, spend, amount, forecast int64, period time.Time) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var alertID string
	err = tx.QueryRow(ctx, `INSERT INTO gateway_alerts (organization_id,budget_id,period_start,threshold_pct,scope,project_id,
		principal_id,spend_usd_micros,amount_usd_micros,forecast_usd_micros)
		SELECT $1,$2,$3,$4,$5,NULLIF($6,'')::uuid,NULLIF($7,'')::uuid,$8,$9,$10
		WHERE NOT EXISTS (SELECT 1 FROM gateway_alerts WHERE organization_id=$1 AND budget_id=$2 AND period_start=$3 AND threshold_pct>=$4)
		ON CONFLICT (organization_id,budget_id,period_start,threshold_pct) DO NOTHING RETURNING id::text`,
		org, budgetID, period, threshold, scope, projectID, principalID, spend, amount, forecast).Scan(&alertID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	detail, _ := json.Marshal(map[string]any{"alert_id": alertID, "budget": name, "scope": scope, "threshold_pct": threshold,
		"spend_usd_micros": spend, "amount_usd_micros": amount, "forecast_usd_micros": forecast,
		"period_start": period.Format("2006-01-02")})
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events (organization_id,actor_kind,action,subject_id,project_id,detail)
		VALUES ($1,'system','gateway.budget.threshold_crossed',$2,NULLIF($3,'')::uuid,$4)`, org, budgetID, projectID, detail); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
