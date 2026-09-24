package access

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ErrKeyRejected means the provider answered 401/403 to the key itself.
var ErrKeyRejected = errors.New("provider rejected the API key")

// CatalogModel is one provider model as listed by its models endpoint.
type CatalogModel struct {
	ID, DisplayName string
	ReleasedAt      *time.Time
	ContextTokens   int
	Capabilities    json.RawMessage
	Meta            ModelMeta // What the provider itself reported; see ResolveModel.
}

// ModelCatalog lists a provider's models with a key, which also validates it.
// Endpoints (checked 2026-09-24):
//   - OpenAI: GET https://api.openai.com/v1/models, Bearer key.
//   - Anthropic: GET https://api.anthropic.com/v1/models?limit=1000, x-api-key
//     and anthropic-version; paged by after_id while has_more.
//   - OpenCode Zen/Go: GET https://opencode.ai/zen[/go]/v1/models is public, so
//     the key is checked separately (see checkOpenCodeKey).
type ModelCatalog struct {
	Client *http.Client
	// Base overrides the provider origin (tests only).
	Base map[string]string
}

func (c ModelCatalog) base(provider string) string {
	if b := c.Base[provider]; b != "" {
		return b
	}
	return modelOrigin(provider)
}

func (c ModelCatalog) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func (c ModelCatalog) Fetch(ctx context.Context, provider string, key []byte) ([]CatalogModel, error) {
	if modelOrigin(provider) == "" || len(key) == 0 {
		return nil, ErrDenied
	}
	switch provider {
	case "openai":
		return c.openAIList(ctx, c.base(provider)+"/v1/models", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+string(key))
		})
	case "anthropic":
		return c.anthropicList(ctx, key)
	default: // opencode, opencode-go
		base := c.base(provider)
		models, err := c.openAIList(ctx, base+"/models", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+string(key))
		})
		if err != nil {
			return nil, err
		}
		if len(models) == 0 {
			return nil, errors.New("provider listed no models")
		}
		return models, c.checkOpenCodeKey(ctx, base, models[0].ID, key)
	}
}

func (c ModelCatalog) get(ctx context.Context, rawURL string, auth func(*http.Request), into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	auth(req)
	return c.do(req, into)
}

func (c ModelCatalog) do(req *http.Request, into any) error {
	req.Header.Set("Accept", "application/json")
	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("provider unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrKeyRejected
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("provider returned HTTP %d", resp.StatusCode)
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("provider returned an unreadable model list")
	}
	return nil
}

func (c ModelCatalog) openAIList(ctx context.Context, rawURL string, auth func(*http.Request)) ([]CatalogModel, error) {
	var page struct {
		Data []struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
		} `json:"data"`
	}
	if err := c.get(ctx, rawURL, auth, &page); err != nil {
		return nil, err
	}
	out := make([]CatalogModel, 0, len(page.Data))
	for _, m := range page.Data {
		item := CatalogModel{ID: m.ID, DisplayName: m.ID}
		if m.Created > 0 {
			t := time.Unix(m.Created, 0).UTC()
			item.ReleasedAt = &t
		}
		out = append(out, item)
	}
	return out, nil
}

func (c ModelCatalog) anthropicList(ctx context.Context, key []byte) ([]CatalogModel, error) {
	var out []CatalogModel
	after := ""
	for range 5 {
		q := url.Values{"limit": {"1000"}}
		if after != "" {
			q.Set("after_id", after)
		}
		var page struct {
			Data []struct {
				ID             string          `json:"id"`
				DisplayName    string          `json:"display_name"`
				CreatedAt      time.Time       `json:"created_at"`
				MaxInputTokens int             `json:"max_input_tokens"`
				Capabilities   json.RawMessage `json:"capabilities"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		err := c.get(ctx, c.base("anthropic")+"/v1/models?"+q.Encode(), func(r *http.Request) {
			r.Header.Set("x-api-key", string(key))
			r.Header.Set("anthropic-version", "2023-06-01")
		}, &page)
		if err != nil {
			return nil, err
		}
		for _, m := range page.Data {
			item := CatalogModel{ID: m.ID, DisplayName: m.DisplayName, ContextTokens: m.MaxInputTokens}
			if !m.CreatedAt.IsZero() && m.CreatedAt.Unix() > 0 {
				t := m.CreatedAt.UTC()
				item.ReleasedAt = &t
			}
			if len(m.Capabilities) > 0 && string(m.Capabilities) != "null" {
				item.Capabilities = m.Capabilities
				item.Meta.Efforts = anthropicEfforts(m.Capabilities)
			}
			out = append(out, item)
		}
		if !page.HasMore || page.LastID == "" {
			break
		}
		after = page.LastID
	}
	return out, nil
}

// checkOpenCodeKey sends an empty chat request for a listed model. Zen
// answers an unknown key with an AuthError body (HTTP 401) before running
// anything; it answers an unknown model with a ModelError, also 401, so the
// status alone does not identify the key. Any non-AuthError answer (normally
// a 400 for the empty request) means the key was accepted.
func (c ModelCatalog) checkOpenCodeKey(ctx context.Context, base, model string, key []byte) error {
	payload, err := json.Marshal(map[string]any{"model": model, "messages": []any{}, "max_tokens": 1})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+string(key))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("provider unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if bytes.Contains(body, []byte(`"AuthError"`)) {
		return ErrKeyRejected
	}
	return nil
}
