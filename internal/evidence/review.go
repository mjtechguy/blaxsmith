package evidence

import (
	"bytes"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"unicode/utf8"
)

// ReviewReport is a bounded agent assessment, never platform test evidence.
type ReviewReport struct {
	Schema       string                  `json:"schema_version"`
	Candidate    string                  `json:"candidate_revision"`
	Verdict      string                  `json:"verdict"`
	Summary      string                  `json:"summary"`
	Findings     []ReviewFinding         `json:"findings"`
	Requirements []RequirementAssessment `json:"requirement_assessment"`
	Limitations  []string                `json:"limitations"`
}
type ReviewFinding struct {
	Severity       string `json:"severity"`
	Path           string `json:"path"`
	Line           int    `json:"line"`
	Evidence       string `json:"evidence"`
	Impact         string `json:"impact"`
	Recommendation string `json:"recommendation"`
}
type RequirementAssessment struct {
	ID         string `json:"requirement_id"`
	Assessment string `json:"assessment"`
	Evidence   string `json:"evidence"`
}

func ParseReview(data []byte, candidate, verdict string) (ReviewReport, error) {
	var report ReviewReport
	if len(data) == 0 || len(data) > 256<<10 {
		return report, ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&report) != nil || decoder.Decode(new(any)) != io.EOF {
		return report, ErrInvalid
	}
	text := func(value string) bool {
		return strings.TrimSpace(value) != "" && len(value) <= 4000 && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
	}
	if report.Schema != "blaxsmith.review/v1alpha1" || report.Candidate != candidate || report.Verdict != verdict || !slices.Contains([]string{"pass", "fail"}, report.Verdict) || !text(report.Summary) || len(report.Findings) > 128 || len(report.Requirements) > 128 || len(report.Limitations) > 32 {
		return report, ErrInvalid
	}
	for _, finding := range report.Findings {
		if !slices.Contains([]string{"critical", "high", "medium", "low", "info"}, finding.Severity) || (finding.Path != "" && !Path(finding.Path)) || finding.Line < 0 || finding.Line > 10000000 || (finding.Path == "" && finding.Line != 0) || !text(finding.Evidence) || !text(finding.Impact) || !text(finding.Recommendation) {
			return report, ErrInvalid
		}
	}
	seen := map[string]bool{}
	for _, requirement := range report.Requirements {
		if !ID.MatchString(requirement.ID) || seen[requirement.ID] || !slices.Contains([]string{"met", "gap", "uncertain"}, requirement.Assessment) || !text(requirement.Evidence) {
			return report, ErrInvalid
		}
		seen[requirement.ID] = true
	}
	for _, limitation := range report.Limitations {
		if !text(limitation) {
			return report, ErrInvalid
		}
	}
	return report, nil
}
