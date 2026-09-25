package gateway

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Metric is one provider-reported limit window (§5): requests, tokens,
// input-tokens or output-tokens.
type Metric struct {
	Limit     int64     `json:"limit"`
	Remaining int64     `json:"remaining"`
	ResetAt   time.Time `json:"reset_at"`
}

// exhausted reports whether the metric cannot serve estimate more units
// before its reset.
func (m Metric) exhausted(estimate int64, now time.Time) bool {
	return m.Limit > 0 && m.Remaining < max(estimate, 1) && m.ResetAt.After(now)
}

// metricNeed is what one request needs of a metric: its token estimate for
// token metrics, one for the requests metric.
func metricNeed(name string, estimate int64) int64 {
	if strings.Contains(name, "tokens") {
		return estimate
	}
	return 1
}

// rateHeaders parses Anthropic's anthropic-ratelimit-* and OpenAI's (and
// OpenAI-compatible providers') x-ratelimit-* headers, plus retry-after.
// Bedrock and Vertex send none; their quota comes from route configuration.
func rateHeaders(h http.Header, now time.Time) (map[string]Metric, time.Duration) {
	metrics := map[string]Metric{}
	for _, name := range []string{"requests", "tokens", "input-tokens", "output-tokens"} {
		prefix := "Anthropic-Ratelimit-" + name
		limit, lok := headerInt(h, prefix+"-Limit")
		remaining, rok := headerInt(h, prefix+"-Remaining")
		if lok || rok {
			m := Metric{Limit: limit, Remaining: remaining}
			if at, err := time.Parse(time.RFC3339, h.Get(prefix+"-Reset")); err == nil {
				m.ResetAt = at
			}
			metrics[name] = m
		}
	}
	for _, name := range []string{"requests", "tokens"} {
		limit, lok := headerInt(h, "X-Ratelimit-Limit-"+name)
		remaining, rok := headerInt(h, "X-Ratelimit-Remaining-"+name)
		if lok || rok {
			m := Metric{Limit: limit, Remaining: remaining}
			if d, err := time.ParseDuration(h.Get("X-Ratelimit-Reset-" + name)); err == nil && d >= 0 {
				m.ResetAt = now.Add(d)
			}
			metrics[name] = m
		}
	}
	return metrics, retryAfter(h, now)
}

// retryAfter reads retry-after-ms, then retry-after as seconds or an HTTP date.
func retryAfter(h http.Header, now time.Time) time.Duration {
	if ms, err := strconv.ParseFloat(h.Get("Retry-After-Ms"), 64); err == nil && ms > 0 && ms < 1e9 {
		return time.Duration(ms * float64(time.Millisecond))
	}
	value := strings.TrimSpace(h.Get("Retry-After"))
	if s, err := strconv.ParseFloat(value, 64); err == nil && s > 0 && s < 1e6 {
		return time.Duration(s * float64(time.Second))
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

func headerInt(h http.Header, name string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(h.Get(name)), 10, 64)
	return n, err == nil && n >= 0
}

// Window is one personal-subscription usage window (§6): used percent of the
// window, its length, and when it resets, as the provider reported it.
type Window struct {
	Name          string
	UsedPct       float64
	WindowMinutes int
	ResetsAt      time.Time
}

// subscriptionWindows reads the usage windows a subscription upstream reports
// on each response. Display only: nothing is paced or pooled on them.
//   - Codex with a ChatGPT sign-in: x-codex-{primary,secondary}-used-percent,
//     -window-minutes, and -reset-at (Unix seconds) or -reset-after-seconds,
//     as codex-rs parses them.
//   - Claude subscription (unofficial, observed): anthropic-ratelimit-unified-
//     <window>-utilization (0–1) and -reset (Unix seconds), e.g. 5h and 7d.
func subscriptionWindows(h http.Header, now time.Time) []Window {
	var out []Window
	for _, name := range []string{"primary", "secondary"} {
		prefix := "X-Codex-" + name + "-"
		used, err := strconv.ParseFloat(h.Get(prefix+"Used-Percent"), 64)
		if err != nil || used < 0 || used > 1000 || math.IsNaN(used) {
			continue
		}
		w := Window{Name: name, UsedPct: used}
		if minutes, ok := headerInt(h, prefix+"Window-Minutes"); ok && minutes > 0 && minutes < 1e6 {
			w.WindowMinutes = int(minutes)
		}
		if at, ok := headerInt(h, prefix+"Reset-At"); ok && at > 0 {
			w.ResetsAt = time.Unix(at, 0).UTC()
		} else if after, ok := headerInt(h, prefix+"Reset-After-Seconds"); ok {
			w.ResetsAt = now.Add(time.Duration(after) * time.Second).UTC()
		}
		out = append(out, w)
	}
	for key := range h {
		lower := strings.ToLower(key)
		rest, ok := strings.CutPrefix(lower, "anthropic-ratelimit-unified-")
		if !ok {
			continue
		}
		window, ok := strings.CutSuffix(rest, "-utilization")
		if !ok || window == "" || len(window) > 24 {
			continue
		}
		util, err := strconv.ParseFloat(h.Get(key), 64)
		if err != nil || util < 0 || util > 10 || math.IsNaN(util) {
			continue
		}
		w := Window{Name: "claude_" + strings.NewReplacer("-", "_", ".", "_").Replace(window), UsedPct: util * 100,
			WindowMinutes: windowMinutes(window)}
		if at, ok := headerInt(h, "Anthropic-Ratelimit-Unified-"+window+"-Reset"); ok && at > 0 {
			w.ResetsAt = time.Unix(at, 0).UTC()
		}
		if validWindowName(w.Name) {
			out = append(out, w)
		}
	}
	return out
}

// windowMinutes reads "5h" or "7d" style window names; 0 when unknown.
func windowMinutes(window string) int {
	n, err := strconv.Atoi(strings.TrimRight(window[:len(window)-1], "_"))
	if err != nil || n <= 0 || n > 10000 {
		return 0
	}
	switch window[len(window)-1] {
	case 'h':
		return n * 60
	case 'd':
		return n * 24 * 60
	}
	return 0
}

func validWindowName(name string) bool {
	if name == "" || len(name) > 32 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}
