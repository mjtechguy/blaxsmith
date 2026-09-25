package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// Model gateway settings and usage reads (docs/model-gateway-plan.md §9,
// §11, §15.1). Costs are estimates. Nothing here reads content: the gateway
// stores counts and metadata only.

var ErrGatewayUnavailable = errors.New("model gateway is not installed on this installation")

type GatewaySettings struct {
	Enabled             bool
	DefaultDeliveryMode string
	AllowProjectChoice  bool
	RemoveDirectEgress  bool
}

type GatewaySettingsView struct {
	Settings          GatewaySettings
	Version           int64
	UpdatedAt         *time.Time
	UpdatedByUsername string
}

var defaultGatewaySettings = GatewaySettings{DefaultDeliveryMode: "native_raw", AllowProjectChoice: true, RemoveDirectEgress: true}

func validDeliveryMode(mode string) bool { return mode == "native_raw" || mode == "brokered_gateway" }

func (s *Store) readGatewaySettings(ctx context.Context, q pgx.Tx, orgID string, lock bool) (GatewaySettingsView, error) {
	view := GatewaySettingsView{Settings: defaultGatewaySettings}
	query := `SELECT g.enabled,g.default_delivery_mode,g.allow_project_choice,g.remove_direct_egress,g.version,g.updated_at,
		COALESCE(p.username,'') FROM gateway_org_settings g LEFT JOIN identity_principals p ON p.id=g.updated_by
		WHERE g.organization_id=$1`
	if lock {
		query += ` FOR UPDATE OF g`
	}
	var at time.Time
	err := q.QueryRow(ctx, query, orgID).Scan(&view.Settings.Enabled, &view.Settings.DefaultDeliveryMode,
		&view.Settings.AllowProjectChoice, &view.Settings.RemoveDirectEgress, &view.Version, &at, &view.UpdatedByUsername)
	if errors.Is(err, pgx.ErrNoRows) {
		return view, nil
	}
	view.UpdatedAt = &at
	return view, err
}

// GatewaySettingsAs reads the organization's switches (owners and admins).
func (s *Store) GatewaySettingsAs(ctx context.Context, caller identity.Caller) (GatewaySettingsView, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	tx, err := s.beginScoped(ctx, caller, ScopeOrganization, "")
	if err != nil {
		return GatewaySettingsView{}, adminScopeError(err)
	}
	defer tx.Rollback(ctx)
	return s.readGatewaySettings(ctx, tx, caller.OrganizationID, false)
}

