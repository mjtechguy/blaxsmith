package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/access"
)

// Master keys that wrap the per-organization data keys:
//
//	BLAXSMITH_ACCESS_KEY_FILE           current 32-byte key; unset disables credential custody
//	BLAXSMITH_ACCESS_KEY_ID             its ID, default "primary"
//	BLAXSMITH_ACCESS_PREVIOUS_KEY_FILES optional id=path,id=path of older keys kept for
//	                                    decryption while rotation re-wraps data keys
const defaultAccessKeyID = "primary"

var accessKeyIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func appSecretStore(pool *pgxpool.Pool) (*access.SecretStore, error) {
	current, keys, err := loadAccessKeys(os.Getenv)
	if err != nil || keys == nil {
		return nil, err
	}
	defer func() {
		for _, key := range keys {
			clear(key)
		}
	}()
	return access.NewSecretStore(pool, current, keys)
}

// loadAccessKeys fails closed on any malformed entry. It returns nil keys
// only when no access key setting is present at all. The caller clears keys.
func loadAccessKeys(getenv func(string) string) (string, map[string][]byte, error) {
	path := getenv("BLAXSMITH_ACCESS_KEY_FILE")
	current := getenv("BLAXSMITH_ACCESS_KEY_ID")
	previous := getenv("BLAXSMITH_ACCESS_PREVIOUS_KEY_FILES")
	if path == "" {
		if current != "" || previous != "" {
			return "", nil, errors.New("access key ID and previous keys require BLAXSMITH_ACCESS_KEY_FILE")
		}
		return "", nil, nil
	}
	if current == "" {
		current = defaultAccessKeyID
	}
	if !accessKeyIDPattern.MatchString(current) {
		return "", nil, errors.New("BLAXSMITH_ACCESS_KEY_ID is invalid")
	}
	keys := map[string][]byte{}
	fail := func(err error) (string, map[string][]byte, error) {
		for _, key := range keys {
			clear(key)
		}
		return "", nil, err
	}
	key, err := readAccessKey(path)
	if err != nil {
		return fail(err)
	}
	keys[current] = key
	if previous == "" {
		return current, keys, nil
	}
	for entry := range strings.SplitSeq(previous, ",") {
		id, file, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok || !accessKeyIDPattern.MatchString(id) || file == "" {
			return fail(errors.New("BLAXSMITH_ACCESS_PREVIOUS_KEY_FILES entries must be id=path"))
		}
		if _, dup := keys[id]; dup {
			return fail(fmt.Errorf("access key ID %q is configured twice", id))
		}
		key, err := readAccessKey(file)
		if err != nil {
			return fail(fmt.Errorf("previous access key %q: %w", id, err))
		}
		keys[id] = key
	}
	return current, keys, nil
}

func readAccessKey(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o027 != 0 {
		return nil, errors.New("access encryption key must be a private regular file")
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("access encryption key unavailable")
	}
	if len(key) != 32 {
		clear(key)
		return nil, errors.New("access encryption key must be 32 bytes")
	}
	return key, nil
}
