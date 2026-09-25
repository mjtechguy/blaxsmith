package access

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

func fakeJWT(claims map[string]any) string {
	body, _ := json.Marshal(claims)
	return "e30." + base64.RawURLEncoding.EncodeToString(body) + ".sig"
}

func codexAuthJSON(t *testing.T, account, access, refresh string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"OPENAI_API_KEY": nil, "last_refresh": "2026-09-01T00:00:00Z",
		"tokens": map[string]string{"access_token": access, "refresh_token": refresh,
			"id_token": fakeJWT(map[string]any{"https://api.openai.com/auth": map[string]string{
				"chatgpt_account_id": account, "chatgpt_plan_type": "pro"}})}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestParseCodexAuthRejectsUnsafeMaterial(t *testing.T) {
	access := fakeJWT(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	if _, account, err := ParseCodexAuth(codexAuthJSON(t, "acct", access, "rt")); err != nil || account.AccountID != "acct" || account.PlanType != "pro" {
		t.Fatalf("valid auth.json: %+v %v", account, err)
	}
	for name, raw := range map[string][]byte{
		"api key":       []byte(`{"OPENAI_API_KEY":"sk-test","tokens":null}`),
		"no refresh":    codexAuthJSON(t, "acct", access, ""),
		"opaque access": codexAuthJSON(t, "acct", "not-a-jwt", "rt"),
		"no account":    codexAuthJSON(t, "", access, "rt"),
		"garbage":       []byte("{"),
	} {
		if _, _, err := ParseCodexAuth(raw); !errors.Is(err, ErrDenied) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func TestCodexSubscriptionRefreshCustodyPostgres(t *testing.T) {
	ctx := tenant.System(context.Background())
	pool := testPool(t)
	store, err := NewSecretStore(pool, "key", map[string][]byte{"key": []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		time.Sleep(200 * time.Millisecond) // Widen the race window.
		switch body["refresh_token"] {
		case "rt-alice-1":
			_ = json.NewEncoder(w).Encode(map[string]string{"refresh_token": "rt-alice-2",
				"access_token": fakeJWT(map[string]any{"exp": time.Now().Add(2 * time.Hour).Unix(), "n": 2})})
		case "rt-bob-1":
			w.WriteHeader(http.StatusBadGateway) // Outcome unknown: the token may be spent.
		default:
			w.WriteHeader(http.StatusBadRequest) // Reuse of a rotated token.
		}
	}))
	defer endpoint.Close()
	refresher := &OAuthRefresher{DB: pool, Secrets: store, TokenURL: endpoint.URL}

	for _, statement := range []string{
		`INSERT INTO access_provider_registrations (organization_id,id,provider_kind,origin,delivery_modes,state)
			VALUES ('org-a','openai','openai','https://api.openai.com',ARRAY['native_raw','oauth_access'],'active')`,
		`INSERT INTO access_project_policies (organization_id,project_id,version,git_read_enabled,delivery_modes)
			VALUES ('org-a','project',1,false,ARRAY['native_raw','oauth_access'])`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	connect := func(owner, account, refresh string) string {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		soon := fakeJWT(map[string]any{"exp": time.Now().Add(2 * time.Minute).Unix(), "n": 1})
		id, _, err := CreateCodexConnection(ctx, tx, store, "org-a", owner, "openai", codexAuthJSON(t, account, soon, refresh))
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return id
	}
	alice := connect("alice", "acct-alice", "rt-alice-1")
	for _, g := range [][3]string{{"g-alice", "user", "alice"}, {"g-bob", "user", "bob"}, {"g-work", "workload", "blaxsmith-dispatcher"}} {
		if _, err := pool.Exec(ctx, `INSERT INTO access_grants
			(organization_id,id,connection_id,project_id,grantee_kind,grantee_id,capability,resource,delivery_mode,issuer_id)
			VALUES ('org-a',$1,$2,'project',$3,$4,'model.invoke','openai/gpt-6','oauth_access','alice')`,
			g[0], alice, g[1], g[2]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO access_grants
		(organization_id,id,connection_id,project_id,grantee_kind,grantee_id,capability,resource,delivery_mode,issuer_id)
		VALUES ('org-a','g-raw',$1,'project','user','alice','model.invoke','openai/gpt-6','native_raw','alice')`, alice); err != nil {
		t.Fatal(err)
	}

	// Personal-owner-only: only Alice's own attempts may use her login, and
	// the refresh bundle can never leave through the raw API-key path.
	preflight := func(grant, kind, grantee string) (ModelApproval, error) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		return PreflightModelInvoke(ctx, tx, ModelGrant{OrganizationID: "org-a", ProjectID: "project",
			GrantID: grant, GranteeKind: kind, GranteeID: grantee, Provider: "openai", Model: "gpt-6"})
	}
	approval, err := preflight("g-alice", "user", "alice")
	if err != nil || approval.Decision.DeliveryMode != "oauth_access" {
		t.Fatalf("owner preflight: %+v %v", approval, err)
	}
	for _, denied := range [][3]string{{"g-bob", "user", "bob"}, {"g-work", "workload", "blaxsmith-dispatcher"},
		{"g-raw", "user", "alice"}, {"g-alice", "user", "bob"}} {
		if _, err := preflight(denied[0], denied[1], denied[2]); !errors.Is(err, ErrDenied) {
			t.Fatalf("%v authorized: %v", denied, err)
		}
	}

	// Concurrent refresh: one provider call, one rotation, same token for both.
	until := time.Now().Add(30 * time.Minute)
	var wg sync.WaitGroup
	results := make([]Delivery, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = refresher.Deliver(ctx, "org-a", alice, until)
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || calls.Load() != 1 ||
		string(results[0].AccessToken) != string(results[1].AccessToken) ||
		results[0].SecretVersion != 2 || results[1].SecretVersion != 2 {
		t.Fatalf("concurrent refresh: calls=%d %v %v v=%d/%d", calls.Load(), errs[0], errs[1],
			results[0].SecretVersion, results[1].SecretVersion)
	}
	if !results[0].ExpiresAt.After(until) {
		t.Fatalf("delivered token expires before lease: %v", results[0].ExpiresAt)
	}

	// The rotated refresh token was committed before Deliver returned.
	check, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var version int64
	if err := check.QueryRow(ctx, `SELECT secret_version FROM access_oauth_sessions
		WHERE organization_id='org-a' AND connection_id=$1`, alice).Scan(&version); err != nil || version != 2 {
		t.Fatalf("persisted version: %d %v", version, err)
	}
	stored, err := store.readVersion(ctx, check, "org-a", alice, 2)
	_ = check.Rollback(ctx)
	if err != nil || !strings.Contains(string(stored.Bytes), `"refresh_token":"rt-alice-2"`) {
		t.Fatalf("rotated refresh token not persisted: %v", err)
	}

	// The pod payload carries no refresh token, only access + id tokens.
	var file struct {
		Tokens codexTokens `json:"tokens"`
	}
	for _, d := range results {
		if strings.Contains(string(d.File), "rt-alice") || d.FileName != ".codex/auth.json" ||
			json.Unmarshal(d.File, &file) != nil || file.Tokens.RefreshToken != "" ||
			file.Tokens.AccessToken != string(d.AccessToken) || file.Tokens.AccountID != "acct-alice" {
			t.Fatalf("unsafe delivery file: %s", d.File)
		}
	}

	// Renewal seam: re-authorizes the delivered lease, no refresh needed yet.
	bind, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	grant := ModelGrant{OrganizationID: "org-a", ProjectID: "project", GrantID: "g-alice",
		GranteeKind: "user", GranteeID: "alice", Provider: "openai", Model: "gpt-6"}
	approval, err = PreflightModelInvoke(ctx, bind, grant)
	if err != nil {
		t.Fatal(err)
	}
	bindingID, err := BindModelInvoke(ctx, bind, grant, approval, "attempt")
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO bootstrap_owners (cluster_id,attempt_id,owner_generation,actor_atespace,actor_name,actor_uid,active)
			VALUES ('cluster','attempt',1,'space','task','actor',true)`,
		`INSERT INTO bootstrap_challenges
			(id,cluster_id,attempt_id,owner_generation,actor_atespace,actor_name,actor_uid,nonce_sha256,expires_at,consumed_at,release_attempted_at,phase)
			VALUES ('challenge','cluster','attempt',1,'space','task','actor',decode(repeat('01',32),'hex'),clock_timestamp()+interval '1 minute',clock_timestamp(),clock_timestamp(),'model')`,
	} {
		if _, err := bind.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	leaseID, err := ReserveModelLease(ctx, bind, ModelLeaseRequest{OrganizationID: "org-a", BindingID: bindingID,
		ChallengeID: "challenge", ClusterID: "cluster", AttemptID: "attempt", OwnerGeneration: 1,
		ActorUID: "actor", Provider: "openai", Model: "gpt-6", ExpiresAt: time.Now().Add(15 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkLeaseAttempt(ctx, bind, "org-a", leaseID, alice, 2); err != nil {
		t.Fatal(err)
	}
	if err := MarkLeaseDelivered(ctx, bind, "org-a", leaseID); err != nil {
		t.Fatal(err)
	}
	if err := bind.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	lease := OAuthLease{LeaseID: leaseID, Until: until, Invoke: ModelInvoke{OrganizationID: "org-a",
		ProjectID: "project", AttemptID: "attempt", BindingID: bindingID, GranteeKind: "user",
		GranteeID: "alice", Provider: "openai", Model: "gpt-6", PolicyVersion: 1}}
	renew := func() (Delivery, error) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		return refresher.RenewOAuthDelivery(ctx, tx, lease)
	}
	renewed, err := renew()
	if err != nil || string(renewed.AccessToken) != string(results[0].AccessToken) || calls.Load() != 1 {
		t.Fatalf("renewal: %v calls=%d", err, calls.Load())
	}
	// The renewal hook's database half rebuilds the binding from the lease
	// alone and keeps the lease inside the token's expiry minus 5 minutes.
	hookDelivery, announced, err := refresher.RenewLease(ctx, RenewedLease{OrganizationID: "org-a", AttemptID: "attempt",
		LeaseID: leaseID, DeliveryMode: "oauth_access", ExpiresAt: until})
	if err != nil || string(hookDelivery.AccessToken) != string(results[0].AccessToken) ||
		announced.After(hookDelivery.ExpiresAt.Add(-CodexLeaseMargin)) || calls.Load() != 1 {
		t.Fatalf("renewal hook: announced=%v token=%v %v calls=%d", announced, hookDelivery.ExpiresAt, err, calls.Load())
	}
	var leaseExpiry time.Time
	if err := pool.QueryRow(ctx, `SELECT expires_at FROM access_leases WHERE organization_id='org-a' AND id=$1`,
		leaseID).Scan(&leaseExpiry); err != nil || !leaseExpiry.Equal(announced) {
		t.Fatalf("lease expiry %v, announced %v: %v", leaseExpiry, announced, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE access_leases SET expires_at=$2 WHERE organization_id='org-a' AND id=$1`,
		leaseID, results[0].ExpiresAt.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, capped, err := refresher.RenewLease(ctx, RenewedLease{OrganizationID: "org-a", AttemptID: "attempt",
		LeaseID: leaseID, DeliveryMode: "oauth_access", ExpiresAt: until}); err != nil ||
		!capped.Equal(results[0].ExpiresAt.Add(-CodexLeaseMargin).Truncate(time.Microsecond)) {
		t.Fatalf("lease past the token's expiry was not capped: %v %v", capped, err)
	}
	for _, bad := range []RenewedLease{
		{OrganizationID: "org-a", AttemptID: "attempt", LeaseID: leaseID, DeliveryMode: "native_raw", ExpiresAt: until},
		{OrganizationID: "org-a", AttemptID: "other", LeaseID: leaseID, DeliveryMode: "oauth_access", ExpiresAt: until},
	} {
		if _, _, err := refresher.RenewLease(ctx, bad); !errors.Is(err, ErrDenied) {
			t.Fatalf("renewal hook accepted %+v: %v", bad, err)
		}
	}

	// A revoked connection fails closed without touching the provider.
	if err := RevokeConnection(ctx, pool, "org-a", alice); err != nil {
		t.Fatal(err)
	}
	if _, err := renew(); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked renewal: %v", err)
	}
	if _, err := refresher.Deliver(ctx, "org-a", alice, until); !errors.Is(err, ErrDenied) || calls.Load() != 1 {
		t.Fatalf("revoked delivery: %v calls=%d", err, calls.Load())
	}

	// An uncertain refresh blocks the login instead of retrying a maybe-spent token.
	bob := connect("bob", "acct-bob", "rt-bob-1")
	for range 2 {
		if _, err := refresher.Deliver(ctx, "org-a", bob, until); !errors.Is(err, ErrReconnect) {
			t.Fatalf("uncertain refresh: %v", err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("uncertain refresh retried: calls=%d", calls.Load())
	}
}

// A crash after the provider rotated the refresh token but before the session
// adopted it must not strand the login: the next renewal resumes from the
// persisted rotation instead of spending the old token again.
func TestCodexRefreshSurvivesCrashAfterRotationPostgres(t *testing.T) {
	ctx := tenant.System(context.Background())
	pool := testPool(t)
	store, err := NewSecretStore(pool, "key", map[string][]byte{"key": []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch body["refresh_token"] {
		case "rt-1":
			_ = json.NewEncoder(w).Encode(map[string]string{"refresh_token": "rt-2",
				"access_token": fakeJWT(map[string]any{"exp": time.Now().Add(20 * time.Minute).Unix(), "n": 2})})
		case "rt-2":
			_ = json.NewEncoder(w).Encode(map[string]string{"refresh_token": "rt-3",
				"access_token": fakeJWT(map[string]any{"exp": time.Now().Add(2 * time.Hour).Unix(), "n": 3})})
		default:
			w.WriteHeader(http.StatusBadRequest) // A spent token.
		}
	}))
	defer endpoint.Close()
	if _, err := pool.Exec(ctx, `INSERT INTO access_provider_registrations (organization_id,id,provider_kind,origin,delivery_modes,state)
		VALUES ('org-a','openai','openai','https://api.openai.com',ARRAY['native_raw','oauth_access'],'active')`); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	soon := fakeJWT(map[string]any{"exp": time.Now().Add(2 * time.Minute).Unix(), "n": 1})
	id, _, err := CreateCodexConnection(ctx, tx, store, "org-a", "alice", "openai", codexAuthJSON(t, "acct", soon, "rt-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	crash := errors.New("crashed before the session commit")
	refresher := &OAuthRefresher{DB: pool, Secrets: store, TokenURL: endpoint.URL, afterPersist: func() error { return crash }}
	if _, err := refresher.Deliver(ctx, "org-a", id, time.Now().Add(30*time.Minute)); !errors.Is(err, crash) {
		t.Fatalf("injected crash: %v", err)
	}
	refresher.afterPersist = nil
	// rt-2's access token (20 minutes) is too short for this lease, so the
	// renewal must refresh with rt-2, not the spent rt-1.
	delivery, err := refresher.Deliver(ctx, "org-a", id, time.Now().Add(30*time.Minute))
	if err != nil || delivery.SecretVersion != 3 || calls.Load() != 2 {
		t.Fatalf("renewal after crash: v=%d calls=%d %v", delivery.SecretVersion, calls.Load(), err)
	}
	var reason *string
	var version int64
	if err := pool.QueryRow(ctx, `SELECT secret_version,reconnect_reason FROM access_oauth_sessions
		WHERE organization_id='org-a' AND connection_id=$1`, id).Scan(&version, &reason); err != nil || version != 3 || reason != nil {
		t.Fatalf("session after recovery: v=%d reason=%v %v", version, reason, err)
	}
}
