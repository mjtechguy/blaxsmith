package evidence

// Usage is an untrusted harness report, never a billing receipt or budget grant.
// InputTokens includes cache reads/writes; OutputTokens includes reasoning.
// A report is a single autonomous session (Codex/Claude) or step (OpenCode).
type Usage struct {
	Key           string `json:"key"`
	Harness       string `json:"harness"`
	InputTokens   int64  `json:"input_tokens"`
	OutputTokens  int64  `json:"output_tokens"`
	CostMicrosUSD *int64 `json:"cost_micros_usd,omitempty"`
}

const MaxUsageReports = 10000
const MaxUsageValue int64 = 1_000_000_000_000

func (u Usage) Validate() error {
	if !ID.MatchString(u.Key) || (u.Harness != "codex" && u.Harness != "claude-code" && u.Harness != "opencode") ||
		u.InputTokens < 0 || u.InputTokens > MaxUsageValue || u.OutputTokens < 0 || u.OutputTokens > MaxUsageValue ||
		(u.CostMicrosUSD != nil && (*u.CostMicrosUSD < 0 || *u.CostMicrosUSD > MaxUsageValue)) {
		return ErrInvalid
	}
	if u.Harness != "opencode" && u.Key != "session" {
		return ErrInvalid
	}
	return nil
}
