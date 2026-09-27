package tooladapter

import (
	"encoding/json"
	"math/big"
	"strconv"
	"strings"

	"github.com/mjtechguy/blaxsmith/internal/evidence"
)

// HarnessUsage reads only terminal usage events. Assistant/tool text cannot
// claim usage. The entire sandbox remains untrusted; these are reported totals.
// The worker starts exactly one autonomous session per attempt; takeover uses
// a TUI, whose extra usage is unavailable here. Never sum session snapshots.
func HarnessUsage(harness string, line []byte) *evidence.Usage {
	if len(line) > 256<<10 {
		return nil
	}
	var event struct {
		Type  string `json:"type"`
		Usage *struct {
			Input  *int64 `json:"input_tokens"`
			Output *int64 `json:"output_tokens"`
			Read   *int64 `json:"cache_read_input_tokens"`
			Write  *int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
		Cost json.Number `json:"total_cost_usd"`
		Part *struct {
			ID     string      `json:"id"`
			Cost   json.Number `json:"cost"`
			Tokens *struct {
				Input     *int64 `json:"input"`
				Output    *int64 `json:"output"`
				Reasoning *int64 `json:"reasoning"`
				Cache     *struct {
					Read  *int64 `json:"read"`
					Write *int64 `json:"write"`
				} `json:"cache"`
			} `json:"tokens"`
		} `json:"part"`
	}
	if json.Unmarshal(line, &event) != nil {
		return nil
	}
	u := evidence.Usage{Key: "session", Harness: harness}
	valid := func(n *int64) bool { return n != nil && *n >= 0 && *n <= evidence.MaxUsageValue }
	switch {
	case harness == "codex" && event.Type == "turn.completed":
		if event.Usage == nil || !valid(event.Usage.Input) || !valid(event.Usage.Output) {
			return nil
		}
		u.InputTokens, u.OutputTokens = *event.Usage.Input, *event.Usage.Output
	case harness == "claude-code" && event.Type == "result":
		v := event.Usage
		if v == nil || !valid(v.Input) || !valid(v.Output) || !valid(v.Read) || !valid(v.Write) {
			return nil
		}
		u.InputTokens, u.OutputTokens = *v.Input+*v.Read+*v.Write, *v.Output
		u.CostMicrosUSD = reportedCost(event.Cost)
	case harness == "opencode" && event.Type == "step_finish":
		p := event.Part
		if p == nil || p.ID == "" || len(p.ID) > 256 || p.Tokens == nil {
			return nil
		}
		v := p.Tokens
		if !valid(v.Input) || !valid(v.Output) || !valid(v.Reasoning) || v.Cache == nil || !valid(v.Cache.Read) || !valid(v.Cache.Write) {
			return nil
		}
		// Stable native step identity, bounded and free of arbitrary guest text.
		u.Key = evidence.SHA([]byte(p.ID))
		u.InputTokens, u.OutputTokens = *v.Input+*v.Cache.Read+*v.Cache.Write, *v.Output+*v.Reasoning
		u.CostMicrosUSD = reportedCost(p.Cost)
	default:
		return nil
	}
	if u.Validate() != nil {
		return nil
	}
	return &u
}

// Decimal arithmetic avoids float rounding and keeps missing cost distinct
// from an explicit zero. Round up fractions of a microdollar for display.
func reportedCost(n json.Number) *int64 {
	if len(n) == 0 || len(n) > 32 {
		return nil
	}
	// Bound exponent before big.Rat allocates powers of ten (including underflow).
	if i := strings.IndexAny(string(n), "eE"); i >= 0 {
		exponent, err := strconv.Atoi(string(n)[i+1:])
		if err != nil || exponent < -99 || exponent > 99 {
			return nil
		}
	}
	// Bound value before decimal conversion.
	if f, err := strconv.ParseFloat(string(n), 64); err != nil || f < 0 || f > 1_000_000 {
		return nil
	}
	r, ok := new(big.Rat).SetString(string(n))
	if !ok || r.Sign() < 0 {
		return nil
	}
	r.Mul(r, big.NewRat(1_000_000, 1))
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(r.Num(), r.Denom(), rem)
	if rem.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() || q.Int64() > evidence.MaxUsageValue {
		return nil
	}
	value := q.Int64()
	return &value
}
