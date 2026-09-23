package identity

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	passwordMin = 12
	passwordMax = 1024
	argonMemory = 64 * 1024 // KiB
	argonTime   = 3
	argonLanes  = 1
)

const hashPrefix = "$argon2id$v=19$m=65536,t=3,p=1$"

// ponytail: one fixed parameter set; add versioned verification when the
// password policy changes and existing hashes need migration.

var ErrPassword = errors.New("password must be valid UTF-8 and 12–1024 bytes")

func HashPassword(password []byte) (string, error) {
	if len(password) < passwordMin || len(password) > passwordMax || !utf8.Valid(password) {
		return "", ErrPassword
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey(password, salt, argonTime, argonMemory, argonLanes, 32)
	return fmt.Sprintf("%s%s$%s", hashPrefix,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

func VerifyPassword(encoded string, password []byte) bool {
	if len(password) < passwordMin || len(password) > passwordMax || !utf8.Valid(password) || !strings.HasPrefix(encoded, hashPrefix) {
		return false
	}
	saltText, hashText, ok := strings.Cut(strings.TrimPrefix(encoded, hashPrefix), "$")
	if !ok {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(saltText)
	if err != nil || len(salt) != 16 {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(hashText)
	if err != nil || len(want) != 32 {
		return false
	}
	actual := argon2.IDKey(password, salt, argonTime, argonMemory, argonLanes, 32)
	return subtle.ConstantTimeCompare(actual, want) == 1
}

func burnPasswordAttempt(password []byte) {
	// Spend roughly the same work when the account has no local password.
	actual := argon2.IDKey(password, []byte("blaxsmith-no-user"), argonTime, argonMemory, argonLanes, 32)
	clear(actual)
}
