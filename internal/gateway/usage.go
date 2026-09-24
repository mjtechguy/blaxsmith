package gateway

import (
	"bytes"
	"encoding/json"
	"strings"
)

// API shapes the gateway meters. Nothing is translated between them.
const (
	APIAnthropicMessages = "anthropic_messages"
	APIOpenAIResponses   = "openai_responses"
	APIOpenAIChat        = "openai_chat"
	APIOther             = "other"
)

// Usage is normalized token counts from a provider's final usage block.
// Input excludes cache reads and writes (OpenAI's prompt_tokens includes its
// cached tokens; they are split out so every price applies once).
// Reasoning is informational: providers already count it in Output.
type Usage struct {
	Input, Output, CacheRead, CacheWrite, Reasoning int64
	ServedModel                                     string
	Reported                                        bool
}

func apiFor(path string) string {
	switch {
	case path == "/v1/messages":
		return APIAnthropicMessages
	case path == "/v1/responses":
		return APIOpenAIResponses
	case path == "/v1/chat/completions":
		return APIOpenAIChat
	}
	return APIOther
}

// maxEventBytes bounds one buffered SSE event or JSON body. Responses API
// completion events repeat the whole output, so this is generous; a larger
// event is still streamed to the client, only not parsed for usage.
const maxEventBytes = 16 << 20

// meter parses usage out of a response body as it streams past, holding at
// most one SSE event (or one JSON body) in memory. It never stores content.
type meter struct {
	api      string
	streamed bool
	usage    Usage
	line     []byte // partial line
	event    []byte // current SSE event's data lines
	body     []byte // non-streamed JSON body
	overflow bool
}

func newMeter(api string, streamed bool) *meter { return &meter{api: api, streamed: streamed} }

func (m *meter) Write(p []byte) {
	if m.api == APIOther {
		return
	}
	if !m.streamed {
		if len(m.body)+len(p) > maxEventBytes {
			m.overflow = true
			m.body = nil
			return
		}
		if !m.overflow {
			m.body = append(m.body, p...)
		}
		return
	}
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if len(m.line)+len(p) <= maxEventBytes {
				m.line = append(m.line, p...)
			} else {
				m.line, m.overflow = nil, true
			}
			return
		}
		m.line = append(m.line, p[:i]...)
		m.handleLine(bytes.TrimSuffix(m.line, []byte("\r")))
		m.line = m.line[:0]
		p = p[i+1:]
	}
}

func (m *meter) handleLine(line []byte) {
	if len(line) == 0 { // event boundary
		if len(m.event) > 0 && !m.overflow {
			m.parse(m.event)
		}
		m.event, m.overflow = m.event[:0], false
		return
	}
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return // event:, id:, retry: and ": keep-alive" comments carry no usage
	}
	data = bytes.TrimPrefix(data, []byte(" "))
	if len(m.event)+len(data)+1 > maxEventBytes {
		m.overflow, m.event = true, m.event[:0]
		return
	}
	if len(m.event) > 0 {
		m.event = append(m.event, '\n')
	}
	m.event = append(m.event, data...)
}

// Finish parses a trailing event without a blank line, or the whole body.
func (m *meter) Finish() Usage {
	if m.streamed {
		if len(m.line) > 0 {
			m.handleLine(m.line)
			m.line = nil
		}
		m.handleLine(nil)
	} else if len(m.body) > 0 && !m.overflow {
		m.parse(m.body)
	}
	return m.usage
}

type anthropicUsage struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
}

type openAIUsage struct {
	// Responses API names.
	InputTokens        *int64 `json:"input_tokens"`
	OutputTokens       *int64 `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	// Chat Completions names.
	PromptTokens        *int64 `json:"prompt_tokens"`
	CompletionTokens    *int64 `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	// Anthropic-style cache writes, as some OpenAI-compatible providers
	// (OpenCode Zen) report them for Claude models.
	CacheWriteTokens *int64 `json:"cache_write_tokens"`
}

func (m *meter) parse(data []byte) {
	if bytes.Equal(data, []byte("[DONE]")) || !bytes.Contains(data, []byte("usage")) && !bytes.Contains(data, []byte(`"model"`)) {
		return
	}
	switch m.api {
	case APIAnthropicMessages:
		var event struct {
			Type    string          `json:"type"`
			Model   string          `json:"model"`
			Usage   *anthropicUsage `json:"usage"`
			Message *struct {
				Model string          `json:"model"`
				Usage *anthropicUsage `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(data, &event) != nil {
			return
		}
		if event.Message != nil { // message_start
			m.served(event.Message.Model)
			m.anthropic(event.Message.Usage)
		}
		m.served(event.Model) // non-streamed message
		m.anthropic(event.Usage)
	case APIOpenAIResponses:
		var event struct {
			Type     string       `json:"type"`
			Model    string       `json:"model"`
			Usage    *openAIUsage `json:"usage"`
			Response *struct {
				Model string       `json:"model"`
				Usage *openAIUsage `json:"usage"`
			} `json:"response"`
		}
		if json.Unmarshal(data, &event) != nil {
			return
		}
		if event.Response != nil { // response.created/.completed/.failed/.incomplete
			m.served(event.Response.Model)
			m.openAI(event.Response.Usage)
		}
		m.served(event.Model)
		m.openAI(event.Usage)
	case APIOpenAIChat:
		var event struct {
			Model string       `json:"model"`
			Usage *openAIUsage `json:"usage"`
		}
		if json.Unmarshal(data, &event) != nil {
			return
		}
		m.served(event.Model)
		m.openAI(event.Usage)
	}
}

func (m *meter) served(model string) {
	if model != "" && len(model) <= 128 && !strings.ContainsAny(model, "\r\n\x00") {
		m.usage.ServedModel = model
	}
}

// anthropic merges message_start (input and cache counts, a provisional
// output count) with message_delta (the final cumulative output count, and on
// newer API versions final input counts too): the last value reported wins.
func (m *meter) anthropic(u *anthropicUsage) {
	if u == nil {
		return
	}
	m.usage.Reported = true
	set := func(dst *int64, v *int64) {
		if v != nil && *v >= 0 {
			*dst = *v
		}
	}
	set(&m.usage.Input, u.InputTokens)
	set(&m.usage.Output, u.OutputTokens)
	set(&m.usage.CacheWrite, u.CacheCreationInputTokens)
	set(&m.usage.CacheRead, u.CacheReadInputTokens)
}

func (m *meter) openAI(u *openAIUsage) {
	if u == nil {
		return
	}
	var input, output, cached, reasoning int64
	switch {
	case u.InputTokens != nil || u.OutputTokens != nil:
		input, output = deref(u.InputTokens), deref(u.OutputTokens)
		cached, reasoning = u.InputTokensDetails.CachedTokens, u.OutputTokensDetails.ReasoningTokens
	case u.PromptTokens != nil || u.CompletionTokens != nil:
		input, output = deref(u.PromptTokens), deref(u.CompletionTokens)
		cached, reasoning = u.PromptTokensDetails.CachedTokens, u.CompletionTokensDetails.ReasoningTokens
	default:
		return
	}
	if input < 0 || output < 0 || cached < 0 || reasoning < 0 {
		return
	}
	m.usage.Reported = true
	m.usage.Input, m.usage.Output = max(input-cached, 0), output
	m.usage.CacheRead, m.usage.Reasoning = min(cached, input), reasoning
	m.usage.CacheWrite = max(deref(u.CacheWriteTokens), 0)
}

func deref(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
