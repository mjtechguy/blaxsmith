package bootstrap

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"
)

func TestEnvelopeBindsRecipientAndChallenge(t *testing.T) {
	recipient, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	challenge := Challenge{Nonce: base64.RawURLEncoding.EncodeToString(nonce[:]), ExpiresAt: time.Now().Add(time.Minute).Unix(), Phase: PhaseSetup,
		Atespace: "team", Task: "task", RecipientKey: base64.RawURLEncoding.EncodeToString(recipient.PublicKey().Bytes())}
	secret := []byte("synthetic-private-git-token")
	envelope, err := Seal(challenge, secret)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := Open(challenge, recipient, envelope)
	if err != nil || !bytes.Equal(opened, secret) {
		t.Fatalf("open envelope: %v", err)
	}
	changed := challenge
	changed.Task = "another-task"
	if _, err := Open(changed, recipient, envelope); err == nil {
		t.Fatal("envelope opened for another task")
	}
	other, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(challenge, other, envelope); err == nil {
		t.Fatal("envelope opened for another recipient")
	}
	tampered := envelope
	tampered.Ciphertext = base64.RawURLEncoding.EncodeToString(append([]byte("x"), []byte(envelope.Ciphertext)...))
	if _, err := Open(challenge, recipient, tampered); err == nil {
		t.Fatal("tampered envelope opened")
	}
	if _, err := Seal(Challenge{}, secret); err == nil {
		t.Fatal("missing guest key accepted")
	}
}
