package tooladapter

import "testing"

// One boundary check: normalization follows each pinned harness's token shape,
// including cache/reasoning, missing values and hostile cost exponents.
func TestHarnessUsage(t *testing.T) {
	for _, tc := range []struct {
		harness, line string
		input, output int64
		cost          string
	}{
		{"codex", `{"type":"turn.completed","usage":{"input_tokens":300,"output_tokens":40}}`, 300, 40, "unknown"},
		{"claude-code", `{"type":"result","subtype":"error_max_turns","usage":{"input_tokens":100,"output_tokens":40,"cache_read_input_tokens":150,"cache_creation_input_tokens":50},"total_cost_usd":0.0000001}`, 300, 40, "1"},
		{"opencode", `{"type":"step_finish","part":{"id":"step1","cost":0,"tokens":{"input":100,"output":30,"reasoning":10,"cache":{"read":150,"write":50}}}}`, 300, 40, "0"},
		{"claude-code", `{"type":"result","usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},"total_cost_usd":1e-99999999}`, 0, 0, "unknown"},
	} {
		got := HarnessUsage(tc.harness, []byte(tc.line))
		if got == nil || got.InputTokens != tc.input || got.OutputTokens != tc.output {
			t.Fatalf("%s normalization: %+v", tc.harness, got)
		}
		if tc.cost == "unknown" {
			if got.CostMicrosUSD != nil {
				t.Fatalf("unknown cost became reported: %+v", got)
			}
		} else if got.CostMicrosUSD == nil || (tc.cost == "1" && *got.CostMicrosUSD != 1) || (tc.cost == "0" && *got.CostMicrosUSD != 0) {
			t.Fatalf("decimal cost: %+v", got)
		}
	}
	for _, line := range []string{
		`{"type":"turn.completed","usage":{}}`,
		`{"type":"turn.completed","usage":{"input_tokens":null,"output_tokens":0}}`,
		`{"type":"turn.completed","usage":{"input_tokens":-1,"output_tokens":0}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1e100,"output_tokens":0}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1.5,"output_tokens":0}}`,
		`{"type":"assistant","usage":{"input_tokens":1,"output_tokens":0}}`,
	} {
		if got := HarnessUsage("codex", []byte(line)); got != nil {
			t.Fatalf("accepted invalid usage: %s", line)
		}
	}
}
