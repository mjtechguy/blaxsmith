package interact

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

var headingLine = regexp.MustCompile(`(?m)^#`)

// quote keeps interaction text from opening a new Forge transcript block.
func quote(s string) string { return headingLine.ReplaceAllString(strings.TrimSpace(s), `\#`) }

// RenderTranscript renders answered question and interview_round records in
// the Forge interview shape that guild.Validate parses:
//
//	## Q-001 / **Question:** / **Options presented:** a | b
//	## A-001 [TAG] / <answer> [from Q-001]
//
// Rounds answered only with their finalize option end the interview and are
// not answers, so they are left out (they would otherwise need spec citations).
func RenderTranscript(title string, records []Record) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# Interview Transcript: %s\n\n*Verbatim Q/A record rendered by Blaxsmith from structured interactions.*\n\n---\n", strings.TrimSpace(title))
	n := 0
	for _, r := range records {
		if r.State != "answered" || r.Answer == nil || (r.Kind != "question" && r.Kind != "interview_round") {
			continue
		}
		labels := map[string]string{}
		var presented []string
		for _, o := range r.Options {
			labels[o.ID] = o.Label
			presented = append(presented, quote(o.Label))
		}
		var chosen []string
		for _, id := range r.Answer.OptionIDs {
			if r.Interview != nil && id == r.Interview.FinalizeOption {
				continue
			}
			chosen = append(chosen, quote(labels[id]))
		}
		text := quote(r.Answer.Text)
		if len(chosen) == 0 && text == "" {
			continue
		}
		n++
		fmt.Fprintf(&b, "\n## Q-%03d\n**Question:** %s\n", n, quote(r.Title))
		if r.BodyMD != "" {
			fmt.Fprintf(&b, "%s\n", quote(r.BodyMD))
		}
		if len(presented) > 0 {
			fmt.Fprintf(&b, "**Options presented:** %s\n", strings.Join(presented, " | "))
		}
		fmt.Fprintf(&b, "\n## A-%03d", n)
		if r.Interview != nil && len(r.Interview.Tags) > 0 {
			fmt.Fprintf(&b, " [%s]", strings.Join(r.Interview.Tags, ", "))
		}
		answer := strings.Join(chosen, "; ")
		if text != "" {
			answer = strings.TrimSpace(answer + "\n" + text)
		}
		fmt.Fprintf(&b, "\n%s [from Q-%03d]\n", answer, n)
	}
	return []byte(b.String())
}

// StageTranscript renders one stage's interview for guild.Validate.
func (s *Store) StageTranscript(ctx context.Context, orgID, runID, stageKey string) ([]byte, error) {
	records, err := s.ListInteractions(ctx, orgID, runID)
	if err != nil {
		return nil, err
	}
	var stage []Record
	for _, r := range records {
		if r.Stage == stageKey {
			stage = append(stage, r)
		}
	}
	if len(stage) == 0 {
		return nil, workflow.ErrNotFound
	}
	return RenderTranscript(stageKey, stage), nil
}
