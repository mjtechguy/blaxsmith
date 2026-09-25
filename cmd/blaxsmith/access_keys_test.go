package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAccessKeys(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	current := write("current", bytes.Repeat([]byte{1}, 32), 0o600)
	older := write("older", bytes.Repeat([]byte{2}, 32), 0o600)
	oldest := write("oldest", bytes.Repeat([]byte{3}, 32), 0o440)
	short := write("short", bytes.Repeat([]byte{4}, 31), 0o600)
	public := write("public", bytes.Repeat([]byte{5}, 32), 0o644)
	env := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}

	id, keys, err := loadAccessKeys(env(nil))
	if err != nil || id != "" || keys != nil {
		t.Fatalf("unset: %q %v %v", id, keys, err)
	}
	id, keys, err = loadAccessKeys(env(map[string]string{"BLAXSMITH_ACCESS_KEY_FILE": current}))
	if err != nil || id != "primary" || len(keys) != 1 || !bytes.Equal(keys["primary"], bytes.Repeat([]byte{1}, 32)) {
		t.Fatalf("key file only must behave as before: %q %d %v", id, len(keys), err)
	}
	id, keys, err = loadAccessKeys(env(map[string]string{
		"BLAXSMITH_ACCESS_KEY_FILE":           current,
		"BLAXSMITH_ACCESS_KEY_ID":             "key-2026-09",
		"BLAXSMITH_ACCESS_PREVIOUS_KEY_FILES": "primary=" + older + ", key.2025=" + oldest,
	}))
	if err != nil || id != "key-2026-09" || len(keys) != 3 ||
		!bytes.Equal(keys["primary"], bytes.Repeat([]byte{2}, 32)) || !bytes.Equal(keys["key.2025"], bytes.Repeat([]byte{3}, 32)) {
		t.Fatalf("rotation config: %q %d %v", id, len(keys), err)
	}

	for name, values := range map[string]map[string]string{
		"id without key file":       {"BLAXSMITH_ACCESS_KEY_ID": "next"},
		"previous without key file": {"BLAXSMITH_ACCESS_PREVIOUS_KEY_FILES": "primary=" + older},
		"missing key file":          {"BLAXSMITH_ACCESS_KEY_FILE": filepath.Join(dir, "absent")},
		"short key":                 {"BLAXSMITH_ACCESS_KEY_FILE": short},
		"readable by others":        {"BLAXSMITH_ACCESS_KEY_FILE": public},
		"directory as key":          {"BLAXSMITH_ACCESS_KEY_FILE": dir},
		"invalid current id":        {"BLAXSMITH_ACCESS_KEY_FILE": current, "BLAXSMITH_ACCESS_KEY_ID": "bad id"},
		"entry without =":           {"BLAXSMITH_ACCESS_KEY_FILE": current, "BLAXSMITH_ACCESS_PREVIOUS_KEY_FILES": older},
		"empty entry":               {"BLAXSMITH_ACCESS_KEY_FILE": current, "BLAXSMITH_ACCESS_PREVIOUS_KEY_FILES": "old=" + older + ","},
		"empty id":                  {"BLAXSMITH_ACCESS_KEY_FILE": current, "BLAXSMITH_ACCESS_PREVIOUS_KEY_FILES": "=" + older},
		"empty path":                {"BLAXSMITH_ACCESS_KEY_FILE": current, "BLAXSMITH_ACCESS_PREVIOUS_KEY_FILES": "old="},
		"invalid previous id":       {"BLAXSMITH_ACCESS_KEY_FILE": current, "BLAXSMITH_ACCESS_PREVIOUS_KEY_FILES": "o/ld=" + older},
		"duplicate of current id":   {"BLAXSMITH_ACCESS_KEY_FILE": current, "BLAXSMITH_ACCESS_PREVIOUS_KEY_FILES": "primary=" + older},
		"duplicate previous id":     {"BLAXSMITH_ACCESS_KEY_FILE": current, "BLAXSMITH_ACCESS_PREVIOUS_KEY_FILES": "old=" + older + ",old=" + oldest},
		"missing previous file":     {"BLAXSMITH_ACCESS_KEY_FILE": current, "BLAXSMITH_ACCESS_PREVIOUS_KEY_FILES": "old=" + filepath.Join(dir, "absent")},
		"short previous key":        {"BLAXSMITH_ACCESS_KEY_FILE": current, "BLAXSMITH_ACCESS_PREVIOUS_KEY_FILES": "old=" + short},
		"public previous key":       {"BLAXSMITH_ACCESS_KEY_FILE": current, "BLAXSMITH_ACCESS_PREVIOUS_KEY_FILES": "old=" + public},
	} {
		if id, keys, err := loadAccessKeys(env(values)); err == nil || id != "" || keys != nil {
			t.Errorf("%s: accepted (%q, %d keys)", name, id, len(keys))
		}
	}
}