// UpdateGatewaySettingsAs saves the switches with optimistic concurrency and
// an audit event carrying old → new. The master switch can only be turned on
// when the installation runs the gateway Deployment. Turning it off drains:
// in-flight streams finish; the next request is rejected (gateway authz).
func (s *Store) UpdateGatewaySettingsAs(ctx context.Context, caller identity.Caller, next GatewaySettings,
	expectedVersion int64, installed bool) (GatewaySettingsView, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if !validDeliveryMode(next.DefaultDeliveryMode) || expectedVersion < 0 {
		return GatewaySettingsView{}, ErrInvalid
	}
	tx, err := s.beginScoped(ctx, caller, ScopeOrganization, "")
	if err != nil {
		return GatewaySettingsView{}, adminScopeError(err)
	}
	defer tx.Rollback(ctx)
	current, err := s.readGatewaySettings(ctx, tx, caller.OrganizationID, true)
	if err != nil {
		return GatewaySettingsView{}, err
	}
	if current.Version != expectedVersion {
		return GatewaySettingsView{}, ErrConflict
	}
	if next.Enabled && !current.Settings.Enabled && !installed {
		return GatewaySettingsView{}, ErrGatewayUnavailable
	}
	if next == current.Settings {
		return current, nil
	}
	var version int64
	var at time.Time
	if err := tx.QueryRow(ctx, `INSERT INTO gateway_org_settings
		(organization_id,enabled,default_delivery_mode,allow_project_choice,remove_direct_egress,version,updated_by,updated_at)
		VALUES ($1,$2,$3,$4,$5,1,$6,clock_timestamp())
		ON CONFLICT (organization_id) DO UPDATE SET enabled=EXCLUDED.enabled,
		default_delivery_mode=EXCLUDED.default_delivery_mode, allow_project_choice=EXCLUDED.allow_project_choice,
		remove_direct_egress=EXCLUDED.remove_direct_egress, version=gateway_org_settings.version+1,
		updated_by=EXCLUDED.updated_by, updated_at=EXCLUDED.updated_at
		RETURNING version,updated_at`, caller.OrganizationID, next.Enabled, next.DefaultDeliveryMode,
		next.AllowProjectChoice, next.RemoveDirectEgress, caller.PrincipalID).Scan(&version, &at); err != nil {
		return GatewaySettingsView{}, err
	}
	detail, _ := json.Marshal(map[string]any{"old": settingsJSON(current.Settings), "new": settingsJSON(next)})
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events (organization_id,actor_kind,actor_id,action,subject_id,detail)
		VALUES ($1,'principal',$2,'gateway.settings.updated',$1,$3)`, caller.OrganizationID, caller.PrincipalID, detail); err != nil {
		return GatewaySettingsView{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return GatewaySettingsView{}, err
	}
	return GatewaySettingsView{Settings: next, Version: version, UpdatedAt: &at}, nil
}

func settingsJSON(s GatewaySettings) map[string]any {
	return map[string]any{"enabled": s.Enabled, "default_delivery_mode": s.DefaultDeliveryMode,
		"allow_project_choice": s.AllowProjectChoice, "remove_direct_egress": s.RemoveDirectEgress}
}

func adminScopeError(err error) error {
	if errors.Is(err, ErrConnectionDenied) {
		return ErrAdminDenied
	}
	return err
}

// GatewayEnabled reports the master switch; UI outside the settings page is
// hidden while it is off.
func (s *Store) GatewayEnabled(ctx context.Context, orgID string) (bool, error) {
	ctx = tenant.Org(ctx, orgID)
	var enabled bool
	err := s.pool.QueryRow(ctx, `SELECT enabled FROM gateway_org_settings WHERE organization_id=$1`, orgID).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return enabled, err
}

// ProjectDelivery is the project's delivery mode as the settings page shows it.
type ProjectDelivery struct {
	Effective, ProjectChoice, OrgDefault string
	GatewayEnabled, ChoiceAllowed        bool
	CanEdit                              bool
}

func (s *Store) projectDelivery(ctx context.Context, tx pgx.Tx, caller identity.Caller, projectID string) (ProjectDelivery, error) {
	view, err := s.readGatewaySettings(ctx, tx, caller.OrganizationID, false)
	if err != nil {
		return ProjectDelivery{}, err
	}
	d := ProjectDelivery{OrgDefault: view.Settings.DefaultDeliveryMode, GatewayEnabled: view.Settings.Enabled,
		ChoiceAllowed: view.Settings.AllowProjectChoice}
	var choice string
	err = tx.QueryRow(ctx, `SELECT delivery_mode FROM gateway_project_settings WHERE organization_id=$1 AND project_id=$2`,
		caller.OrganizationID, projectID).Scan(&choice)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return ProjectDelivery{}, err
	}
	d.ProjectChoice = choice
	d.Effective = d.OrgDefault
	if d.ChoiceAllowed && choice != "" {
		d.Effective = choice
	}
	if !d.GatewayEnabled {
		d.Effective = "native_raw"
	}
	d.CanEdit, err = s.authz.CanAdministerProject(ctx, tx, caller, projectID)
	return d, err
}

// ProjectDeliveryAs lets any member read the project's delivery mode.
func (s *Store) ProjectDeliveryAs(ctx context.Context, caller identity.Caller, projectID string) (ProjectDelivery, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if !ids(caller.OrganizationID, projectID) {
		return ProjectDelivery{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProjectDelivery{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockCallerSession(ctx, tx, caller, true); err != nil {
		return ProjectDelivery{}, err
	}
	if _, err := s.GetProject(ctx, caller.OrganizationID, projectID); err != nil {
		return ProjectDelivery{}, err
	}
	return s.projectDelivery(ctx, tx, caller, projectID)
}

// SetProjectDeliveryAs records a project administrator's choice (Project →
// Settings → Model access). Empty clears it back to the organization default.
// Refused while the organization enforces its default.
func (s *Store) SetProjectDeliveryAs(ctx context.Context, caller identity.Caller, projectID, mode string) (ProjectDelivery, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if mode != "" && !validDeliveryMode(mode) {
		return ProjectDelivery{}, ErrInvalid
	}
	tx, err := s.beginScoped(ctx, caller, ScopeProject, projectID)
	if err != nil {
		return ProjectDelivery{}, err
	}
	defer tx.Rollback(ctx)
	before, err := s.projectDelivery(ctx, tx, caller, projectID)
	if err != nil {
		return ProjectDelivery{}, err
	}
	if !before.ChoiceAllowed {
		return ProjectDelivery{}, ErrConnectionDenied
	}
	if mode == "" {
		_, err = tx.Exec(ctx, `DELETE FROM gateway_project_settings WHERE organization_id=$1 AND project_id=$2`,
			caller.OrganizationID, projectID)
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO gateway_project_settings (organization_id,project_id,delivery_mode,updated_by)
			VALUES ($1,$2,$3,$4) ON CONFLICT (organization_id,project_id) DO UPDATE SET
			delivery_mode=EXCLUDED.delivery_mode,updated_by=EXCLUDED.updated_by,updated_at=clock_timestamp()`,
			caller.OrganizationID, projectID, mode, caller.PrincipalID)
	}
	if err != nil {
		return ProjectDelivery{}, err
	}
	detail, _ := json.Marshal(map[string]string{"old": before.ProjectChoice, "new": mode})
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events (organization_id,actor_kind,actor_id,action,subject_id,detail)
		VALUES ($1,'principal',$2,'gateway.project_delivery.updated',$3,$4)`,
		caller.OrganizationID, caller.PrincipalID, projectID, detail); err != nil {
		return ProjectDelivery{}, err
	}
	after, err := s.projectDelivery(ctx, tx, caller, projectID)
	if err != nil {
		return ProjectDelivery{}, err
	}
	return after, tx.Commit(ctx)
}

// UsageTotals are estimated USD micros and token counts.
type UsageTotals struct {
	Requests, Errors, RateLimited                                 int64
	Input, Output, CacheRead, CacheWrite, Reasoning, CostUSDMicro int64
}

type UsageSlice struct {
	Key, Label, Detail, ProjectID string
	Totals                        UsageTotals
}

type UsagePoint struct {
	Day, Key     string
	Cost, Tokens int64
}

type UsageOverview struct {
	Enabled      bool
	From, To     time.Time
	Totals       UsageTotals
	MedianTTFTMS int64
	Series       []UsagePoint
	SeriesLabels []UsageSlice
	TopProjects  []UsageSlice
	TopUsers     []UsageSlice
	TopRuns      []UsageSlice
}

const totalsColumns = `COALESCE(sum(requests),0),COALESCE(sum(errors),0),COALESCE(sum(rate_limited),0),
	COALESCE(sum(input_tokens),0),COALESCE(sum(output_tokens),0),COALESCE(sum(cache_read_tokens),0),
	COALESCE(sum(cache_write_tokens),0),COALESCE(sum(reasoning_tokens),0),COALESCE(sum(cost_usd_micros),0)`

func scanTotals(row interface{ Scan(...any) error }, prefix ...any) (UsageTotals, error) {
	var t UsageTotals
	err := row.Scan(append(prefix, &t.Requests, &t.Errors, &t.RateLimited, &t.Input, &t.Output, &t.CacheRead,
		&t.CacheWrite, &t.Reasoning, &t.CostUSDMicro)...)
	return t, err
}

func usageWindow(days int) (time.Time, time.Time) {
	if days <= 0 {
		days = 30
	}
	days = min(days, 90)
	to := time.Now().UTC().Truncate(24 * time.Hour)
	return to.AddDate(0, 0, -(days - 1)), to
}

func (s *Store) slices(ctx context.Context, tx pgx.Tx, query string, args ...any) ([]UsageSlice, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageSlice{}
	for rows.Next() {
		var item UsageSlice
		t, err := scanTotals(rows, &item.Key, &item.Label, &item.Detail)
		if err != nil {
			return nil, err
		}
		item.Totals = t
		out = append(out, item)
	}
	return out, rows.Err()
}

// UsageOverviewAs is the Admin → Usage & Gateway dashboard (owners, admins).
func (s *Store) UsageOverviewAs(ctx context.Context, caller identity.Caller, days int, seriesBy string) (UsageOverview, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if seriesBy == "" {
		seriesBy = "project"
	}
	if seriesBy != "project" && seriesBy != "model" {
		return UsageOverview{}, ErrInvalid
	}
	tx, err := s.beginScoped(ctx, caller, ScopeOrganization, "")
	if err != nil {
		return UsageOverview{}, adminScopeError(err)
	}
	defer tx.Rollback(ctx)
	org := caller.OrganizationID
	var out UsageOverview
	if out.Enabled, err = s.GatewayEnabled(ctx, org); err != nil {
		return out, err
	}
	out.From, out.To = usageWindow(days)
	if out.Totals, err = scanTotals(tx.QueryRow(ctx, `SELECT `+totalsColumns+` FROM gateway_usage_daily
		WHERE organization_id=$1 AND day BETWEEN $2 AND $3`, org, out.From, out.To)); err != nil {
		return out, err
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(percentile_disc(0.5) WITHIN GROUP (ORDER BY ttft_ms),0)
		FROM gateway_usage_events WHERE organization_id=$1 AND started_at>=$2 AND ttft_ms IS NOT NULL`,
		org, out.From).Scan(&out.MedianTTFTMS); err != nil {
		return out, err
	}
	key := "project_id::text"
	if seriesBy == "model" {
		key = "model"
	}
	rows, err := tx.Query(ctx, `SELECT to_char(day,'YYYY-MM-DD'),`+key+`,sum(cost_usd_micros),
		sum(input_tokens+output_tokens+cache_read_tokens+cache_write_tokens)
		FROM gateway_usage_daily WHERE organization_id=$1 AND day BETWEEN $2 AND $3 GROUP BY 1,2 ORDER BY 1,2`, org, out.From, out.To)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var p UsagePoint
		if err := rows.Scan(&p.Day, &p.Key, &p.Cost, &p.Tokens); err != nil {
			rows.Close()
			return out, err
		}
		out.Series = append(out.Series, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	if out.TopProjects, err = s.slices(ctx, tx, `SELECT d.project_id::text,COALESCE(p.name,'Deleted project'),'',`+totalsColumns+`
		FROM gateway_usage_daily d LEFT JOIN workflow_projects p ON p.organization_id=d.organization_id AND p.id=d.project_id
		WHERE d.organization_id=$1 AND d.day BETWEEN $2 AND $3 GROUP BY 1,2 ORDER BY sum(d.cost_usd_micros) DESC, 1 LIMIT 10`,
		org, out.From, out.To); err != nil {
		return out, err
	}
	if out.TopUsers, err = s.slices(ctx, tx, `SELECT d.principal_id,COALESCE(NULLIF(u.display_name,''),u.username,'Unknown user'),
		COALESCE(u.username,''),`+totalsColumns+`
		FROM gateway_usage_daily d LEFT JOIN identity_principals u ON u.id::text=d.principal_id
		WHERE d.organization_id=$1 AND d.day BETWEEN $2 AND $3 GROUP BY 1,2,3 ORDER BY sum(d.cost_usd_micros) DESC, 1 LIMIT 10`,
		org, out.From, out.To); err != nil {
		return out, err
	}
	if out.TopRuns, err = s.topRuns(ctx, tx, org, "", out.From); err != nil {
		return out, err
	}
	if seriesBy == "project" {
		out.SeriesLabels = out.TopProjects
		seen := map[string]bool{}
		for _, p := range out.TopProjects {
			seen[p.Key] = true
		}
		for _, p := range out.Series { // series keys beyond the top ten still get a name.
			if !seen[p.Key] {
				seen[p.Key] = true
				var name string
				_ = tx.QueryRow(ctx, `SELECT name FROM workflow_projects WHERE organization_id=$1 AND id::text=$2`, org, p.Key).Scan(&name)
				out.SeriesLabels = append(out.SeriesLabels, UsageSlice{Key: p.Key, Label: firstString(name, "Deleted project")})
			}
		}
	}
	return out, nil
}

