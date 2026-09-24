package bootstrap

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Runtime is read from the trusted AX/Substrate control plane, never a Task
// manifest or worker response. It is compared again immediately before release.
type Runtime struct {
	Actor              Actor
	TemplateUID        string
	Image              string
	SandboxClass       string
	BootstrapPublicKey string
	WorkerPod          string
	WorkerPodUID       string
	WorkerPool         string
	SnapshotOnPause    string
	SnapshotOnCommit   string
	ResumeFromData     string
	SnapshotStorage    string
}

func (r Runtime) DataOnlySnapshots() bool {
	return r.SnapshotOnPause == "SNAPSHOT_CONTENT_SCOPE_DATA" &&
		r.SnapshotOnCommit == "SNAPSHOT_CONTENT_SCOPE_DATA" &&
		r.ResumeFromData == "RESUME_SOURCE_GOLDEN" && r.SnapshotStorage != ""
}

// Connector opens an actor-attested setup or post-readiness model gate. The caller must authenticate
// the scheduler assignment and implement Current and Authorize against trusted
// control-plane/policy state.
type Connector struct {
	Ledger    *Ledger
	Client    *http.Client
	RouterURL string
	Token     func(context.Context) (string, error)
	Roots     *x509.CertPool
	Signer    ed25519.PrivateKey
	Current   func(context.Context) (Runtime, error)
	// Authorize and credential callbacks must use the supplied release transaction for
	// policy/credential reads whose revocation must fence delivery.
	Authorize func(context.Context, pgx.Tx, Runtime, Challenge) error
	// Reserve commits an access-delivery intent before send; Delivered records
	// an acknowledged send in the release transaction.
	Reserve   func(context.Context, pgx.Tx, Redeemed) error
	Delivered func(context.Context, pgx.Tx, Redeemed) error
	// GitSetup runs only after proof, owner fencing, runtime check, and policy.
	// The returned token byte slice is consumed and cleared by Open.
	GitSetup func(context.Context, pgx.Tx, Runtime) (GitSetup, error)
	// ModelCredential is only used by OpenModel, after AX workspace readiness.
	ModelCredential func(context.Context, pgx.Tx, Runtime) (ModelCredential, error)
}

type GitSetup struct {
	RepoURL  string
	Commit   string
	Username string
	Token    []byte
}

type ModelCredential struct {
	AttemptID string
	Provider  string
	ExpiresAt time.Time
	APIKey    []byte
	// CodexAuthJSON replaces APIKey for a personal Codex login: a native
	// auth.json holding an access token and an empty refresh token.
	CodexAuthJSON []byte
}

func validGitCommit(commit string) bool {
	if len(commit) != 40 && len(commit) != 64 {
		return false
	}
	_, err := hex.DecodeString(commit)
	return err == nil && commit == strings.ToLower(commit)
}

func (c *Connector) Open(ctx context.Context, scope Scope, expected Runtime) error {
	return c.OpenPhase(ctx, scope, expected, PhaseSetup)
}

func (c *Connector) OpenModel(ctx context.Context, scope Scope, expected Runtime) error {
	return c.OpenPhase(ctx, scope, expected, PhaseModel)
}

