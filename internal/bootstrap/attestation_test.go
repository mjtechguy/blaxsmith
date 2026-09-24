package bootstrap

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

func TestVerifyActorProof(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	actorKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	identity, _ := json.Marshal(map[string]string{"Atespace": "team", "ActorName": "task", "ActorUid": "uid-1", "Purpose": "atunnel"})
	actorTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		ExtraExtensions: []pkix.Extension{{Id: actorIdentityOID, Value: identity}}}
	actorDER, err := x509.CreateCertificate(rand.Reader, actorTemplate, ca, &actorKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	chain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: actorDER}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...)
	var requestNonce, guestNonce [32]byte
	if _, err := rand.Read(requestNonce[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(guestNonce[:]); err != nil {
		t.Fatal(err)
	}
	recipient, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	challenge := Challenge{Nonce: base64.RawURLEncoding.EncodeToString(guestNonce[:]), ExpiresAt: now.Add(time.Minute).Unix(), Phase: PhaseSetup, Atespace: "team", Task: "task", RecipientKey: base64.RawURLEncoding.EncodeToString(recipient.PublicKey().Bytes())}
	body, _ := json.Marshal(challenge)
	sign := func(body []byte) []byte {
		h := sha256.New()
		h.Write([]byte("blaxsmith/actor-proof/v1\x00"))
		h.Write(requestNonce[:])
		h.Write(body)
		signature, err := ecdsa.SignASN1(rand.Reader, actorKey, h.Sum(nil))
		if err != nil {
			t.Fatal(err)
		}
		return signature
	}
	expected := Expected{Roots: roots, Atespace: "team", ActorName: "task", ActorUID: "uid-1", Nonce: requestNonce, Now: now}
	if got, err := Verify(expected, body, chain, sign(body)); err != nil || got != challenge {
		t.Fatalf("valid actor proof = %+v, %v", got, err)
	}
	wrongPhase := challenge
	wrongPhase.Phase = "publish"
	wrongBody, _ := json.Marshal(wrongPhase)
	if _, err := Verify(expected, wrongBody, chain, sign(wrongBody)); err == nil {
		t.Fatal("unsupported credential phase accepted")
	}
	wrongActor := expected
	wrongActor.ActorUID = "uid-2"
	wrongNonce := expected
	wrongNonce.Nonce[0] ^= 0xff
	wrongCA := expected
	wrongCA.Roots = x509.NewCertPool()
	expired := challenge
	expired.ExpiresAt = now.Add(-time.Second).Unix()
	expiredBody, _ := json.Marshal(expired)
	wrongTask := challenge
	wrongTask.Task = "other"
	wrongTaskBody, _ := json.Marshal(wrongTask)
	invalidKey := challenge
	invalidKey.RecipientKey = "not-a-key"
	invalidKeyBody, _ := json.Marshal(invalidKey)
	for _, tc := range []struct {
		name     string
		expected Expected
		body     []byte
		chain    []byte
		sig      []byte
	}{
		{"wrong actor UID", wrongActor, body, chain, sign(body)},
		{"wrong connector nonce", wrongNonce, body, chain, sign(body)},
		{"wrong cluster CA", wrongCA, body, chain, sign(body)},
		{"tampered guest response", expected, append([]byte{}, expiredBody...), chain, sign(body)},
		{"expired guest challenge", expected, expiredBody, chain, sign(expiredBody)},
		{"wrong task", expected, wrongTaskBody, chain, sign(wrongTaskBody)},
		{"invalid guest encryption key", expected, invalidKeyBody, chain, sign(invalidKeyBody)},
		{"missing certificate", expected, body, nil, sign(body)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Verify(tc.expected, tc.body, tc.chain, tc.sig); err == nil {
				t.Fatal("invalid actor proof accepted")
			}
		})
	}
}
