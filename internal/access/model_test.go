package access

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestModelAuthorityAndAttemptLeasePostgres(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	for _, statement := range []string{
		`INSERT INTO access_provider_registrations
			(organization_id,id,provider_kind,origin,delivery_modes,state)
			VALUES ('org-a','openai','openai','https://api.openai.com',ARRAY['native_raw'],'active')`,
		`INSERT INTO access_connections
			(organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state)
			VALUES ('org-a','connection','user','alice','openai','account','api_key','active')`,
		`INSERT INTO access_project_policies
			(organization_id,project_id,version,git_read_enabled,delivery_modes)
			VALUES ('org-a','project',1,false,ARRAY['native_raw'])`,
		`INSERT INTO access_grants
			(organization_id,id,connection_id,project_id,grantee_kind,grantee_id,capability,resource,delivery_mode,issuer_id,expires_at)
			VALUES ('org-a','grant','connection','project','user','alice','model.invoke','openai/gpt-6-luna','native_raw','owner',clock_timestamp()+interval '1 hour')`,
		`INSERT INTO bootstrap_owners
			(cluster_id,attempt_id,owner_generation,actor_atespace,actor_name,actor_uid,active)
			VALUES ('cluster','attempt',1,'space','task','actor',true)`,
		`INSERT INTO bootstrap_challenges
			(id,cluster_id,attempt_id,owner_generation,actor_atespace,actor_name,actor_uid,nonce_sha256,expires_at,consumed_at,release_attempted_at,phase)
			VALUES ('challenge','cluster','attempt',1,'space','task','actor',decode(repeat('01',32),'hex'),clock_timestamp()+interval '1 minute',clock_timestamp(),clock_timestamp(),'model')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewSecretStore(pool, "key", map[string][]byte{"key": []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	grant := ModelGrant{OrganizationID: "org-a", ProjectID: "project", GrantID: "grant",
		GranteeKind: "user", GranteeID: "alice", Provider: "openai", Model: "gpt-6-luna"}
	missing, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = PreflightModelInvoke(ctx, missing, grant)
	_ = missing.Rollback(ctx)
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("missing encrypted secret passed preflight: %v", err)
	}
	if _, err := store.Rotate(ctx, "org-a", "connection", 0, []byte("private-test-key"), nil); err != nil {
		t.Fatal(err)
	}
	preflight, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := PreflightModelInvoke(ctx, preflight, grant)
	_ = preflight.Rollback(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bind, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bindingID, err := BindModelInvoke(ctx, bind, grant, approval, "attempt")
	if err != nil {
		t.Fatal(err)
	}
	if err := bind.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	request := ModelInvoke{OrganizationID: "org-a", ProjectID: "project", AttemptID: "attempt",
		BindingID: bindingID, GranteeKind: "user", GranteeID: "alice", Provider: "openai",
		Model: "gpt-6-luna", PolicyVersion: 1}
	check, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := AuthorizeModelInvoke(ctx, check, request)
	_ = check.Rollback(ctx)
	if err != nil || decision.ConnectionID != "connection" || decision.DeliveryMode != "native_raw" {
		t.Fatalf("model decision: %+v %v", decision, err)
	}
	expiry := time.Now().Add(15 * time.Minute)
	intent, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	leaseID, err := ReserveModelLease(ctx, intent, ModelLeaseRequest{OrganizationID: "org-a", BindingID: bindingID,
		ChallengeID: "challenge", ClusterID: "cluster", AttemptID: "attempt", OwnerGeneration: 1,
		ActorUID: "actor", Provider: "openai", Model: "gpt-6-luna", ExpiresAt: expiry})
	if err != nil {
		t.Fatal(err)
	}
	if err := intent.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	send, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AuthorizeModelInvoke(ctx, send, request); err != nil {
		t.Fatal(err)
	}
	secret, err := store.ReadCurrent(ctx, send, "org-a", decision.ConnectionID)
	if err != nil || string(secret.Bytes) != "private-test-key" {
		t.Fatalf("secret read: %v", err)
	}
	if err := MarkLeaseAttempt(ctx, send, "org-a", leaseID, decision.ConnectionID, secret.Version); err != nil {
		t.Fatal(err)
	}
	secret.Clear()
	if err := MarkLeaseDelivered(ctx, send, "org-a", leaseID); err != nil {
		t.Fatal(err)
	}
	if err := send.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := RevokeGrant(ctx, pool, "org-a", "grant"); err != nil {
		t.Fatal(err)
	}
	denied, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = AuthorizeModelInvoke(ctx, denied, request)
	_ = denied.Rollback(ctx)
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked grant authorized: %v", err)
	}
	var revoked bool
	if err := pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM access_leases WHERE organization_id='org-a' AND id=$1`, leaseID).Scan(&revoked); err != nil || !revoked {
		t.Fatalf("lease revocation: %t %v", revoked, err)
	}
}
