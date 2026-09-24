package identity

import (
	"context"
	"net/http"
)

// CallbackCaller authenticates a top-level cross-site GET returning from an
// OAuth provider. Such a navigation carries the Lax session cookie but no
// same-origin Origin header, so the caller must bind the request to state it
// issued to this session (the OAuth state and PKCE verifier) before acting.
func (g *BrowserGuard) CallbackCaller(ctx context.Context, header http.Header) (Caller, error) {
	caller, err := g.manager.ValidateAccess(ctx, cookieValue(header, accessCookie))
	if err != nil {
		return Caller{}, browserAuthError(err)
	}
	return caller, nil
}
