package gateway

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/crc32"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Anthropic on Amazon Bedrock (§2): the same Messages body, sent with AWS
// SigV4 to bedrock-runtime InvokeModel / InvokeModelWithResponseStream. The
// streamed reply arrives as AWS event-stream frames and is re-framed as the
// Anthropic SSE the harness expects. Only the transport and auth change.

// AWSCredential is an organization's AWS access key stored as a connection
// secret (auth_method aws_sigv4). Never logged or returned.
type AWSCredential struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token,omitempty"`
}

// ParseAWSCredential validates the stored shape.
func ParseAWSCredential(raw []byte) (AWSCredential, error) {
	var c AWSCredential
	if err := json.Unmarshal(raw, &c); err != nil || len(c.AccessKeyID) < 16 || len(c.AccessKeyID) > 128 ||
		len(c.SecretAccessKey) < 16 || len(c.SecretAccessKey) > 256 || len(c.SessionToken) > 4096 ||
		strings.ContainsAny(c.AccessKeyID+c.SecretAccessKey+c.SessionToken, " \r\n\x00") {
		return AWSCredential{}, errors.New("AWS credential must be JSON with access_key_id and secret_access_key")
	}
	return c, nil
}

func bedrockEndpoint(region string) string {
	return "https://bedrock-runtime." + region + ".amazonaws.com"
}

// bedrockBody moves the request into Bedrock's shape: no model or stream
// fields (they are in the URL), anthropic_version in the body, and any
// anthropic-beta header values as the anthropic_beta body field.
func bedrockBody(body []byte, betas []string) ([]byte, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, false, err
	}
	var stream bool
	_ = json.Unmarshal(fields["stream"], &stream)
	delete(fields, "model")
	delete(fields, "stream")
	fields["anthropic_version"] = json.RawMessage(`"bedrock-2023-05-31"`)
	if len(betas) > 0 {
		encoded, _ := json.Marshal(betas)
		fields["anthropic_beta"] = encoded
	}
	out, err := json.Marshal(fields)
	return out, stream, err
}

