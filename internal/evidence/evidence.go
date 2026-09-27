// Package evidence defines bounded, untrusted worker submissions. Only the
// platform's independent check runner may produce verification observations.
package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

const MaxArtifact = 4 << 20
const MaxArtifacts = 32

var ErrInvalid = errors.New("invalid evidence")
var ID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var digest = regexp.MustCompile(`^[a-f0-9]{64}$`)

func Path(p string) bool {
	return p != "." && filepath.IsLocal(p) && path.Clean(p) == p && len(p) <= 1024 &&
		!strings.ContainsAny(p, "\\\x00\r\n") && !strings.HasPrefix(p, ".git/") && p != ".git"
}
func SHA(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

type Artifact struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	Renderer string `json:"renderer"`
	SHA256   string `json:"sha256"`
}

func (a Artifact) Validate() error {
	if !ID.MatchString(a.ID) || !Path(a.Path) || !slices.Contains([]string{"report", "ledger", "log", "image"}, a.Kind) ||
		!slices.Contains([]string{"markdown", "json", "table", "image"}, a.Renderer) ||
		!utf8.ValidString(a.Title) || strings.TrimSpace(a.Title) == "" || len(a.Title) > 300 || !digest.MatchString(a.SHA256) {
		return ErrInvalid
	}
	return nil
}

type Reference struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type Gate struct {
	ID             string      `json:"id"`
	Check          string      `json:"check"`
	Verdict        string      `json:"verdict"`
	Summary        string      `json:"summary"`
	Evidence       []Reference `json:"evidence"`
	RequirementIDs []string    `json:"requirement_ids,omitempty"`
}

func (g Gate) Validate() error {
	if !ID.MatchString(g.ID) || !ID.MatchString(g.Check) || !slices.Contains([]string{"pass", "fail", "blocked"}, g.Verdict) ||
		!utf8.ValidString(g.Summary) || len(g.Summary) > 4000 || len(g.Evidence) > 32 || len(g.RequirementIDs) > 128 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, e := range g.Evidence {
		if !Path(e.Path) || !digest.MatchString(e.SHA256) || seen[e.Path] {
			return ErrInvalid
		}
		seen[e.Path] = true
	}
	for _, id := range g.RequirementIDs {
		if !ID.MatchString(id) {
			return ErrInvalid
		}
	}
	return nil
}

type Receipt struct {
	GateID   string `json:"gate_id"`
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason,omitempty"`
}
