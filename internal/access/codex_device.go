package access

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// CodexDevice runs the ChatGPT device-code sign-in that `codex login
// --device-auth` performs (codex-rs/login/src/device_code_auth.rs, checked
// 2026-09-24), with the platform as the client:
//
//  1. POST {issuer}/api/accounts/deviceauth/usercode {"client_id"} returns
//     device_auth_id, user_code, and interval (a string of seconds).
//  2. The user opens {issuer}/codex/device and enters user_code.
//  3. POST {issuer}/api/accounts/deviceauth/token {"device_auth_id","user_code"}
//     answers 403/404 while pending and then authorization_code plus the PKCE
//     code_verifier.
//  4. POST {issuer}/oauth/token (form) grant_type=authorization_code with
//     redirect_uri {issuer}/deviceauth/callback exchanges it once.
//
// The resulting tokens go straight into the same custody as a pasted
// auth.json (CreateCodexConnection); the exchange is never retried because
// the code may already be spent.
type CodexDevice struct {
	Client *http.Client
	Issuer string // Empty selects https://auth.openai.com.
}

// CodexDeviceCode is server-side state; DeviceAuthID never reaches a browser.
type CodexDeviceCode struct {
	DeviceAuthID, UserCode, VerificationURL string
	Interval                                time.Duration
}

// ErrDevicePending means the user has not approved the code yet.
var ErrDevicePending = errors.New("device sign-in pending")

func (d CodexDevice) issuer() string {
	if d.Issuer != "" {
		return strings.TrimRight(d.Issuer, "/")
	}
	return "https://auth.openai.com"
}

func (d CodexDevice) client() *http.Client {
	if d.Client != nil {
		return d.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (d CodexDevice) postJSON(ctx context.Context, path string, body any) (*http.Response, []byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.issuer()+path, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp, data, err
}

func (d CodexDevice) Start(ctx context.Context) (CodexDeviceCode, error) {
	resp, body, err := d.postJSON(ctx, "/api/accounts/deviceauth/usercode", map[string]string{"client_id": codexClientID})
	if err != nil {
		return CodexDeviceCode{}, fmt.Errorf("device sign-in unavailable: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return CodexDeviceCode{}, fmt.Errorf("device sign-in request failed with HTTP %d", resp.StatusCode)
	}
	var out struct {
		DeviceAuthID string          `json:"device_auth_id"`
		UserCode     string          `json:"user_code"`
		UserCodeAlt  string          `json:"usercode"`
		Interval     json.RawMessage `json:"interval"`
	}
	if json.Unmarshal(body, &out) != nil {
		return CodexDeviceCode{}, errors.New("device sign-in returned an unreadable code")
	}
	if out.UserCode == "" {
		out.UserCode = out.UserCodeAlt
	}
	seconds, _ := strconv.Atoi(strings.Trim(strings.TrimSpace(string(out.Interval)), `"`))
	if out.DeviceAuthID == "" || out.UserCode == "" || len(out.DeviceAuthID) > 512 || len(out.UserCode) > 64 {
		return CodexDeviceCode{}, errors.New("device sign-in returned an unreadable code")
	}
	return CodexDeviceCode{DeviceAuthID: out.DeviceAuthID, UserCode: out.UserCode,
		VerificationURL: d.issuer() + "/codex/device", Interval: time.Duration(min(max(seconds, 1), 30)) * time.Second}, nil
}

// Poll checks once. On approval it exchanges the code and returns an
// auth.json-shaped bundle for CreateCodexConnection; the caller must clear it.
func (d CodexDevice) Poll(ctx context.Context, code CodexDeviceCode) ([]byte, error) {
	resp, body, err := d.postJSON(ctx, "/api/accounts/deviceauth/token",
		map[string]string{"device_auth_id": code.DeviceAuthID, "user_code": code.UserCode})
	if err != nil {
		return nil, ErrDevicePending // Transport errors are retried at the next poll.
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		return nil, ErrDevicePending
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("device sign-in failed with HTTP %d", resp.StatusCode)
	}
	var approved struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	if json.Unmarshal(body, &approved) != nil || approved.AuthorizationCode == "" || approved.CodeVerifier == "" {
		return nil, errors.New("device sign-in returned an unreadable approval")
	}
	clear(body)
	form := url.Values{"grant_type": {"authorization_code"}, "code": {approved.AuthorizationCode},
		"redirect_uri": {d.issuer() + "/deviceauth/callback"}, "client_id": {codexClientID},
		"code_verifier": {approved.CodeVerifier}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.issuer()+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenResp, err := d.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("device sign-in token exchange outcome unknown; sign in again: %w", err)
	}
	defer tokenResp.Body.Close()
	tokenBody, err := io.ReadAll(io.LimitReader(tokenResp.Body, 64<<10))
	defer clear(tokenBody)
	if err != nil || tokenResp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("device sign-in token exchange failed with HTTP %d", tokenResp.StatusCode)
	}
	var tokens codexTokens
	defer tokens.clear()
	if json.Unmarshal(tokenBody, &tokens) != nil {
		return nil, errors.New("device sign-in returned unreadable tokens")
	}
	return json.Marshal(map[string]any{"OPENAI_API_KEY": nil, "tokens": tokens,
		"last_refresh": time.Now().UTC().Format(time.RFC3339)})
}
