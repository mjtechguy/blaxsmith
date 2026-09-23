// Package runnerexit signs the platform's observation of one AX command exit.
// A signature authenticates the connector's readback; it does not certify the
// command's work or the contents of an evidence artifact.
package runnerexit

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
)

const Schema = "blaxsmith.exit-report/v1alpha1"

var ErrInvalid = errors.New("invalid AX command exit report")

// ExitReport is fixed-order JSON for Ed25519 signing. The platform fills all
// fields after authenticating the current actor and reading its runner endpoint.
type ExitReport struct {
	Schema          string `json:"schema"`
	OrganizationID  string `json:"organization_id"`
	RunID           string `json:"run_id"`
	TaskID          string `json:"task_id"`
	AttemptID       string `json:"attempt_id"`
	OwnerGeneration int64  `json:"owner_generation"`
	AXAtespace      string `json:"ax_atespace"`
	AXTask          string `json:"ax_task"`
	ActorUID        string `json:"actor_uid"`
	TemplateUID     string `json:"template_uid"`
	ActivationNonce string `json:"activation_nonce"`
	CommandSHA256   string `json:"command_sha256"`
	ExitCode        int    `json:"exit_code"`
	Signal          int    `json:"signal"`
	Interrupted     bool   `json:"interrupted"`
	Sequence        int64  `json:"sequence"`
	EvidenceSHA256  string `json:"evidence_sha256"`
	ObservedAt      int64  `json:"observed_at"` // Unix nanoseconds
}

type SignedReport struct {
	Report    ExitReport `json:"report"`
	Signature string     `json:"signature"` // standard base64 Ed25519 signature
}

// Canonical returns the only bytes that may be signed for this schema. JSON
// field order comes from ExitReport's declared fields, not caller input order.
func Canonical(report ExitReport) ([]byte, error) {
	if report.Schema != Schema || report.OrganizationID == "" || report.RunID == "" ||
		report.TaskID == "" || report.AttemptID == "" || report.OwnerGeneration < 1 ||
		report.AXAtespace == "" || report.AXTask == "" || report.ActorUID == "" ||
		report.TemplateUID == "" || report.Sequence != 1 || report.ObservedAt < 1 ||
		report.ExitCode < -1 || report.ExitCode > 255 || report.Signal < 0 || report.Signal > 64 ||
		(report.ExitCode == -1) != (report.Signal != 0) ||
		!digest(report.CommandSHA256) || !digest(report.EvidenceSHA256) {
		return nil, ErrInvalid
	}
	nonce, err := base64.RawURLEncoding.DecodeString(report.ActivationNonce)
	if err != nil || len(nonce) != 32 || base64.RawURLEncoding.EncodeToString(nonce) != report.ActivationNonce {
		return nil, ErrInvalid
	}
	body, err := json.Marshal(report)
	if err != nil {
		return nil, err
	}
	return append([]byte("blaxsmith/exit-report/v1\n"), body...), nil
}

func Sign(report ExitReport, private ed25519.PrivateKey) (SignedReport, error) {
	if len(private) != ed25519.PrivateKeySize {
		return SignedReport{}, ErrInvalid
	}
	message, err := Canonical(report)
	if err != nil {
		return SignedReport{}, err
	}
	return SignedReport{Report: report, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(private, message))}, nil
}

func Verify(signed SignedReport, public ed25519.PublicKey) error {
	if len(public) != ed25519.PublicKeySize {
		return ErrInvalid
	}
	message, err := Canonical(signed.Report)
	if err != nil {
		return err
	}
	signature, err := base64.StdEncoding.DecodeString(signed.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(public, message, signature) {
		return ErrInvalid
	}
	return nil
}

func digest(s string) bool {
	if len(s) != 64 {
		return false
	}
	b, err := hex.DecodeString(s)
	return err == nil && hex.EncodeToString(b) == s
}

// CommandSHA256 matches the pinned AX runner's JSON-array hash of spec.command.
func CommandSHA256(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	body, _ := json.Marshal(argv)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
