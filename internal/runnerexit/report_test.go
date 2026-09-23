package runnerexit

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func TestSignedReportBindsAttemptAndExit(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	evidenceSHA256 := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	report := ExitReport{Schema: Schema, OrganizationID: "org", RunID: "run", TaskID: "task", AttemptID: "attempt",
		OwnerGeneration: 1, AXAtespace: "space", AXTask: "ax-task", ActorUID: "actor", TemplateUID: "template",
		ActivationNonce: base64.RawURLEncoding.EncodeToString(nonce), CommandSHA256: CommandSHA256([]string{"true"}),
		ExitCode: 0, Sequence: 1, EvidenceSHA256: evidenceSHA256, ObservedAt: 1}
	signed, err := Sign(report, private)
	if err != nil || Verify(signed, public) != nil {
		t.Fatalf("signed report failed verification: %v", err)
	}
	for _, mutate := range []func(*ExitReport){
		func(r *ExitReport) { r.AttemptID = "replacement" },
		func(r *ExitReport) { r.ExitCode = 1 },
		func(r *ExitReport) { r.ActivationNonce = base64.RawURLEncoding.EncodeToString(make([]byte, 32)) },
		func(r *ExitReport) { r.Interrupted = true },
	} {
		changed := signed
		mutate(&changed.Report)
		if Verify(changed, public) == nil {
			t.Fatalf("accepted modified report: %+v", changed.Report)
		}
	}
	signed.Signature = base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	if Verify(signed, public) == nil {
		t.Fatal("accepted forged signature")
	}
}
