package recipe

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// FieldError is a validation failure at a JSON path such as
// "stages[3].loop.max_cycles"; "$" is the whole document.
type FieldError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (e *FieldError) Error() string { return e.Message }

func fieldErr(path, format string, args ...any) error {
	return &FieldError{Path: path, Message: fmt.Sprintf(format, args...)}
}

func stagePath(index map[string]int, id string) string {
	return fmt.Sprintf("stages[%d]", index[id])
}

// MaxRecipeBytes bounds a stored or committed recipe document.
const MaxRecipeBytes = maxArtifactBytes

// Validate parses and validates recipe bytes exactly as Freeze does, without
// resolving any repository files. Referenced prompt and skill paths are only
// syntax-checked here; Freeze checks them against the launch commit.
func Validate(data []byte) (Recipe, []string, *FieldError) {
	if len(data) < 1 || len(data) > MaxRecipeBytes {
		return Recipe{}, nil, &FieldError{"$", fmt.Sprintf("recipe must contain 1–%d bytes", MaxRecipeBytes)}
	}
	if !utf8.Valid(data) || bytes.ContainsRune(data, 0) {
		return Recipe{}, nil, &FieldError{"$", "recipe must be UTF-8 text without NUL bytes"}
	}
	r, err := parse(data)
	if err != nil {
		return Recipe{}, nil, parseFieldError(data, err)
	}
	order, err := r.validate()
	if err != nil {
		var fe *FieldError
		if errors.As(err, &fe) {
			return Recipe{}, nil, fe
		}
		return Recipe{}, nil, &FieldError{"$", err.Error()}
	}
	return r, order, nil
}

func parseFieldError(data []byte, err error) *FieldError {
	var syntax *json.SyntaxError
	var typed *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax):
		line, col := position(data, syntax.Offset)
		return &FieldError{"$", fmt.Sprintf("JSON syntax error at line %d, column %d: %s", line, col, syntax.Error())}
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		line, col := position(data, int64(len(data)))
		return &FieldError{"$", fmt.Sprintf("JSON ends early at line %d, column %d", line, col)}
	case errors.As(err, &typed) && typed.Field != "":
		return &FieldError{typed.Field, fmt.Sprintf("expected %s, got JSON %s", typed.Type, typed.Value)}
	}
	return &FieldError{"$", strings.TrimPrefix(err.Error(), "json: ")}
}

// Harnesses, Efforts, and StageKinds are the editor's choices. Validation
// requires a supported harness and kind; efforts are the values each
// harness's adapter documents, and any well-formed effort still validates.
var (
	Harnesses  = []string{"claude-code", "codex", "opencode"}
	StageKinds = []string{"plan", "interview", "research", "implement", "review", "verify", "integrate",
		"ui_review", "documentation", "architect_review", "human_review"}
	Efforts = map[string][]string{
		"claude-code": {"low", "medium", "high", "xhigh", "max"},
		"codex":       {"minimal", "low", "medium", "high", "xhigh"},
		"opencode":    {"provider-default", "low", "medium", "high"},
	}
)

// ValidPath reports whether p is a clean repository-relative file path.
func ValidPath(p string) bool { return validPath(p) }

func position(data []byte, offset int64) (int, int) {
	offset = min(max(offset, 0), int64(len(data)))
	before := data[:offset]
	line := bytes.Count(before, []byte("\n")) + 1
	return line, int(offset) - bytes.LastIndexByte(before, '\n')
}