func (c *Connector) OpenPhase(ctx context.Context, scope Scope, expected Runtime, phase string) error {
	if c.Ledger == nil || c.Client == nil || c.Token == nil || c.Roots == nil ||
		len(c.Signer) != ed25519.PrivateKeySize || c.Current == nil || c.Authorize == nil ||
		(phase != PhaseSetup && phase != PhaseModel) || (phase == PhaseModel && c.ModelCredential == nil) ||
		((c.GitSetup != nil || c.ModelCredential != nil) != (c.Reserve != nil && c.Delivered != nil)) ||
		((c.Reserve == nil) != (c.Delivered == nil)) ||
		expected.Actor.Atespace == "" || expected.Actor.Name == "" || expected.Actor.UID == "" ||
		expected.TemplateUID == "" || expected.Image == "" || expected.SandboxClass == "" || expected.BootstrapPublicKey == "" ||
		expected.WorkerPod == "" || expected.WorkerPodUID == "" || expected.WorkerPool == "" || !expected.DataOnlySnapshots() {
		return ErrDenied
	}
	if expected.BootstrapPublicKey != base64.StdEncoding.EncodeToString(c.Signer.Public().(ed25519.PublicKey)) {
		return ErrDenied
	}
	router, err := url.Parse(c.RouterURL)
	if err != nil || router.Scheme != "https" || router.Host == "" || router.User != nil || router.RawQuery != "" || router.Fragment != "" || router.Path != "" {
		return ErrDenied
	}
	offer, err := c.Ledger.IssuePhase(ctx, scope, phase)
	if err != nil {
		return err
	}
	if offer.ActorAtespace != expected.Actor.Atespace || offer.ActorName != expected.Actor.Name || offer.ActorUID != expected.Actor.UID {
		return ErrDenied
	}
	path := "/blaxsmith/bootstrap/challenge?phase=" + phase
	body, cert, signature, err := c.request(ctx, router, offer, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	redeemed, err := c.Ledger.Redeem(ctx, scope, offer.ID, offer.Nonce,
		Proof{Body: body, CertificatePEM: cert, Signature: signature}, c.Roots)
	if err != nil {
		return err
	}
	var prepare func(context.Context, pgx.Tx) error
	if c.Reserve != nil {
		prepare = func(prepareCtx context.Context, tx pgx.Tx) error { return c.Reserve(prepareCtx, tx, redeemed) }
	}
	_, err = c.Ledger.Release(ctx, redeemed, prepare, func(sendCtx context.Context, tx pgx.Tx) error {
		current, err := c.Current(sendCtx)
		if err != nil {
			return err
		}
		if current != expected {
			return ErrDenied
		}
		challenge := redeemed.Challenge
		if challenge.Phase != phase || offer.Phase != phase {
			return ErrDenied
		}
		if err := c.Authorize(sendCtx, tx, current, challenge); err != nil {
			return err
		}
		message := []byte("blaxsmith/bootstrap/release/v3\n" + phase + "\n" + challenge.Nonce + "\n" +
			strconv.FormatInt(challenge.ExpiresAt, 10) + "\n" + challenge.Atespace + "\n" + challenge.Task + "\n")
		var envelope *Envelope
		var git *GitSetup
		if phase == PhaseSetup && c.GitSetup != nil {
			setup, err := c.GitSetup(sendCtx, tx, current)
			if err != nil {
				return err
			}
			defer clear(setup.Token)
			git = &setup
		}
		var model *ModelCredential
		if phase == PhaseModel {
			credential, err := c.ModelCredential(sendCtx, tx, current)
			if err != nil {
				return err
			}
			defer clear(credential.APIKey)
			defer clear(credential.CodexAuthJSON)
			model = &credential
		}
		if git != nil || model != nil {
			sealed, err := sealCredentials(challenge, redeemed.Scope.AttemptID, git, model)
			if err != nil {
				return err
			}
			envelope = &sealed
		}
		if envelope != nil {
			encoded, _ := json.Marshal(envelope)
			digest := sha256.Sum256(encoded)
			message = []byte("blaxsmith/bootstrap/release/v3\n" + phase + "\n" + challenge.Nonce + "\n" +
				strconv.FormatInt(challenge.ExpiresAt, 10) + "\n" + challenge.Atespace + "\n" + challenge.Task + "\n" +
				base64.RawURLEncoding.EncodeToString(digest[:]) + "\n")
		}
		release, err := json.Marshal(struct {
			Phase     string    `json:"phase"`
			Nonce     string    `json:"nonce"`
			ExpiresAt int64     `json:"expires_at"`
			Signature string    `json:"signature"`
			Envelope  *Envelope `json:"envelope,omitempty"`
		}{phase, challenge.Nonce, challenge.ExpiresAt, base64.StdEncoding.EncodeToString(ed25519.Sign(c.Signer, message)), envelope})
		if err != nil {
			return err
		}
		_, _, _, err = c.request(sendCtx, router, offer, http.MethodPost, "/blaxsmith/bootstrap/release", release)
		if err != nil {
			return err
		}
		if err := c.Ledger.BindActivation(sendCtx, tx, redeemed, current); err != nil {
			return err
		}
		if c.Delivered != nil {
			return c.Delivered(sendCtx, tx, redeemed)
		}
		return nil
	})
	return err
}

type gitCredentialPayload struct {
	RepoURL  string `json:"repo_url"`
	Commit   string `json:"commit"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

type modelCredentialPayload struct {
	Kind      string `json:"kind"`
	AttemptID string `json:"attempt_id"`
	Provider  string `json:"provider"`
	ExpiresAt int64  `json:"expires_at"`
	APIKey    string `json:"api_key,omitempty"`
	// Kind model_codex_auth only. Guest materialization is not implemented
	// yet; the pinned runner rejects this kind, so it fails closed.
	CodexAuthJSON string `json:"codex_auth_json,omitempty"`
}

func sealCredentials(challenge Challenge, attemptID string, git *GitSetup, model *ModelCredential) (Envelope, error) {
	if attemptID == "" || git == nil && model == nil {
		return Envelope{}, ErrDenied
	}
	if challenge.Phase == PhaseSetup && model != nil ||
		challenge.Phase == PhaseModel && (git != nil || model == nil) ||
		(challenge.Phase != PhaseSetup && challenge.Phase != PhaseModel) {
		return Envelope{}, ErrDenied
	}
	var gitPayload *gitCredentialPayload
	if git != nil {
		u, err := url.Parse(git.RepoURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Path == "" || u.Path == "/" ||
			u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !validGitCommit(git.Commit) ||
			git.Username == "" || len(git.Username) > 128 || len(git.Token) == 0 || len(git.Token) > 8192 ||
			strings.ContainsAny(git.RepoURL+git.Username+string(git.Token), "\r\n\x00") {
			return Envelope{}, ErrDenied
		}
		gitPayload = &gitCredentialPayload{git.RepoURL, git.Commit, git.Username, string(git.Token)}
	}
	var modelPayload *modelCredentialPayload
	if model != nil {
		now := time.Now()
		if model.AttemptID != attemptID || !slices.Contains([]string{"openai", "anthropic", "opencode", "opencode-go"}, model.Provider) ||
			!model.ExpiresAt.After(now) || model.ExpiresAt.After(now.Add(time.Hour)) ||
			(len(model.APIKey) == 0) == (len(model.CodexAuthJSON) == 0) {
			return Envelope{}, ErrDenied
		}
		if len(model.CodexAuthJSON) > 0 {
			var file struct {
				Tokens struct {
					RefreshToken *string `json:"refresh_token"`
				} `json:"tokens"`
			}
			if model.Provider != "openai" || len(model.CodexAuthJSON) > 16384 ||
				json.Unmarshal(model.CodexAuthJSON, &file) != nil ||
				file.Tokens.RefreshToken == nil || *file.Tokens.RefreshToken != "" {
				return Envelope{}, ErrDenied
			}
			modelPayload = &modelCredentialPayload{Kind: "model_codex_auth", AttemptID: model.AttemptID,
				Provider: model.Provider, ExpiresAt: model.ExpiresAt.Unix(), CodexAuthJSON: string(model.CodexAuthJSON)}
		} else {
			if len(model.APIKey) > 8192 || strings.ContainsAny(string(model.APIKey), "\r\n\x00") {
				return Envelope{}, ErrDenied
			}
			modelPayload = &modelCredentialPayload{Kind: "model_api_key", AttemptID: model.AttemptID,
				Provider: model.Provider, ExpiresAt: model.ExpiresAt.Unix(), APIKey: string(model.APIKey)}
		}
	}
	payload, err := json.Marshal(struct {
		Schema    string                  `json:"schema"`
		AttemptID string                  `json:"attempt_id"`
		Git       *gitCredentialPayload   `json:"git,omitempty"`
		Model     *modelCredentialPayload `json:"model,omitempty"`
	}{"blaxsmith.bootstrap-capabilities/v1alpha1", attemptID, gitPayload, modelPayload})
	if err != nil {
		return Envelope{}, err
	}
	defer clear(payload)
	return Seal(challenge, payload)
}

func (c *Connector) request(ctx context.Context, router *url.URL, offer Offer, method, path string, body []byte) ([]byte, []byte, []byte, error) {
	token, err := c.Token(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return nil, nil, nil, ErrDenied
	}
	req, err := http.NewRequestWithContext(ctx, method, router.String()+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("ate-target-actor", offer.ActorAtespace+"/"+offer.ActorName)
	req.Header.Set("X-Blaxsmith-Actor-UID", offer.ActorUID)
	if method == http.MethodGet {
		req.Header.Set("X-Blaxsmith-Request-Nonce", base64.RawURLEncoding.EncodeToString(offer.Nonce[:]))
	} else {
		req.Header.Set("Content-Type", "application/json")
	}
	client := *c.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return nil, nil, nil, err
	}
	defer response.Body.Close()
	want := http.StatusOK
	if method == http.MethodPost {
		want = http.StatusNoContent
	}
	if response.StatusCode != want {
		return nil, nil, nil, fmt.Errorf("bootstrap %s denied: HTTP %d", method, response.StatusCode)
	}
	if method == http.MethodPost {
		return nil, nil, nil, nil
	}
	proofBody, err := io.ReadAll(io.LimitReader(response.Body, 2049))
	if err != nil || len(proofBody) > 2048 {
		return nil, nil, nil, ErrDenied
	}
	encodedCert := response.Header.Get("X-Blaxsmith-Actor-Certificate")
	encodedSignature := response.Header.Get("X-Blaxsmith-Actor-Signature")
	if len(encodedCert) > 22000 || len(encodedSignature) > 1024 {
		return nil, nil, nil, ErrDenied
	}
	cert, err := base64.StdEncoding.DecodeString(encodedCert)
	if err != nil {
		return nil, nil, nil, ErrDenied
	}
	signature, err := base64.StdEncoding.DecodeString(encodedSignature)
	if err != nil {
		return nil, nil, nil, ErrDenied
	}
	return proofBody, cert, signature, nil
}
