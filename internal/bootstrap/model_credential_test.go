package bootstrap

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestModelCredentialSealedForCurrentAttempt(t *testing.T) {
	guest, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	challenge := Challenge{Nonce: base64.RawURLEncoding.EncodeToString(nonce), ExpiresAt: time.Now().Add(time.Minute).Unix(),
		Atespace: "space", Task: "task", RecipientKey: base64.RawURLEncoding.EncodeToString(guest.PublicKey().Bytes())}
	key := []byte("private-provider-key")
	credential := ModelCredential{AttemptID: "attempt-a", Provider: "openai", ExpiresAt: time.Now().Add(10 * time.Minute), APIKey: key}
	sealed, err := sealModelCredential(challenge, "attempt-a", credential)
	if err != nil {
		t.Fatal(err)
	}
	serialized, _ := json.Marshal(sealed)
	if bytes.Contains(serialized, key) {
		t.Fatal("provider key appeared in release envelope")
	}
	opened, err := Open(challenge, guest, sealed)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Kind      string `json:"kind"`
		AttemptID string `json:"attempt_id"`
		Provider  string `json:"provider"`
		APIKey    string `json:"api_key"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(opened, &payload); err != nil || payload.Kind != "model_api_key" ||
		payload.AttemptID != "attempt-a" || payload.Provider != "openai" || payload.APIKey != string(key) || payload.ExpiresAt < time.Now().Unix() {
		t.Fatalf("sealed provider payload: %+v %v", payload, err)
	}
	if _, err := sealModelCredential(challenge, "other-attempt", credential); !errors.Is(err, ErrDenied) {
		t.Fatalf("cross-attempt credential accepted: %v", err)
	}
	credential.ExpiresAt = time.Now().Add(2 * time.Hour)
	if _, err := sealModelCredential(challenge, "attempt-a", credential); !errors.Is(err, ErrDenied) {
		t.Fatalf("unbounded credential accepted: %v", err)
	}
}
