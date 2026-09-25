// Package gateway is the brokered model gateway (docs/model-gateway-plan.md,
// phase G1): run-scoped tokens minted at model-lease release, pass-through
// proxying of each provider's native API with the real credential injected on
// the platform, and metering from the provider's own usage blocks.
//
// G2 adds routes and pools of organization-owned API-key and cloud
// connections with failover before the first byte, and G4 personal
// subscription routes and the Bedrock and Vertex transports.
//
// Non-goals, enforced by construction: there is no subscription pooling,
// rotation or failover and no client impersonation. Every upstream request
// is sent as itself (the client's own headers and body) under its route's
// credential; a personal subscription is only ever its owner's single route.
package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/access"
)

// TokenPrefix marks gateway tokens so they are recognisable in redaction and
// never mistaken for a provider key.
const TokenPrefix = "bxgw_"

const (
	ModeNative   = "native_raw"
	ModeBrokered = "brokered_gateway"
)

var (
	ErrDenied   = errors.New("gateway token denied")
	ErrDisabled = errors.New("model gateway disabled by your admin")
)

// NewToken returns a fresh opaque token and its storage hash.
func NewToken() (string, [32]byte, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", [32]byte{}, err
	}
	token := TokenPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	return token, HashToken(token), nil
}

// HashToken is the only form a token is stored or looked up in.
func HashToken(token string) [32]byte { return sha256.Sum256([]byte(token)) }

// validToken rejects anything that is not exactly our token shape before a
// database lookup, so provider keys or junk never reach the hash index.
func validToken(token string) bool {
	rest, ok := strings.CutPrefix(token, TokenPrefix)
	if !ok || len(rest) != 43 {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(rest)
	return err == nil
}

// MintRequest is the release-time context the bootstrap connector already
// holds: the attempt, its model binding, and the lease just reserved.
type MintRequest struct {
	Invoke          access.ModelInvoke
	RunID, TaskID   string
	LeaseID         string
	OwnerGeneration int64
}

// Mint issues a gateway token inside the model-release transaction when the
// attempt was dispatched in brokered_gateway mode. It returns ok=false (and
// no error) for native_raw attempts, so the caller seals the raw key as before.
// A brokered attempt whose organization has since turned the gateway off is
// denied rather than silently given the raw key.
func Mint(ctx context.Context, tx pgx.Tx, request MintRequest) (token []byte, ok bool, err error) {
	in := request.Invoke
	if tx == nil || in.OrganizationID == "" || in.ProjectID == "" || in.AttemptID == "" || in.BindingID == "" ||
		request.RunID == "" || request.TaskID == "" || request.LeaseID == "" || request.OwnerGeneration <= 0 {
		return nil, false, ErrDenied
	}
	var mode, harness, stageKey, principal, authMethod string
	var enabled, personalRoutes bool
	err = tx.QueryRow(ctx, `SELECT d.delivery_mode,d.harness,t.task_key,COALESCE(r.initiator_principal_id,''),
		COALESCE(s.enabled,false),COALESCE(s.personal_routes_enabled,false),COALESCE(c.auth_method,'')
		FROM gateway_attempt_delivery d
		JOIN workflow_attempts a ON a.organization_id=d.organization_id AND a.id=d.attempt_id
		JOIN workflow_tasks t ON t.organization_id=a.organization_id AND t.id=a.task_id
		JOIN workflow_runs r ON r.organization_id=a.organization_id AND r.id=a.run_id
		LEFT JOIN gateway_org_settings s ON s.organization_id=d.organization_id
		LEFT JOIN access_bindings b ON b.organization_id=d.organization_id::text AND b.id=$6
		LEFT JOIN access_grants g ON g.organization_id=b.organization_id AND g.id=b.grant_id
		LEFT JOIN access_connections c ON c.organization_id=g.organization_id AND c.id=g.connection_id
		WHERE d.organization_id=$1 AND d.attempt_id=$2 AND a.run_id=$3 AND a.task_id=$4 AND r.project_id=$5
		FOR SHARE OF d`, in.OrganizationID, in.AttemptID, request.RunID, request.TaskID, in.ProjectID, in.BindingID).
		Scan(&mode, &harness, &stageKey, &principal, &enabled, &personalRoutes, &authMethod)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil // dispatched before the gateway existed: native_raw.
	}
	if err != nil {
		return nil, false, fmt.Errorf("read attempt delivery: %w", err)
	}
	if mode != ModeBrokered {
		return nil, false, nil
	}
	if !enabled {
		return nil, false, ErrDisabled
	}
	if authMethod == access.CodexSubscriptionAuth && !personalRoutes {
		// A Codex sign-in goes through the gateway only as a personal
		// subscription route (§6); otherwise it keeps its native delivery.
		return nil, false, nil
	}
	value, hash, err := NewToken()
	if err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO gateway_tokens
		(token_sha256,organization_id,project_id,run_id,attempt_id,task_id,stage_key,principal_id,harness,
		 provider,model,allowed_families,binding_id,grantee_kind,grantee_id,policy_version,lease_id,lease_generation)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,ARRAY[$10],$12,$13,$14,$15,$16,$17)`,
		hash[:], in.OrganizationID, in.ProjectID, request.RunID, in.AttemptID, request.TaskID, stageKey, principal,
		harness, in.Provider, in.Model, in.BindingID, in.GranteeKind, in.GranteeID, in.PolicyVersion,
		request.LeaseID, request.OwnerGeneration); err != nil {
		return nil, false, fmt.Errorf("mint gateway token: %w", err)
	}
	return []byte(value), true, nil
}
