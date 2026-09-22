package bootstrap

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
}

// Connector only opens the pre-workspace gate. It never carries a credential.
// The caller must authenticate the scheduler assignment and implement Current
// and Authorize against trusted control-plane and policy state.
type Connector struct {
	Ledger    *Ledger
	Client    *http.Client
	RouterURL string
	Token     func(context.Context) (string, error)
	Roots     *x509.CertPool
	Signer    ed25519.PrivateKey
	Current   func(context.Context) (Runtime, error)
	Authorize func(context.Context, Runtime) error
}

func (c *Connector) Open(ctx context.Context, scope Scope, expected Runtime) error {
	if c.Ledger == nil || c.Client == nil || c.Token == nil || c.Roots == nil ||
		len(c.Signer) != ed25519.PrivateKeySize || c.Current == nil || c.Authorize == nil ||
		expected.Actor.Atespace == "" || expected.Actor.Name == "" || expected.Actor.UID == "" ||
		expected.TemplateUID == "" || expected.Image == "" || expected.SandboxClass == "" || expected.BootstrapPublicKey == "" ||
		expected.WorkerPod == "" || expected.WorkerPodUID == "" || expected.WorkerPool == "" {
		return ErrDenied
	}
	if expected.BootstrapPublicKey != base64.StdEncoding.EncodeToString(c.Signer.Public().(ed25519.PublicKey)) {
		return ErrDenied
	}
	router, err := url.Parse(c.RouterURL)
	if err != nil || router.Scheme != "https" || router.Host == "" || router.User != nil || router.RawQuery != "" || router.Fragment != "" || router.Path != "" {
		return ErrDenied
	}
	offer, err := c.Ledger.Issue(ctx, scope)
	if err != nil {
		return err
	}
	if offer.ActorAtespace != expected.Actor.Atespace || offer.ActorName != expected.Actor.Name || offer.ActorUID != expected.Actor.UID {
		return ErrDenied
	}
	body, cert, signature, err := c.request(ctx, router, offer, http.MethodGet, "/blaxsmith/bootstrap/challenge", nil)
	if err != nil {
		return err
	}
	redeemed, err := c.Ledger.Redeem(ctx, scope, offer.ID, offer.Nonce,
		Proof{Body: body, CertificatePEM: cert, Signature: signature}, c.Roots)
	if err != nil {
		return err
	}
	_, err = c.Ledger.Release(ctx, redeemed, func(sendCtx context.Context) error {
		current, err := c.Current(sendCtx)
		if err != nil {
			return err
		}
		if current != expected {
			return ErrDenied
		}
		if err := c.Authorize(sendCtx, current); err != nil {
			return err
		}
		challenge := redeemed.Challenge
		message := []byte("blaxsmith/bootstrap/release/v1\n" + challenge.Nonce + "\n" +
			strconv.FormatInt(challenge.ExpiresAt, 10) + "\n" + challenge.Atespace + "\n" + challenge.Task + "\n")
		release, err := json.Marshal(struct {
			Nonce     string `json:"nonce"`
			ExpiresAt int64  `json:"expires_at"`
			Signature string `json:"signature"`
		}{challenge.Nonce, challenge.ExpiresAt, base64.StdEncoding.EncodeToString(ed25519.Sign(c.Signer, message))})
		if err != nil {
			return err
		}
		_, _, _, err = c.request(sendCtx, router, offer, http.MethodPost, "/blaxsmith/bootstrap/release", release)
		return err
	})
	return err
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