func firstString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (s *Store) topRuns(ctx context.Context, tx pgx.Tx, org, principal string, since time.Time) ([]UsageSlice, error) {
	rows, err := tx.Query(ctx, `SELECT r.project_id::text,u.run_id::text,r.launch_key,COALESCE(p.name,''),
		u.requests,u.errors,0,u.input_tokens,u.output_tokens,u.cache_read_tokens,u.cache_write_tokens,u.reasoning_tokens,u.cost_usd_micros
		FROM gateway_run_usage u JOIN workflow_runs r ON r.organization_id=u.organization_id AND r.id=u.run_id
		LEFT JOIN workflow_projects p ON p.organization_id=r.organization_id AND p.id=r.project_id
		WHERE u.organization_id=$1 AND ($2='' OR u.principal_id=$2) AND u.last_request_at>=$3
		ORDER BY u.cost_usd_micros DESC, u.run_id LIMIT 10`, org, principal, since)
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

type MyUsage struct {
	Enabled   bool
	From, To  time.Time
	Totals    UsageTotals
	ByProject []UsageSlice
	ByModel   []UsageSlice
	TopRuns   []UsageSlice
}

// MyUsageAs is account menu → My usage: only the caller's own runs.
func (s *Store) MyUsageAs(ctx context.Context, caller identity.Caller, days int) (MyUsage, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID) {
		return MyUsage{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MyUsage{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockCallerSession(ctx, tx, caller, true); err != nil {
		return MyUsage{}, err
	}
	org, me := caller.OrganizationID, caller.PrincipalID
	var out MyUsage
	if out.Enabled, err = s.GatewayEnabled(ctx, org); err != nil {
		return out, err
	}
	out.From, out.To = usageWindow(days)
	if out.Totals, err = scanTotals(tx.QueryRow(ctx, `SELECT `+totalsColumns+` FROM gateway_usage_daily
		WHERE organization_id=$1 AND principal_id=$2 AND day BETWEEN $3 AND $4`, org, me, out.From, out.To)); err != nil {
		return out, err
	}
	if out.ByProject, err = s.slices(ctx, tx, `SELECT d.project_id::text,COALESCE(p.name,'Deleted project'),'',`+totalsColumns+`
		FROM gateway_usage_daily d LEFT JOIN workflow_projects p ON p.organization_id=d.organization_id AND p.id=d.project_id
		WHERE d.organization_id=$1 AND d.principal_id=$2 AND d.day BETWEEN $3 AND $4
		GROUP BY 1,2 ORDER BY sum(d.cost_usd_micros) DESC, 1 LIMIT 50`, org, me, out.From, out.To); err != nil {
		return out, err
	}
	if out.ByModel, err = s.slices(ctx, tx, `SELECT d.model,d.model,min(d.route_kind),`+totalsColumns+`
		FROM gateway_usage_daily d WHERE d.organization_id=$1 AND d.principal_id=$2 AND d.day BETWEEN $3 AND $4
		GROUP BY 1,2 ORDER BY sum(d.cost_usd_micros) DESC, 1 LIMIT 50`, org, me, out.From, out.To); err != nil {
		return out, err
	}
	out.TopRuns, err = s.topRuns(ctx, tx, org, me, out.From)
	return out, err
}

type ModelCall struct {
	StartedAt                    time.Time
	Stage, Model, RouteKind, API string
	Status                       string
	HTTPStatus, RetryCount       int32
	Streamed, UsageReported      bool
	TTFTMS                       int32 // -1 when none
	DurationMS                   int32
	Totals                       UsageTotals
}

type StageCost struct {
	TaskID, Stage string
	Totals        UsageTotals
}

type RunCost struct {
	Enabled   bool
	Totals    UsageTotals
	Stages    []StageCost
	Calls     []ModelCall
	Truncated bool
}

// RunCostAs is the run page Cost tab: any member of the run's organization.
func (s *Store) RunCostAs(ctx context.Context, caller identity.Caller, runID string) (RunCost, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if !uuidPattern.MatchString(runID) {
		return RunCost{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RunCost{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockCallerSession(ctx, tx, caller, true); err != nil {
		return RunCost{}, err
	}
	org := caller.OrganizationID
	if _, err := s.GetRun(ctx, org, runID); err != nil {
		return RunCost{}, err
	}
	var out RunCost
	if out.Enabled, err = s.GatewayEnabled(ctx, org); err != nil {
		return out, err
	}
	if out.Totals, err = scanTotals(tx.QueryRow(ctx, `SELECT COALESCE(sum(requests),0),COALESCE(sum(errors),0),0,
		COALESCE(sum(input_tokens),0),COALESCE(sum(output_tokens),0),COALESCE(sum(cache_read_tokens),0),
		COALESCE(sum(cache_write_tokens),0),COALESCE(sum(reasoning_tokens),0),COALESCE(sum(cost_usd_micros),0)
		FROM gateway_run_usage WHERE organization_id=$1 AND run_id=$2`, org, runID)); err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `SELECT task_id::text,stage_key,count(*),count(*) FILTER (WHERE status<>'ok'),
		count(*) FILTER (WHERE http_status=429),sum(input_tokens),sum(output_tokens),sum(cache_read_tokens),
		sum(cache_write_tokens),sum(reasoning_tokens),sum(cost_usd_micros)
		FROM gateway_usage_events WHERE organization_id=$1 AND run_id=$2 GROUP BY 1,2 ORDER BY min(started_at)`, org, runID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var stage StageCost
		if stage.Totals, err = scanTotals(rows, &stage.TaskID, &stage.Stage); err != nil {
			rows.Close()
			return out, err
		}
		out.Stages = append(out.Stages, stage)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	rows, err = tx.Query(ctx, `SELECT started_at,stage_key,COALESCE(NULLIF(served_model,''),requested_model),route_kind,api,
		status,http_status,retry_count,streamed,usage_reported,COALESCE(ttft_ms,-1),duration_ms,
		input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,cost_usd_micros
		FROM gateway_usage_events WHERE organization_id=$1 AND run_id=$2 ORDER BY started_at DESC, id LIMIT 501`, org, runID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var c ModelCall
		if err := rows.Scan(&c.StartedAt, &c.Stage, &c.Model, &c.RouteKind, &c.API, &c.Status, &c.HTTPStatus, &c.RetryCount,
			&c.Streamed, &c.UsageReported, &c.TTFTMS, &c.DurationMS, &c.Totals.Input, &c.Totals.Output,
			&c.Totals.CacheRead, &c.Totals.CacheWrite, &c.Totals.Reasoning, &c.Totals.CostUSDMicro); err != nil {
			return out, err
		}
		c.Totals.Requests = 1
		out.Calls = append(out.Calls, c)
	}
	if len(out.Calls) > 500 {
		out.Calls, out.Truncated = out.Calls[:500], true
	}
	return out, rows.Err()
}

// ModelPrice is a bundled or overridden rate card (USD micros per MTok).
type ModelPrice struct {
	Provider, Model, Source, Version     string
	Input, Output, CacheRead, CacheWrite int64
	EffectiveFrom                        *time.Time
}

var priceModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var priceProvider = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ModelPricesAs lists bundled manifest prices with any effective override on top.
func (s *Store) ModelPricesAs(ctx context.Context, caller identity.Caller) ([]ModelPrice, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	tx, err := s.beginScoped(ctx, caller, ScopeOrganization, "")
	if err != nil {
		return nil, adminScopeError(err)
	}
	defer tx.Rollback(ctx)
	byKey := map[string]ModelPrice{}
	for _, provider := range []string{"anthropic", "openai"} {
		for _, m := range access.ManifestModels(provider) {
			price, version, ok := access.ManifestPrice(provider, m.Slug)
			if !ok {
				continue
			}
			micros := func(usd float64) int64 { return int64(usd*1e6 + 0.5) }
			byKey[provider+"/"+m.Slug] = ModelPrice{Provider: provider, Model: m.Slug, Source: "manifest", Version: version,
				Input: micros(price.Input), Output: micros(price.Output), CacheRead: micros(price.CacheRead), CacheWrite: micros(price.CacheWrite)}
		}
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT ON (provider,model) id::text,provider,model,input_micros_per_mtok,
		output_micros_per_mtok,cache_read_micros_per_mtok,cache_write_micros_per_mtok,effective_from
		FROM gateway_price_overrides WHERE organization_id=$1 AND effective_from<=clock_timestamp()
		ORDER BY provider,model,effective_from DESC`, caller.OrganizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p ModelPrice
		var id string
		var at time.Time
		if err := rows.Scan(&id, &p.Provider, &p.Model, &p.Input, &p.Output, &p.CacheRead, &p.CacheWrite, &at); err != nil {
			return nil, err
		}
		p.Source, p.Version, p.EffectiveFrom = "override", "override:"+id, &at
		byKey[p.Provider+"/"+p.Model] = p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]ModelPrice, 0, len(byKey))
	for _, p := range byKey {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out, nil
}

// SetModelPriceOverrideAs records contracted rates, effective now (audited).
func (s *Store) SetModelPriceOverrideAs(ctx context.Context, caller identity.Caller, p ModelPrice) (ModelPrice, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	const maxRate = 1_000_000_000 // $1000 per MTok.
	if !priceProvider.MatchString(p.Provider) || access.ModelOrigin(p.Provider) == "" || !priceModel.MatchString(p.Model) ||
		p.Input < 0 || p.Output < 0 || p.CacheRead < 0 || p.CacheWrite < 0 ||
		p.Input > maxRate || p.Output > maxRate || p.CacheRead > maxRate || p.CacheWrite > maxRate {
		return ModelPrice{}, ErrInvalid
	}
	tx, err := s.beginScoped(ctx, caller, ScopeOrganization, "")
	if err != nil {
		return ModelPrice{}, adminScopeError(err)
	}
	defer tx.Rollback(ctx)
	var id string
	var at time.Time
	if err := tx.QueryRow(ctx, `INSERT INTO gateway_price_overrides
		(organization_id,provider,model,input_micros_per_mtok,output_micros_per_mtok,cache_read_micros_per_mtok,
		 cache_write_micros_per_mtok,created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id::text,effective_from`, caller.OrganizationID, p.Provider, p.Model,
		p.Input, p.Output, p.CacheRead, p.CacheWrite, caller.PrincipalID).Scan(&id, &at); err != nil {
		return ModelPrice{}, err
	}
	detail, _ := json.Marshal(map[string]any{"provider": p.Provider, "model": p.Model, "input": p.Input, "output": p.Output,
		"cache_read": p.CacheRead, "cache_write": p.CacheWrite})
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events (organization_id,actor_kind,actor_id,action,subject_id,detail)
		VALUES ($1,'principal',$2,'gateway.price.overridden',$3,$4)`, caller.OrganizationID, caller.PrincipalID, id, detail); err != nil {
		return ModelPrice{}, err
	}
	p.Source, p.Version, p.EffectiveFrom = "override", "override:"+id, &at
	return p, tx.Commit(ctx)
}
