// Package guild runs the pinned upstream Forge gate without importing Guild's
// Claude-specific dispatch, mutable local state, or plugin hooks.
package guild

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/limit"
)

const Revision = "dda615434dfb4624e1ab6328851afc91ef58e5e1"

//go:embed upstream/validate-spec.py
var validator []byte

type Result struct {
	Revision        string `json:"revision"`
	ValidatorSHA256 string `json:"validator_sha256"`
	Report          string `json:"report"`
}

func Validate(ctx context.Context, spec, transcript []byte) (Result, error) {
	result := Result{Revision: Revision, ValidatorSHA256: fmt.Sprintf("%x", sha256.Sum256(validator))}
	dir, err := os.MkdirTemp("", "blaxsmith-forge-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(dir)
	for name, data := range map[string][]byte{"validate-spec.py": validator, "spec.md": spec, "transcript.md": transcript} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			return result, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "-I", "validate-spec.py", "spec.md", "transcript.md")
	cmd.Dir = dir
	output := limit.Buffer{Max: 1 << 20}
	cmd.Stdout, cmd.Stderr = &output, &output
	err = cmd.Run()
	result.Report = strings.TrimSpace(output.String())
	if err != nil {
		return result, fmt.Errorf("Guild Forge gate failed: %w\n%s", err, result.Report)
	}
	return result, nil
}
