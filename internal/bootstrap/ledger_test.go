package bootstrap

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/db"
)

func TestLedgerPostgres(t *testing.T) {
	dsn := os.Getenv("BLAXSMITH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set BLAXSMITH_TEST_DATABASE_URL for the PostgreSQL concurrency test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := "blaxsmith_bootstrap_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	ledger := NewLedger(pool)
	actor := Actor{Atespace: "team-a", Name: "task-a", UID: "uid-a"}
	owner, err := ledger.Assign(ctx, "cluster-a", "attempt-a", 0, actor)
	if err != nil || owner.OwnerGeneration != 1 {
		t.Fatalf("initial owner: %+v, %v", owner, err)
	}
	currentScope, currentActor, active, exists, err := ledger.CurrentOwner(ctx, owner.ClusterID, owner.AttemptID)
	if err != nil || !exists || !active || currentScope != owner || currentActor != actor {
		t.Fatalf("current owner lookup: %+v %+v active=%t exists=%t err=%v", currentScope, currentActor, active, exists, err)
	}
	if _, err := ledger.Assign(ctx, owner.ClusterID, owner.AttemptID, 0, actor); !errors.Is(err, ErrDenied) {
		t.Fatalf("duplicate initial assignment: %v", err)
	}
	issue := func(scope Scope) Offer {
		t.Helper()
		offer, err := ledger.Issue(ctx, scope)
		if err != nil {
			t.Fatal(err)
		}
		return offer
	}
	denied := func(scope Scope, offer Offer, proof Proof, roots *x509.CertPool) {
		t.Helper()
		if _, err := ledger.Redeem(ctx, scope, offer.ID, offer.Nonce, proof, roots); !errors.Is(err, ErrDenied) {
			t.Fatalf("expected denial, got %v", err)
		}
	}

	first := issue(owner)
	proof, roots := signedProof(t, first)
	var storedHash []byte
	if err := pool.QueryRow(ctx, `SELECT nonce_sha256 FROM bootstrap_challenges WHERE id=$1`, first.ID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if hash := sha256.Sum256(first.Nonce[:]); string(storedHash) != string(hash[:]) || string(storedHash) == string(first.Nonce[:]) {
		t.Fatal("raw connector nonce persisted")
	}
	denied(Scope{ClusterID: "cluster-b", AttemptID: owner.AttemptID, OwnerGeneration: 1}, first, proof, roots)
	denied(Scope{ClusterID: owner.ClusterID, AttemptID: "attempt-b", OwnerGeneration: 1}, first, proof, roots)
	wrongNonce := first
	wrongNonce.Nonce[0] ^= 1
	denied(owner, wrongNonce, proof, roots)
	wrongRoots := x509.NewCertPool()
	denied(owner, first, proof, wrongRoots)
	second := issue(owner)
	denied(owner, first, proof, roots) // issuing again cancels the first challenge
	secondProof, secondRoots := signedProof(t, second)
	otherPool, err := pgxpool.NewWithConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer otherPool.Close()
	redeemed, err := NewLedger(otherPool).Redeem(ctx, owner, second.ID, second.Nonce, secondProof, secondRoots)
	if err != nil {
		t.Fatalf("durable redeem from another connection: %v", err)
	}
	runtime := Runtime{Actor: actor, TemplateUID: "template-a", Image: "image-a", WorkerPool: "pool-a"}
	if err := ledger.VerifyActivation(ctx, owner, runtime, redeemed.Challenge.Nonce); !errors.Is(err, ErrDenied) {
		t.Fatalf("unreleased activation accepted: %v", err)
	}
	sent := 0
	if _, err := ledger.Release(ctx, redeemed, nil, func(sendCtx context.Context, tx pgx.Tx) error {
		sent++
		return ledger.BindActivation(sendCtx, tx, redeemed, runtime)
	}); err != nil {
		t.Fatalf("release under owner fence: %v", err)
	}
	if err := NewLedger(otherPool).VerifyActivation(ctx, owner, runtime, redeemed.Challenge.Nonce); err != nil {
		t.Fatalf("released activation rejected: %v", err)
	}
	for _, changed := range []Runtime{{Actor: Actor{Atespace: actor.Atespace, Name: actor.Name, UID: "old-uid"}, TemplateUID: runtime.TemplateUID, Image: runtime.Image, WorkerPool: runtime.WorkerPool},
		{Actor: actor, TemplateUID: "old-template", Image: runtime.Image, WorkerPool: runtime.WorkerPool},
		{Actor: actor, TemplateUID: runtime.TemplateUID, Image: "old-image", WorkerPool: runtime.WorkerPool},
		{Actor: actor, TemplateUID: runtime.TemplateUID, Image: runtime.Image, WorkerPool: "old-pool"}} {
		if err := ledger.VerifyActivation(ctx, owner, changed, redeemed.Challenge.Nonce); !errors.Is(err, ErrDenied) {
			t.Fatalf("changed runtime accepted: %+v, %v", changed, err)
		}
	}
	if err := ledger.VerifyActivation(ctx, owner, runtime, base64.RawURLEncoding.EncodeToString(make([]byte, 32))); !errors.Is(err, ErrDenied) {
		t.Fatalf("wrong activation nonce accepted: %v", err)
	}
	if err := ledger.VerifyActivation(ctx, Scope{ClusterID: owner.ClusterID, AttemptID: owner.AttemptID, OwnerGeneration: 2}, runtime, redeemed.Challenge.Nonce); !errors.Is(err, ErrDenied) {
		t.Fatalf("stale generation accepted: %v", err)
	}
	if _, err := ledger.Release(ctx, redeemed, nil, func(context.Context, pgx.Tx) error { sent++; return nil }); !errors.Is(err, ErrDenied) || sent != 1 {
		t.Fatalf("replayed release: %v, sends=%d", err, sent)
	}
	denied(owner, second, secondProof, secondRoots) // replay

	if _, err := pool.Exec(ctx, `CREATE TABLE policy_fence (id integer PRIMARY KEY, active boolean NOT NULL);
		INSERT INTO policy_fence VALUES (1, true)`); err != nil {
		t.Fatal(err)
	}
	policyOffer := issue(owner)
	policyProof, policyRoots := signedProof(t, policyOffer)
	policyRedeemed, err := ledger.Redeem(ctx, owner, policyOffer.ID, policyOffer.Nonce, policyProof, policyRoots)
	if err != nil {
		t.Fatal(err)
	}
	checked := make(chan error, 1)
	finishSend := make(chan struct{})
	completedSend := make(chan error, 1)
	go func() {
		_, err := ledger.Release(ctx, policyRedeemed, nil, func(sendCtx context.Context, tx pgx.Tx) error {
			var active bool
			err := tx.QueryRow(sendCtx, `SELECT active FROM policy_fence WHERE id=1 FOR SHARE`).Scan(&active)
			if err == nil && !active {
				err = ErrDenied
			}
			checked <- err
			if err != nil {
				return err
			}
			<-finishSend
			return nil
		})
		completedSend <- err
	}()
	checkErr := <-checked
	if checkErr != nil {
		t.Fatal(checkErr)
	}
	revokeCtx, revokeCancel := context.WithTimeout(ctx, 150*time.Millisecond)
	_, revokeErr := otherPool.Exec(revokeCtx, `UPDATE policy_fence SET active=false WHERE id=1`)
	revokeCancel()
	close(finishSend)
	if !errors.Is(revokeErr, context.DeadlineExceeded) {
		t.Fatalf("revocation did not wait for release transaction: %v", revokeErr)
	}
	if err := <-completedSend; err != nil {
		t.Fatalf("release under policy row fence: %v", err)
	}
	if err := ledger.VerifyActivation(ctx, owner, runtime, redeemed.Challenge.Nonce); !errors.Is(err, ErrDenied) {
		t.Fatalf("older activation accepted after newer unbound release: %v", err)
	}
	if _, err := otherPool.Exec(ctx, `UPDATE policy_fence SET active=false WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	revokedOffer := issue(owner)
	revokedProof, revokedRoots := signedProof(t, revokedOffer)
	revokedRedeemed, err := ledger.Redeem(ctx, owner, revokedOffer.ID, revokedOffer.Nonce, revokedProof, revokedRoots)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Release(ctx, revokedRedeemed, nil, func(sendCtx context.Context, tx pgx.Tx) error {
		var active bool
		if err := tx.QueryRow(sendCtx, `SELECT active FROM policy_fence WHERE id=1 FOR SHARE`).Scan(&active); err != nil {
			return err
		}
		if !active {
			return ErrDenied
		}
		return nil
	}); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked policy released: %v", err)
	}

	unknown := issue(owner)
	unknownProof, unknownRoots := signedProof(t, unknown)
	unknownRedeemed, err := ledger.Redeem(ctx, owner, unknown.ID, unknown.Nonce, unknownProof, unknownRoots)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE release_intents (challenge_id text PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	transportErr := errors.New("unknown delivery outcome")
	if _, err := ledger.Release(ctx, unknownRedeemed, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO release_intents (challenge_id) VALUES ($1)`, unknown.ID)
		return err
	}, func(context.Context, pgx.Tx) error { return transportErr }); !errors.Is(err, transportErr) {
		t.Fatalf("release outcome: %v", err)
	}
	var intentCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM release_intents WHERE challenge_id=$1`, unknown.ID).
		Scan(&intentCount); err != nil || intentCount != 1 {
		t.Fatalf("unknown send lost durable intent: %d, %v", intentCount, err)
	}
	if _, err := ledger.Release(ctx, unknownRedeemed, nil, func(context.Context, pgx.Tx) error { sent++; return nil }); !errors.Is(err, ErrDenied) || sent != 1 {
		t.Fatalf("unknown release retried: %v, sends=%d", err, sent)
	}
	var attempted, completed bool
	if err := pool.QueryRow(ctx, `SELECT release_attempted_at IS NOT NULL, released_at IS NOT NULL
		FROM bootstrap_challenges WHERE id=$1`, unknown.ID).Scan(&attempted, &completed); err != nil || !attempted || completed {
		t.Fatalf("unknown release state: attempted=%t completed=%t err=%v", attempted, completed, err)
	}

	superseded := issue(owner)
	supersededProof, supersededRoots := signedProof(t, superseded)
	supersededRedeemed, err := ledger.Redeem(ctx, owner, superseded.ID, superseded.Nonce, supersededProof, supersededRoots)
	if err != nil {
		t.Fatal(err)
	}
	_ = issue(owner)
	if _, err := ledger.Release(ctx, supersededRedeemed, nil, func(context.Context, pgx.Tx) error { sent++; return nil }); !errors.Is(err, ErrDenied) || sent != 1 {
		t.Fatalf("superseded proof released: %v, sends=%d", err, sent)
	}

	concurrent := issue(owner)
	concurrentProof, concurrentRoots := signedProof(t, concurrent)
	results := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Go(func() {
			_, err := ledger.Redeem(ctx, owner, concurrent.ID, concurrent.Nonce, concurrentProof, concurrentRoots)
			results <- err
		})
	}
	group.Wait()
	close(results)
	var success, replay int
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrDenied):
			replay++
		default:
			t.Fatal(err)
		}
	}
	if success != 1 || replay != 1 {
		t.Fatalf("concurrent redemption: %d success, %d denied", success, replay)
	}

	expired := issue(owner)
	expiredProof, expiredRoots := signedProof(t, expired)
	if _, err := pool.Exec(ctx, `UPDATE bootstrap_challenges SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, expired.ID); err != nil {
		t.Fatal(err)
	}
	denied(owner, expired, expiredProof, expiredRoots)
	stale := issue(owner)
	staleProof, staleRoots := signedProof(t, stale)
	staleRedeemed, err := ledger.Redeem(ctx, owner, stale.ID, stale.Nonce, staleProof, staleRoots)
	if err != nil {
		t.Fatal(err)
	}
	newOwner, err := ledger.Assign(ctx, owner.ClusterID, owner.AttemptID, owner.OwnerGeneration,
		Actor{Atespace: actor.Atespace, Name: actor.Name, UID: "uid-b"})
	if err != nil || newOwner.OwnerGeneration != 2 {
		t.Fatalf("replacement owner: %+v, %v", newOwner, err)
	}
	if err := ledger.VerifyActivation(ctx, owner, runtime, redeemed.Challenge.Nonce); !errors.Is(err, ErrDenied) {
		t.Fatalf("replaced owner activation accepted: %v", err)
	}
	if _, err := ledger.Release(ctx, staleRedeemed, nil, func(context.Context, pgx.Tx) error { sent++; return nil }); !errors.Is(err, ErrDenied) || sent != 1 {
		t.Fatalf("stale owner released: %v, sends=%d", err, sent)
	}
	denied(owner, stale, staleProof, staleRoots)
	if _, err := ledger.Issue(ctx, owner); !errors.Is(err, ErrDenied) {
		t.Fatalf("stale owner issued challenge: %v", err)
	}
	if _, err := ledger.Deactivate(ctx, owner); !errors.Is(err, ErrDenied) {
		t.Fatalf("stale owner deactivated replacement: %v", err)
	}
	if _, err := ledger.Assign(ctx, owner.ClusterID, owner.AttemptID, owner.OwnerGeneration, actor); !errors.Is(err, ErrDenied) {
		t.Fatalf("stale owner assignment: %v", err)
	}
	current := issue(newOwner)
	if current.ActorUID != "uid-b" {
		t.Fatal("new challenge bound to stale actor UID")
	}
	currentProof, currentRoots := signedProof(t, current)
	if _, err := pool.Exec(ctx, `UPDATE bootstrap_owners SET actor_uid='uid-c'
		WHERE cluster_id=$1 AND attempt_id=$2`, owner.ClusterID, owner.AttemptID); err == nil {
		t.Fatal("actor identity changed without advancing bootstrap owner generation")
	}
	current = issue(newOwner)
	currentProof, currentRoots = signedProof(t, current)
	inactive, err := ledger.Deactivate(ctx, newOwner)
	if err != nil || inactive.OwnerGeneration != 3 {
		t.Fatalf("deactivate owner: %+v, %v", inactive, err)
	}
	currentScope, currentActor, active, exists, err = ledger.CurrentOwner(ctx, newOwner.ClusterID, newOwner.AttemptID)
	if err != nil || !exists || active || currentScope != inactive || currentActor.UID != "uid-b" {
		t.Fatalf("inactive owner lookup: %+v %+v active=%t exists=%t err=%v", currentScope, currentActor, active, exists, err)
	}
	denied(newOwner, current, currentProof, currentRoots)
	if _, err := ledger.Deactivate(ctx, newOwner); !errors.Is(err, ErrDenied) {
		t.Fatalf("repeated deactivate: %v", err)
	}
	if _, err := ledger.Issue(ctx, inactive); !errors.Is(err, ErrDenied) {
		t.Fatalf("inactive owner issued challenge: %v", err)
	}
	resumed, err := ledger.Assign(ctx, owner.ClusterID, owner.AttemptID, inactive.OwnerGeneration, actor)
	if err != nil || resumed.OwnerGeneration != 4 {
		t.Fatalf("resume with new generation: %+v, %v", resumed, err)
	}
	denied(resumed, current, currentProof, currentRoots)
	assignments := make(chan error, 2)
	for _, uid := range []string{"uid-rival-a", "uid-rival-b"} {
		group.Go(func() {
			_, err := ledger.Assign(ctx, owner.ClusterID, owner.AttemptID, resumed.OwnerGeneration,
				Actor{Atespace: actor.Atespace, Name: actor.Name, UID: uid})
			assignments <- err
		})
	}
	group.Wait()
	close(assignments)
	success, replay = 0, 0
	for err := range assignments {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrDenied):
			replay++
		default:
			t.Fatal(err)
		}
	}
	if success != 1 || replay != 1 {
		t.Fatalf("concurrent assignments: %d success, %d denied", success, replay)
	}
}

