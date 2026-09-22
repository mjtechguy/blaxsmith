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
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
	schemaSQL, err := os.ReadFile(filepath.Join("..", "..", "db", "migrations", "0001_bootstrap.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(schemaSQL)); err != nil {
		t.Fatal(err)
	}
	ledger := NewLedger(pool)
	actor := Actor{Atespace: "team-a", Name: "task-a", UID: "uid-a"}
	owner, err := ledger.Assign(ctx, "cluster-a", "attempt-a", 0, actor)
	if err != nil || owner.OwnerGeneration != 1 {
		t.Fatalf("initial owner: %+v, %v", owner, err)
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
	if _, err := NewLedger(otherPool).Redeem(ctx, owner, second.ID, second.Nonce, secondProof, secondRoots); err != nil {
		t.Fatalf("durable redeem from another connection: %v", err)
	}
	denied(owner, second, secondProof, secondRoots) // replay

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
	newOwner, err := ledger.Assign(ctx, owner.ClusterID, owner.AttemptID, owner.OwnerGeneration,
		Actor{Atespace: actor.Atespace, Name: actor.Name, UID: "uid-b"})
	if err != nil || newOwner.OwnerGeneration != 2 {
		t.Fatalf("replacement owner: %+v, %v", newOwner, err)
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
		WHERE cluster_id=$1 AND attempt_id=$2`, owner.ClusterID, owner.AttemptID); err != nil {
		t.Fatal(err)
	}
	denied(newOwner, current, currentProof, currentRoots)
	current = issue(newOwner)
	currentProof, currentRoots = signedProof(t, current)
	inactive, err := ledger.Deactivate(ctx, newOwner)
	if err != nil || inactive.OwnerGeneration != 3 {
		t.Fatalf("deactivate owner: %+v, %v", inactive, err)
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
