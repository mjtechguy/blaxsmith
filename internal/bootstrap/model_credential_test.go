package bootstrap

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
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
	sealed, err := sealCredentials(challenge, "attempt-a", nil, &credential)
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
		Schema    string `json:"schema"`
		AttemptID string `json:"attempt_id"`
		Model     struct {
			Kind      string `json:"kind"`
			AttemptID string `json:"attempt_id"`
			Provider  string `json:"provider"`
			APIKey    string `json:"api_key"`
			ExpiresAt int64  `json:"expires_at"`
		} `json:"model"`
	}
	if err := json.Unmarshal(opened, &payload); err != nil || payload.Schema != "blaxsmith.bootstrap-capabilities/v1alpha1" ||
		payload.AttemptID != "attempt-a" || payload.Model.Kind != "model_api_key" || payload.Model.AttemptID != "attempt-a" ||
		payload.Model.Provider != "openai" || payload.Model.APIKey != string(key) || payload.Model.ExpiresAt < time.Now().Unix() {
		t.Fatalf("sealed provider payload: %+v %v", payload, err)
	}
	gitCredential := GitSetup{RepoURL: "https://github.com/owner/repo.git", Commit: strings.Repeat("a", 40),
		Username: "blaxsmith", Token: []byte("private-git-token")}
	combined, err := sealCredentials(challenge, "attempt-a", &gitCredential, &credential)
	if err != nil {
		t.Fatal(err)
	}
	combinedPayload, err := Open(challenge, guest, combined)
	if err != nil {
		t.Fatal(err)
	}
	var bundle struct {
		AttemptID string `json:"attempt_id"`
		Git       struct {
			RepoURL string `json:"repo_url"`
			Commit  string `json:"commit"`
			Token   string `json:"token"`
		} `json:"git"`
		Model struct {
			APIKey string `json:"api_key"`
		} `json:"model"`
	}
	if err := json.Unmarshal(combinedPayload, &bundle); err != nil || bundle.AttemptID != "attempt-a" ||
		bundle.Git.RepoURL != gitCredential.RepoURL || bundle.Git.Commit != gitCredential.Commit ||
		bundle.Git.Token != string(gitCredential.Token) || bundle.Model.APIKey != string(key) {
		clear(combinedPayload)
		t.Fatalf("combined Git/model payload: %+v %v", bundle, err)
	}
	clear(combinedPayload)
	if _, err := sealCredentials(challenge, "other-attempt", nil, &credential); !errors.Is(err, ErrDenied) {
		t.Fatalf("cross-attempt credential accepted: %v", err)
	}
	credential.ExpiresAt = time.Now().Add(2 * time.Hour)
	if _, err := sealCredentials(challenge, "attempt-a", nil, &credential); !errors.Is(err, ErrDenied) {
		t.Fatalf("unbounded credential accepted: %v", err)
	}
}
