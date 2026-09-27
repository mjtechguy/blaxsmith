package repoinspect

import (
	"path"
	"slices"
	"strings"
)

// Baseline describes observable setup, not semantic understanding or passing
// tests. Detection never executes repository files.
type Baseline struct {
	Mode            string    `json:"detected_mode"`
	Scope           string    `json:"scope"`
	FileCount       int       `json:"file_count"`
	Reasons         []string  `json:"reasons"`
	Manifests       []string  `json:"manifests"`
	Instructions    []string  `json:"instructions"`
	SuggestedChecks []Command `json:"suggested_checks,omitempty"`
	Limitations     []string  `json:"limitations"`
}

func InspectBaseline(files []string, contents map[string][]byte, scope string) Baseline {
	b := Baseline{Mode: "greenfield", Scope: scope, Manifests: []string{}, Instructions: []string{}, Reasons: []string{}, Limitations: []string{"File and manifest inspection only; behavior, test outcomes and environment readiness are unverified."}}
	scoped := []string{}
	selected := map[string][]byte{}
	sourceFiles := 0
	for _, file := range files {
		inScope := scope == "." || strings.HasPrefix(file, scope+"/")
		if path.Base(file) == "AGENTS.md" && (scope == "." || path.Dir(file) == "." || path.Dir(file) == scope || strings.HasPrefix(scope, path.Dir(file)+"/") || strings.HasPrefix(file, scope+"/")) {
			b.Instructions = append(b.Instructions, file)
		}
		if !inScope {
			continue
		}
		b.FileCount++
		relative := file
		if scope != "." {
			relative = strings.TrimPrefix(file, scope+"/")
		}
		scoped = append(scoped, relative)
		if data, ok := contents[file]; ok {
			selected[relative] = data
		}
		if slices.Contains(Wanted, relative) {
			b.Manifests = append(b.Manifests, file)
		}
		switch path.Ext(file) {
		case ".go", ".ts", ".tsx", ".js", ".jsx", ".py", ".rs", ".java", ".c", ".cc", ".cpp", ".cs", ".rb", ".php", ".swift", ".kt", ".html", ".sh":
			sourceFiles++
		}
	}
	slices.Sort(b.Manifests)
	slices.Sort(b.Instructions)
	if sourceFiles > 0 || len(b.Manifests) > 0 {
		b.Mode = "brownfield"
		b.Reasons = append(b.Reasons, "Existing source files or build manifests were found in scope.")
	} else if scope != "." && len(files) > b.FileCount {
		b.Mode = "mixed"
		b.Reasons = append(b.Reasons, "The selected scope has no detected implementation; parent or sibling project files exist.")
	} else {
		b.Reasons = append(b.Reasons, "No known source-file extensions or build manifests were detected in scope.")
	}
	r := Inspect(scoped, selected)
	b.SuggestedChecks = r.Verification
	if r.FileError != "" {
		b.Limitations = append(b.Limitations, r.FileError)
	}
	if len(b.SuggestedChecks) == 0 {
		b.Limitations = append(b.Limitations, "No check commands detected in the selected scope. Configure checks explicitly.")
	}
	b.Limitations = append(b.Limitations, "Mode is a filename heuristic; correct it in project inputs when the intended work differs.")
	return b
}