// bedrockCountBody wraps an InvokeModel body for Bedrock CountTokens:
// {"input":{"invokeModel":{"body":<base64>}}}. A count request has no
// max_tokens, which InvokeModel requires, so a placeholder of 1 is added
// (it does not change the input count).
func bedrockCountBody(invoke []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(invoke, &fields); err != nil {
		return nil, err
	}
	if _, ok := fields["max_tokens"]; !ok {
		fields["max_tokens"] = json.RawMessage(`1`)
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	type invokeModel struct {
		Body []byte `json:"body"` // base64 in JSON
	}
	return json.Marshal(map[string]any{"input": map[string]any{"invokeModel": invokeModel{Body: body}}})
}

func headerList(h http.Header, name string) []string {
	var out []string
	for _, value := range h.Values(name) {
		for _, v := range strings.Split(value, ",") {
			if v = strings.TrimSpace(v); v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

// awsEscape is SigV4's URI encoding: RFC 3986 unreserved characters stay,
// everything else is %XX (upper case).
func awsEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

// signSigV4 signs r in place (AWS Signature Version 4, header form). The
// canonical URI encodes each segment of the already-escaped path again, as
// every service but S3 requires. It signs host, x-amz-date, any
// x-amz-security-token, and content-type when present.
func signSigV4(r *http.Request, body []byte, c AWSCredential, region, service string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	r.Header.Set("X-Amz-Date", amzDate)
	if c.SessionToken != "" {
		r.Header.Set("X-Amz-Security-Token", c.SessionToken)
	}
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	signed := map[string]string{"host": host, "x-amz-date": amzDate}
	if c.SessionToken != "" {
		signed["x-amz-security-token"] = c.SessionToken
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		signed["content-type"] = ct
	}
	names := make([]string, 0, len(signed))
	for name := range signed {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name + ":" + strings.Join(strings.Fields(signed[name]), " ") + "\n")
	}
	segments := strings.Split(r.URL.EscapedPath(), "/")
	for i, s := range segments {
		segments[i] = awsEscape(s)
	}
	uri := strings.Join(segments, "/")
	if uri == "" {
		uri = "/"
	}
	query := r.URL.Query()
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var pairs []string
	for _, k := range keys {
		values := append([]string(nil), query[k]...)
		sort.Strings(values)
		for _, v := range values {
			pairs = append(pairs, awsEscape(k)+"="+awsEscape(v))
		}
	}
	payload := sha256.Sum256(body)
	signedHeaders := strings.Join(names, ";")
	canonical := strings.Join([]string{r.Method, uri, strings.Join(pairs, "&"), canonicalHeaders.String(),
		signedHeaders, hex.EncodeToString(payload[:])}, "\n")
	scope := date + "/" + region + "/" + service + "/aws4_request"
	digest := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(digest[:])
	key := hmacSHA256([]byte("AWS4"+c.SecretAccessKey), date)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, toSign))
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.AccessKeyID+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// eventStreamSSE re-frames AWS event-stream messages (prelude with total
// and header lengths and a CRC, headers, payload, message CRC) as Anthropic
// SSE events. Each Bedrock chunk payload is {"bytes":"<base64 event JSON>"}.
type eventStreamSSE struct {
	pending []byte
	failed  bool
}

const maxEventStreamFrame = 16 << 20

// Write consumes upstream bytes and returns the SSE bytes for every
// complete frame. A corrupt frame ends conversion with an SSE error event.
func (c *eventStreamSSE) Write(p []byte) []byte {
	if c.failed {
		return nil
	}
	c.pending = append(c.pending, p...)
	var out []byte
	for len(c.pending) >= 12 {
		total := int(binary.BigEndian.Uint32(c.pending[0:4]))
		if total < 16 || total > maxEventStreamFrame {
			return append(out, c.fail("malformed Bedrock event stream")...)
		}
		if len(c.pending) < total {
			break
		}
		event, err := decodeEventFrame(c.pending[:total])
		if err != nil {
			return append(out, c.fail(err.Error())...)
		}
		out = append(out, event...)
		c.pending = append(c.pending[:0], c.pending[total:]...)
	}
	return out
}

func (c *eventStreamSSE) fail(message string) []byte {
	c.failed, c.pending = true, nil
	return sseError("api_error", message)
}

func sseError(kind, message string) []byte {
	body, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": kind, "message": message}})
	return []byte("event: error\ndata: " + string(body) + "\n\n")
}

func decodeEventFrame(frame []byte) ([]byte, error) {
	total := len(frame)
	headersLen := int(binary.BigEndian.Uint32(frame[4:8]))
	if crc32.ChecksumIEEE(frame[:8]) != binary.BigEndian.Uint32(frame[8:12]) ||
		crc32.ChecksumIEEE(frame[:total-4]) != binary.BigEndian.Uint32(frame[total-4:]) ||
		12+headersLen > total-4 {
		return nil, errors.New("Bedrock event stream checksum mismatch")
	}
	headers, err := eventHeaders(frame[12 : 12+headersLen])
	if err != nil {
		return nil, err
	}
	payload := frame[12+headersLen : total-4]
	switch headers[":message-type"] {
	case "event":
		if headers[":event-type"] != "chunk" {
			return nil, nil
		}
		var chunk struct {
			Bytes string `json:"bytes"`
		}
		if err := json.Unmarshal(payload, &chunk); err != nil {
			return nil, errors.New("malformed Bedrock chunk")
		}
		event, err := base64.StdEncoding.DecodeString(chunk.Bytes)
		if err != nil {
			return nil, errors.New("malformed Bedrock chunk")
		}
		var typed struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(event, &typed) != nil || typed.Type == "" || strings.ContainsAny(typed.Type, "\r\n") {
			return nil, errors.New("malformed Bedrock chunk")
		}
		return []byte("event: " + typed.Type + "\ndata: " + string(bytes.TrimSpace(event)) + "\n\n"), nil
	case "exception", "error":
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(payload, &e)
		kind := "api_error"
		if exception := strings.ToLower(headers[":exception-type"]); strings.Contains(exception, "throttling") {
			kind = "rate_limit_error"
		} else if strings.Contains(exception, "validation") {
			kind = "invalid_request_error"
		}
		return sseError(kind, firstNonEmpty(e.Message, headers[":exception-type"], "Bedrock stream error")), nil
	}
	return nil, nil
}

// eventHeaders reads event-stream headers; only string values are kept.
func eventHeaders(b []byte) (map[string]string, error) {
	out := map[string]string{}
	bad := errors.New("malformed Bedrock event headers")
	for len(b) > 0 {
		n := int(b[0])
		if len(b) < 2+n {
			return nil, bad
		}
		name := string(b[1 : 1+n])
		kind := b[1+n]
		b = b[2+n:]
		size := map[byte]int{0: 0, 1: 0, 2: 1, 3: 2, 4: 4, 5: 8, 8: 8, 9: 16}
		switch kind {
		case 6, 7:
			if len(b) < 2 {
				return nil, bad
			}
			l := int(binary.BigEndian.Uint16(b[:2]))
			if len(b) < 2+l {
				return nil, bad
			}
			if kind == 7 {
				out[name] = string(b[2 : 2+l])
			}
			b = b[2+l:]
		default:
			l, ok := size[kind]
			if !ok || len(b) < l {
				return nil, bad
			}
			b = b[l:]
		}
	}
	return out, nil
}
