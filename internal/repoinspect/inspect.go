// Package repoinspect suggests project setup from a repository: the
// committed .blaxsmith.json when it is valid (docs/project-file.md), else
// verification and setup commands detected from root manifests. Results are
// suggestions a person confirms; this package saves nothing.
package repoinspect

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

// ProjectFileName is read from the repository root.
const ProjectFileName = ".blaxsmith.json"

// Command is one check or setup step: an exact argv, no shell.
type Command struct {
	ID      string   `json:"id"`
	Command []string `json:"command"`
}

// ProjectFile is .blaxsmith.json version 1 (docs/project-file.schema.json).
type ProjectFile struct {
	Schema       string    `json:"$schema,omitempty"`
	Version      int       `json:"version"`
	Verification []Command `json:"verification,omitempty"`
	Setup        []Command `json:"setup,omitempty"`
	Recipe       string    `json:"recipe,omitempty"`
}

// Result is what a repository suggests. Source is "file" when a valid
// .blaxsmith.json supplied everything, "detected" when manifests did, and
// "none" otherwise. FileError explains an ignored .blaxsmith.json.
type Result struct {
	Source       string
	FileError    string
	Verification []Command
	Setup        []Command
	Recipe       string
	Evidence     []string // Human-readable reasons, e.g. "go.mod", "package.json scripts (pnpm)".
}

// Wanted lists the root files Inspect reads; every other file only matters
// by name (lockfiles).
var Wanted = []string{ProjectFileName, "package.json", "go.mod", "Makefile", "makefile", "GNUmakefile",
	"pyproject.toml", "setup.cfg", "tox.ini", "Cargo.toml"}

