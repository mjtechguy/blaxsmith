package main

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/mjtechguy/blaxsmith/internal/bootstrap"
)

func main() {
	proofPath := flag.String("proof", "", "synthetic proof JSON")
	caPath := flag.String("ca", "", "cluster actor-identity CA PEM")
	flag.Parse()
	if *proofPath == "" || *caPath == "" {
		panic("-proof and -ca are required")
	}
	var input struct {
		Atespace, ActorName, ActorUID              string
		RequestNonce, Body, Certificate, Signature string
	}
	data, err := os.ReadFile(*proofPath)
	must(err)
	must(json.Unmarshal(data, &input))
	ca, err := os.ReadFile(*caPath)
	must(err)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		panic("no actor CA certificate")
	}
	nonce, err := base64.RawURLEncoding.DecodeString(input.RequestNonce)
	must(err)
	if len(nonce) != 32 {
		panic("request nonce must be 32 bytes")
	}
	body, err := base64.StdEncoding.DecodeString(input.Body)
	must(err)
	chain, err := base64.StdEncoding.DecodeString(input.Certificate)
	must(err)
	signature, err := base64.StdEncoding.DecodeString(input.Signature)
	must(err)
	expected := bootstrap.Expected{Roots: roots, Atespace: input.Atespace, ActorName: input.ActorName, ActorUID: input.ActorUID}
	copy(expected.Nonce[:], nonce)
	challenge, err := bootstrap.Verify(expected, body, chain, signature)
	must(err)
	checks := []string{"cluster CA, actor UID, signature, nonce, guest challenge verified"}
	wrongUID := expected
	wrongUID.ActorUID += "-wrong"
	if _, err := bootstrap.Verify(wrongUID, body, chain, signature); err == nil {
		panic("wrong actor UID accepted")
	}
	checks = append(checks, "wrong actor UID rejected")
	wrongNonce := expected
	wrongNonce.Nonce[0] ^= 0xff
	if _, err := bootstrap.Verify(wrongNonce, body, chain, signature); err == nil {
		panic("wrong connector nonce accepted")
	}
	checks = append(checks, "wrong connector nonce rejected")
	tampered := append([]byte{}, body...)
	tampered[0] ^= 1
	if _, err := bootstrap.Verify(expected, tampered, chain, signature); err == nil {
		panic("tampered guest challenge accepted")
	}
	checks = append(checks, "tampered guest challenge rejected")
	wrongCA := expected
	wrongCA.Roots = x509.NewCertPool()
	if _, err := bootstrap.Verify(wrongCA, body, chain, signature); err == nil {
		panic("wrong cluster CA accepted")
	}
	checks = append(checks, "wrong cluster CA rejected")
	result := struct {
		ActorUID   string   `json:"actor_uid"`
		GuestNonce string   `json:"guest_nonce"`
		Checks     []string `json:"checks"`
	}{input.ActorUID, challenge.Nonce, checks}
	must(json.NewEncoder(os.Stdout).Encode(result))
}

func must(err error) {
	if err != nil {
		panic(fmt.Sprintf("actor proof verification: %v", err))
	}
}
