package bootstrap

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strconv"
)

// Envelope is encrypted for the guest key in an attested challenge. Its bytes
// alone confer no authority; the signed one-use release must bind their hash.
type Envelope struct {
	EphemeralKey string `json:"ephemeral_key"`
	Nonce        string `json:"nonce"`
	Ciphertext   string `json:"ciphertext"`
}

func Seal(challenge Challenge, plaintext []byte) (Envelope, error) {
	if len(plaintext) == 0 || len(plaintext) > 32768 {
		return Envelope{}, ErrDenied
	}
	recipientBytes, err := base64.RawURLEncoding.DecodeString(challenge.RecipientKey)
	if err != nil || len(recipientBytes) != 32 {
		return Envelope{}, ErrDenied
	}
	recipient, err := ecdh.X25519().NewPublicKey(recipientBytes)
	if err != nil {
		return Envelope{}, ErrDenied
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return Envelope{}, err
	}
	shared, err := eph.ECDH(recipient)
	if err != nil {
		return Envelope{}, ErrDenied
	}
	context, salt, err := envelopeContext(challenge)
	if err != nil {
		return Envelope{}, err
	}
	key, err := hkdf.Key(sha256.New, shared, salt, "blaxsmith/bootstrap-envelope/v1", 32)
	if err != nil {
		return Envelope{}, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return Envelope{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return Envelope{}, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Envelope{}, err
	}
	return Envelope{base64.RawURLEncoding.EncodeToString(eph.PublicKey().Bytes()),
		base64.RawURLEncoding.EncodeToString(nonce),
		base64.RawURLEncoding.EncodeToString(aead.Seal(nil, nonce, plaintext, context))}, nil
}

// Open is the guest-side counterpart. The private key must be generated after
// activation and kept out of AX task configuration and snapshots.
func Open(challenge Challenge, recipient *ecdh.PrivateKey, envelope Envelope) ([]byte, error) {
	if recipient == nil || challenge.RecipientKey != base64.RawURLEncoding.EncodeToString(recipient.PublicKey().Bytes()) {
		return nil, ErrDenied
	}
	ephBytes, err := base64.RawURLEncoding.DecodeString(envelope.EphemeralKey)
	if err != nil || len(ephBytes) != 32 {
		return nil, ErrDenied
	}
	eph, err := ecdh.X25519().NewPublicKey(ephBytes)
	if err != nil {
		return nil, ErrDenied
	}
	shared, err := recipient.ECDH(eph)
	if err != nil {
		return nil, ErrDenied
	}
	context, salt, err := envelopeContext(challenge)
	if err != nil {
		return nil, err
	}
	key, err := hkdf.Key(sha256.New, shared, salt, "blaxsmith/bootstrap-envelope/v1", 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce, err := base64.RawURLEncoding.DecodeString(envelope.Nonce)
	if err != nil || len(nonce) != aead.NonceSize() {
		return nil, ErrDenied
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(envelope.Ciphertext)
	if err != nil || len(ciphertext) > 32800 {
		return nil, ErrDenied
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, context)
	if err != nil || len(plaintext) == 0 || len(plaintext) > 32768 {
		return nil, ErrDenied
	}
	return plaintext, nil
}

func envelopeContext(challenge Challenge) ([]byte, []byte, error) {
	nonce, err := base64.RawURLEncoding.DecodeString(challenge.Nonce)
	if err != nil || len(nonce) != 32 || challenge.ExpiresAt <= 0 ||
		(challenge.Phase != PhaseSetup && challenge.Phase != PhaseModel) ||
		challenge.Atespace == "" || challenge.Task == "" || challenge.RecipientKey == "" {
		return nil, nil, ErrDenied
	}
	context, _ := json.Marshal([]string{challenge.Phase, challenge.Nonce, strconv.FormatInt(challenge.ExpiresAt, 10), challenge.Atespace, challenge.Task, challenge.RecipientKey})
	return context, nonce, nil
}