func signedProof(t *testing.T, offer Offer) (Proof, *x509.CertPool) {
	t.Helper()
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	actorKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := json.Marshal(map[string]string{"Atespace": offer.ActorAtespace,
		"ActorName": offer.ActorName, "ActorUid": offer.ActorUID, "Purpose": "atunnel"})
	actorTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		ExtraExtensions: []pkix.Extension{{Id: actorIdentityOID, Value: identity}}}
	actorDER, err := x509.CreateCertificate(rand.Reader, actorTemplate, ca, &actorKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	var guestNonce [32]byte
	if _, err := rand.Read(guestNonce[:]); err != nil {
		t.Fatal(err)
	}
	guest, _ := json.Marshal(Challenge{Nonce: base64.RawURLEncoding.EncodeToString(guestNonce[:]),
		ExpiresAt: now.Add(time.Minute).Unix(), Atespace: offer.ActorAtespace, Task: offer.ActorName})
	hash := sha256.New()
	hash.Write([]byte("blaxsmith/actor-proof/v1\x00"))
	hash.Write(offer.Nonce[:])
	hash.Write(guest)
	signature, err := ecdsa.SignASN1(rand.Reader, actorKey, hash.Sum(nil))
	if err != nil {
		t.Fatal(err)
	}
	chain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: actorDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...)
	return Proof{Body: guest, CertificatePEM: chain, Signature: signature}, roots
}