var commandID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// ParseProjectFile strictly decodes and validates .blaxsmith.json.
func ParseProjectFile(data []byte) (ProjectFile, error) {
	var f ProjectFile
	if len(data) > 64<<10 {
		return f, errors.New("file is larger than 64 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&f); err != nil {
		return f, fmt.Errorf("invalid JSON: %s", strings.TrimPrefix(err.Error(), "json: "))
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return f, errors.New("invalid JSON: trailing data after the object")
	}
	if f.Version != 1 {
		return f, fmt.Errorf("unsupported version %d; this Blaxsmith reads version 1", f.Version)
	}
	if len(f.Verification) > 64 || len(f.Setup) > 16 {
		return f, errors.New("at most 64 verification and 16 setup commands")
	}
	for field, list := range map[string][]Command{"verification": f.Verification, "setup": f.Setup} {
		seen := map[string]bool{}
		for i, c := range list {
			if !commandID.MatchString(c.ID) || seen[c.ID] {
				return f, fmt.Errorf("%s[%d].id must be a unique lowercase id", field, i)
			}
			seen[c.ID] = true
			if len(c.Command) == 0 || len(c.Command) > 32 || slices.ContainsFunc(c.Command, func(p string) bool {
				return p == "" || len(p) > 4096 || strings.ContainsRune(p, 0)
			}) {
				return f, fmt.Errorf("%s[%d].command must be 1–32 non-empty arguments", field, i)
			}
		}
	}
	if len(f.Recipe) > 120 || (f.Recipe != "" && strings.TrimSpace(f.Recipe) == "") {
		return f, errors.New("recipe must be a library recipe name of at most 120 characters")
	}
	return f, nil
}

// Inspect builds the suggestion from the repository's file list and the
// contents of the Wanted root files that exist.
func Inspect(files []string, contents map[string][]byte) Result {
	if data, ok := contents[ProjectFileName]; ok {
		f, err := ParseProjectFile(data)
		if err == nil {
			return Result{Source: "file", Verification: f.Verification, Setup: f.Setup, Recipe: f.Recipe,
				Evidence: []string{ProjectFileName}}
		}
		r := detect(files, contents)
		r.FileError = err.Error()
		return r
	}
	return detect(files, contents)
}

func detect(files []string, contents map[string][]byte) Result {
	has := func(name string) bool { return slices.Contains(files, name) }
	r := Result{Source: "none"}
	add := func(list *[]Command, id string, argv ...string) {
		if !slices.ContainsFunc(*list, func(c Command) bool { return c.ID == id }) {
			*list = append(*list, Command{ID: id, Command: argv})
		}
	}

	if data, ok := contents["package.json"]; ok {
		var pkg struct {
			Scripts        map[string]string `json:"scripts"`
			PackageManager string            `json:"packageManager"`
		}
		if json.Unmarshal(data, &pkg) == nil {
			pm, install := nodeManager(has, pkg.PackageManager)
			found := []string{}
			for _, script := range []string{"test", "lint", "typecheck"} {
				body := strings.TrimSpace(pkg.Scripts[script])
				if body == "" || strings.Contains(body, "no test specified") {
					continue
				}
				found = append(found, script)
				if pm == "npm" && script == "test" {
					add(&r.Verification, "test", "npm", "test")
				} else {
					add(&r.Verification, script, pm, "run", script)
				}
			}
			add(&r.Setup, "install", install...)
			evidence := fmt.Sprintf("package.json (%s)", pm)
			if len(found) > 0 {
				evidence = fmt.Sprintf("package.json scripts %s (%s)", strings.Join(found, ", "), pm)
			}
			r.Evidence = append(r.Evidence, evidence)
		}
	}
	if has("go.mod") {
		add(&r.Verification, "go-test", "go", "test", "./...")
		add(&r.Verification, "go-vet", "go", "vet", "./...")
		add(&r.Setup, "go-mod-download", "go", "mod", "download")
		r.Evidence = append(r.Evidence, "go.mod")
	}
	for _, name := range []string{"GNUmakefile", "makefile", "Makefile"} {
		data, ok := contents[name]
		if !ok {
			continue
		}
		targets := []string{}
		for _, target := range []string{"test", "lint", "check"} {
			if regexp.MustCompile(`(?m)^` + target + `\s*:([^=]|$)`).Match(data) {
				targets = append(targets, target)
				add(&r.Verification, "make-"+target, "make", target)
			}
		}
		if len(targets) > 0 {
			r.Evidence = append(r.Evidence, fmt.Sprintf("%s targets %s", name, strings.Join(targets, ", ")))
		}
		break // make reads GNUmakefile, then makefile, then Makefile.
	}
	pyproject := string(contents["pyproject.toml"])
	pytest := strings.Contains(pyproject, "[tool.pytest") || strings.Contains(pyproject, "pytest") || has("pytest.ini") || has("conftest.py") ||
		strings.Contains(string(contents["setup.cfg"]), "[tool:pytest]") || strings.Contains(string(contents["tox.ini"]), "[pytest]")
	if pytest || has("pyproject.toml") {
		run := []string{"python", "-m"}
		switch {
		case has("uv.lock"):
			run = []string{"uv", "run"}
			add(&r.Setup, "uv-sync", "uv", "sync")
		case has("poetry.lock"):
			run = []string{"poetry", "run"}
			add(&r.Setup, "poetry-install", "poetry", "install")
		}
		if pytest {
			add(&r.Verification, "pytest", append(slices.Clone(run), "pytest")...)
			r.Evidence = append(r.Evidence, "pytest configuration")
		}
		if strings.Contains(pyproject, "[tool.ruff") {
			add(&r.Verification, "ruff", append(slices.Clone(run), "ruff", "check", ".")...)
			r.Evidence = append(r.Evidence, "pyproject.toml [tool.ruff]")
		}
	}
	if has("Cargo.toml") {
		add(&r.Verification, "cargo-test", "cargo", "test")
		add(&r.Setup, "cargo-fetch", "cargo", "fetch")
		r.Evidence = append(r.Evidence, "Cargo.toml")
	}
	if len(r.Verification) > 0 || len(r.Setup) > 0 {
		r.Source = "detected"
	}
	return r
}

// nodeManager picks the package manager from the lockfile, else the
// packageManager field, else npm, with its reproducible install command.
func nodeManager(has func(string) bool, field string) (string, []string) {
	pm := "npm"
	switch {
	case has("pnpm-lock.yaml"):
		pm = "pnpm"
	case has("yarn.lock"):
		pm = "yarn"
	case has("bun.lock") || has("bun.lockb"):
		pm = "bun"
	case has("package-lock.json"):
		pm = "npm"
	default:
		if name, _, _ := strings.Cut(field, "@"); name == "pnpm" || name == "yarn" || name == "bun" {
			pm = name
		}
	}
	switch pm {
	case "pnpm":
		return pm, []string{"pnpm", "install", "--frozen-lockfile"}
	case "yarn":
		if has(".yarnrc.yml") {
			return pm, []string{"yarn", "install", "--immutable"}
		}
		return pm, []string{"yarn", "install", "--frozen-lockfile"}
	case "bun":
		return pm, []string{"bun", "install", "--frozen-lockfile"}
	}
	if has("package-lock.json") {
		return pm, []string{"npm", "ci"}
	}
	return pm, []string{"npm", "install"}
}
