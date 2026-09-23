package access

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestLeaseIntentAndRevocationPostgres(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	const repo = "https://git.example.invalid/team/private.git"
	const commit = "0123456789abcdef0123456789abcdef01234567"
	for _, statement := range []string{
		`INSERT INTO access_provider_registrations
		(organization_id,id,provider_kind,origin,delivery_modes,state)
		VALUES ('org-a','git','git','https://git.example.invalid',ARRAY['native_raw'],'active')`,
		`INSERT INTO access_connections
		(organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state,active_secret_version)
		VALUES ('org-a','connection-a','user','alice','git','account-a','test','active',1)`,
		`INSERT INTO access_project_policies
		(organization_id,project_id,version,git_read_enabled,delivery_modes)
		VALUES ('org-a','project-a',1,true,ARRAY['native_raw'])`,
		`INSERT INTO bootstrap_owners
		(cluster_id,attempt_id,owner_generation,actor_atespace,actor_name,actor_uid,active)
		VALUES ('cluster-a','attempt-a',1,'space-a','task-a','actor-a',true)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO access_grants
		(organization_id,id,connection_id,project_id,grantee_kind,grantee_id,capability,resource,delivery_mode,issuer_id)
		VALUES ('org-a','grant-a','connection-a','project-a','user','alice','git.read',$1,'native_raw','owner')`, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO access_bindings
		(organization_id,id,attempt_id,project_id,grant_id,grant_version,capability,resource,input_commit,policy_version)
		VALUES ('org-a','binding-a','attempt-a','project-a','grant-a',1,'git.read',$1,$2,1)`, repo, commit); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO access_secret_versions
		(organization_id,connection_id,version,key_id,algorithm,nonce,ciphertext)
		VALUES ('org-a','connection-a',1,'test-key','AES-256-GCM',$1,$2)`,
		bytes.Repeat([]byte{1}, 12), bytes.Repeat([]byte{2}, 17)); err != nil {
		t.Fatal(err)
	}
	challenge := func(id string, nonce byte) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO bootstrap_challenges
			(id,cluster_id,attempt_id,owner_generation,actor_atespace,actor_name,actor_uid,
			 nonce_sha256,expires_at,consumed_at,release_attempted_at)
			VALUES ($1,'cluster-a','attempt-a',1,'space-a','task-a','actor-a',$2,
			 clock_timestamp()+interval '1 minute',clock_timestamp(),clock_timestamp())`,
			id, bytes.Repeat([]byte{nonce}, 32)); err != nil {
			t.Fatal(err)
		}
	}
	challenge("proof-1", 1)
	request := LeaseRequest{OrganizationID: "org-a", BindingID: "binding-a", ChallengeID: "proof-1",
		ClusterID: "cluster-a", AttemptID: "attempt-a", OwnerGeneration: 1,
		ActorUID: "actor-a", RepoURL: repo, GuestExpiresAt: time.Now().Add(time.Minute)}
	forged := request
	forged.ActorUID = "forged-actor"
	wrong, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, forgedErr := ReserveGitLease(ctx, wrong, forged)
	_ = wrong.Rollback(ctx)
	if !errors.Is(forgedErr, ErrDenied) {
		t.Fatalf("forged actor reserved lease: %v", forgedErr)
	}
	intent, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	leaseID, err := ReserveGitLease(ctx, intent, request)
	if err != nil || leaseID == "" {
		t.Fatalf("reserve access lease: %q, %v", leaseID, err)
	}
	if err := intent.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	duplicate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, duplicateErr := ReserveGitLease(ctx, duplicate, request)
	_ = duplicate.Rollback(ctx)
	if duplicateErr == nil {
		t.Fatal("challenge accepted another lease")
	}
	send, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkLeaseAttempt(ctx, send, "org-a", leaseID, "wrong-connection", 1); !errors.Is(err, ErrDenied) {
		t.Fatalf("wrong connection marked lease: %v", err)
	}
	if err := MarkLeaseAttempt(ctx, send, "org-a", leaseID, "connection-a", 1); err != nil {
		t.Fatal(err)
	}
	if err := MarkLeaseDelivered(ctx, send, "org-a", leaseID); err != nil {
		t.Fatal(err)
	}
	if err := send.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var attempted, delivered bool
	var version int64
	if err := pool.QueryRow(ctx, `SELECT delivery_attempted_at IS NOT NULL,
		delivered_at IS NOT NULL,secret_version FROM access_leases
		WHERE organization_id='org-a' AND id=$1`, leaseID).Scan(&attempted, &delivered, &version); err != nil ||
		!attempted || !delivered || version != 1 {
		t.Fatalf("completed lease: attempted=%t delivered=%t version=%d err=%v", attempted, delivered, version, err)
	}
	challenge("proof-2", 2)
	request.ChallengeID = "proof-2"
	unknown, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	unknownID, err := ReserveGitLease(ctx, unknown, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := unknown.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	uncertainSend, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkLeaseAttempt(ctx, uncertainSend, "org-a", unknownID, "connection-a", 1); err != nil {
		t.Fatal(err)
	}
	if err := uncertainSend.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT delivery_attempted_at IS NOT NULL,delivered_at IS NOT NULL
		FROM access_leases WHERE organization_id='org-a' AND id=$1`, unknownID).
		Scan(&attempted, &delivered); err != nil || attempted || delivered {
		t.Fatalf("uncertain delivery lost durable reservation: %t %t %v", attempted, delivered, err)
	}
	if err := RevokeGrant(ctx, pool, "org-a", "grant-a"); err != nil {
		t.Fatal(err)
	}
	var revoked int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM access_leases
		WHERE organization_id='org-a' AND revoked_at IS NOT NULL`).Scan(&revoked); err != nil || revoked != 2 {
		t.Fatalf("grant revocation did not mark recorded leases: %d, %v", revoked, err)
	}
	check, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = AuthorizeGitRead(ctx, check, GitRead{OrganizationID: "org-a", ProjectID: "project-a",
		AttemptID: "attempt-a", BindingID: "binding-a", GranteeKind: "user", GranteeID: "alice",
		RepoURL: repo, Commit: commit, PolicyVersion: 1})
	_ = check.Rollback(ctx)
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked grant authorized new delivery: %v", err)
	}
}

func TestConnectionRevocationFencesDeliveryPostgres(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	_, err := pool.Exec(ctx, `
		INSERT INTO access_provider_registrations (organization_id,id,provider_kind,origin,delivery_modes,state)
		VALUES ('org-a','provider','git','https://git.example.invalid',ARRAY['native_raw'],'active');
		INSERT INTO access_connections (organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state)
		VALUES ('org-a','connection','organization','org-a','provider','account','test','active');
		INSERT INTO access_project_policies (organization_id,project_id,version,git_read_enabled,delivery_modes)
		VALUES ('org-a','project',1,true,ARRAY['native_raw']);
		INSERT INTO access_grants (organization_id,id,connection_id,project_id,grantee_kind,grantee_id,capability,resource,delivery_mode,issuer_id)
		VALUES ('org-a','grant','connection','project','user','alice','git.read','https://git.example.invalid/team/repo.git','native_raw','owner');
		INSERT INTO access_bindings (organization_id,id,attempt_id,project_id,grant_id,grant_version,capability,resource,input_commit,policy_version)
		VALUES ('org-a','binding','attempt','project','grant',1,'git.read','https://git.example.invalid/team/repo.git','0123456789abcdef0123456789abcdef01234567',1);
		INSERT INTO bootstrap_owners (cluster_id,attempt_id,owner_generation,actor_atespace,actor_name,actor_uid,active)
		VALUES ('cluster','attempt',1,'space','task','actor',true);
		INSERT INTO bootstrap_challenges (id,cluster_id,attempt_id,owner_generation,actor_atespace,actor_name,actor_uid,nonce_sha256,expires_at,consumed_at,release_attempted_at)
		VALUES ('challenge','cluster','attempt',1,'space','task','actor',decode(repeat('01',32),'hex'),clock_timestamp()+interval '1 minute',clock_timestamp(),clock_timestamp());
		INSERT INTO access_leases (organization_id,id,binding_id,connection_id,bootstrap_challenge_id,cluster_id,attempt_id,owner_generation,actor_uid,capability,resource,audience,expires_at)
		VALUES ('org-a','lease','binding','connection','challenge','cluster','attempt',1,'actor','git.read','https://git.example.invalid/team/repo.git','https://git.example.invalid',clock_timestamp()+interval '1 minute');`)
	if err != nil {
		t.Fatal(err)
	}
	read, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback(ctx)
	if _, err := AuthorizeGitRead(ctx, read, GitRead{OrganizationID: "org-a", ProjectID: "project", AttemptID: "attempt",
		BindingID: "binding", GranteeKind: "user", GranteeID: "alice",
		RepoURL: "https://git.example.invalid/team/repo.git", Commit: "0123456789abcdef0123456789abcdef01234567", PolicyVersion: 1}); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		close(started)
		finished <- RevokeConnection(ctx, pool, "org-a", "connection")
	}()
	<-started
	select {
	case err := <-finished:
		t.Fatalf("connection revocation bypassed release lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := read.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	var state string
	var revoked bool
	if err := pool.QueryRow(ctx, `SELECT c.state,l.revoked_at IS NOT NULL FROM access_connections c
		JOIN access_leases l ON l.organization_id=c.organization_id AND l.connection_id=c.id
		WHERE c.organization_id='org-a' AND c.id='connection'`).Scan(&state, &revoked); err != nil || state != "revoked" || !revoked {
		t.Fatalf("connection and lease not revoked: %q %t %v", state, revoked, err)
	}
	if err := RevokeConnection(ctx, pool, "org-a", "connection"); !errors.Is(err, ErrDenied) {
		t.Fatalf("repeated connection revocation: %v", err)
	}
}
