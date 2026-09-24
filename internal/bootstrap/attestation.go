package bootstrap

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"slices"
	"time"
)

var actorIdentityOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11129, 2, 12, 2}

type Expected struct {
	Roots     *x509.CertPool
	Atespace  string
	ActorName string
	ActorUID  string
	Nonce     [32]byte
	Now       time.Time
}

type Challenge struct {
	Nonce        string `json:"nonce"`
	ExpiresAt    int64  `json:"expires_at"`
	Phase        string `json:"phase"`
	Atespace     string `json:"atespace"`
	Task         string `json:"task"`
	RecipientKey string `json:"recipient_key,omitempty"`
}

// Verify checks a Substrate atunnel proof against the connector's own pending
// nonce and current actor identity. The caller must still consume that nonce
// once and recheck execution ownership before signing the runner release.
func Verify(expected Expected, body, chainPEM, signature []byte) (Challenge, error) {
	if expected.Roots == nil || expected.Atespace == "" || expected.ActorName == "" || expected.ActorUID == "" || expected.Nonce == [32]byte{} ||
		len(body) == 0 || len(body) > 2048 || len(chainPEM) == 0 || len(chainPEM) > 16384 || len(signature) == 0 {
		return Challenge{}, fmt.Errorf("incomplete actor proof")
	}
	if expected.Now.IsZero() {
		expected.Now = time.Now()
	}
	var certs []*x509.Certificate
	for len(chainPEM) > 0 {
		block, rest := pem.Decode(chainPEM)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return Challenge{}, fmt.Errorf("malformed actor certificate chain")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return Challenge{}, fmt.Errorf("parsing actor certificate: %w", err)
		}
		certs = append(certs, cert)
		chainPEM = bytes.TrimSpace(rest)
	}
	if len(certs) == 0 || len(certs) > 4 {
		return Challenge{}, fmt.Errorf("invalid actor certificate chain")
	}
	leaf := certs[0]
	if leaf.IsCA || leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 || !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth) {
		return Challenge{}, fmt.Errorf("actor certificate cannot sign proofs")
	}
	intermediates := x509.NewCertPool()
	for _, cert := range certs[1:] {
		intermediates.AddCert(cert)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: expected.Roots, Intermediates: intermediates,
		CurrentTime: expected.Now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return Challenge{}, fmt.Errorf("actor certificate is not trusted: %w", err)
	}
	var identityBytes []byte
	for _, ext := range leaf.Extensions {
		if ext.Id.Equal(actorIdentityOID) {
			if identityBytes != nil {
				return Challenge{}, fmt.Errorf("duplicate actor identity")
			}
			identityBytes = ext.Value
		}
	}
	var identity struct {
		Atespace  string
		ActorName string
		ActorUid  string
		Purpose   string
	}
	if len(identityBytes) == 0 || json.Unmarshal(identityBytes, &identity) != nil ||
		identity.Atespace != expected.Atespace || identity.ActorName != expected.ActorName ||
		identity.ActorUid != expected.ActorUID || identity.Purpose != "atunnel" {
		return Challenge{}, fmt.Errorf("actor certificate identity mismatch")
	}
	key, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return Challenge{}, fmt.Errorf("unsupported actor proof key")
	}
	h := sha256.New()
	h.Write([]byte("blaxsmith/actor-proof/v1\x00"))
	h.Write(expected.Nonce[:])
	h.Write(body)
	if !ecdsa.VerifyASN1(key, h.Sum(nil), signature) {
		return Challenge{}, fmt.Errorf("invalid actor proof signature")
	}
	var challenge Challenge
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&challenge); err != nil {
		return Challenge{}, fmt.Errorf("invalid guest challenge")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Challenge{}, fmt.Errorf("invalid guest challenge")
	}
	guestNonce, err := base64.RawURLEncoding.DecodeString(challenge.Nonce)
	if err != nil || len(guestNonce) != 32 ||
		(challenge.Phase != PhaseSetup && challenge.Phase != PhaseModel) ||
		challenge.Atespace != expected.Atespace || challenge.Task != expected.ActorName ||
		!time.Unix(challenge.ExpiresAt, 0).After(expected.Now) || time.Unix(challenge.ExpiresAt, 0).After(expected.Now.Add(2*time.Minute)) {
		return Challenge{}, fmt.Errorf("guest challenge is stale or mismatched")
	}
	if challenge.RecipientKey != "" {
		key, err := base64.RawURLEncoding.DecodeString(challenge.RecipientKey)
		if err != nil || len(key) != 32 {
			return Challenge{}, fmt.Errorf("invalid guest recipient key")
		}
		if _, err := ecdh.X25519().NewPublicKey(key); err != nil {
			return Challenge{}, fmt.Errorf("invalid guest recipient key")
		}
	}
	return challenge, nil
}
