package access

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// GitHubOAuthProvider is the provider_kind of the organization's registered
// GitHub OAuth App. Its client secret is the encrypted secret of a
// connection with auth_method GitHubOAuthClient; it is never a Git credential.
const (
	GitHubOAuthProvider = "github_oauth_app"
	GitHubOAuthClient   = "oauth_client"
)

var gitFullName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}(/[A-Za-z0-9][A-Za-z0-9._-]{0,99}){1,4}$`)

// GitAPI talks to GitHub and GitLab on behalf of one stored token. Endpoints:
// GitHub OAuth web flow with PKCE (github.com/login/oauth/authorize and
// /access_token), REST /user, /user/repos, /repos/{full}/branches; GitLab
// REST v4 /projects?membership=true and /projects/{id}/repository/branches.
type GitAPI struct {
	Client *http.Client
	// Base overrides (tests only): "github-web", "github-api", "gitlab".
	Base map[string]string
}

func (g GitAPI) base(name, fallback string) string {
	if b := g.Base[name]; b != "" {
		return b
	}
	return fallback
}

func (g GitAPI) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return &http.Client{Timeout: 20 * time.Second}
}

// PKCE returns a random verifier and its S256 challenge.
func PKCE() (verifier, challenge string, err error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw[:])
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func (g GitAPI) GitHubAuthorizeURL(clientID, redirectURI, state, challenge string) string {
	q := url.Values{"client_id": {clientID}, "redirect_uri": {redirectURI}, "scope": {"repo read:user"},
		"state": {state}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}, "allow_signup": {"false"}}
	return g.base("github-web", "https://github.com") + "/login/oauth/authorize?" + q.Encode()
}

// GitHubExchange redeems a callback code once and returns the user token and login.
func (g GitAPI) GitHubExchange(ctx context.Context, clientID string, clientSecret []byte, code, redirectURI, verifier string) ([]byte, string, error) {
	form := url.Values{"client_id": {clientID}, "client_secret": {string(clientSecret)}, "code": {code},
		"redirect_uri": {redirectURI}, "code_verifier": {verifier}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.base("github-web", "https://github.com")+"/login/oauth/access_token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := g.client().Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("GitHub token exchange failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	defer clear(body)
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err != nil || resp.StatusCode/100 != 2 || json.Unmarshal(body, &out) != nil || out.AccessToken == "" {
		if out.Error != "" && len(out.Error) < 64 {
			return nil, "", fmt.Errorf("GitHub sign-in failed: %s", out.Error)
		}
		return nil, "", errors.New("GitHub sign-in failed")
	}
	token := []byte(out.AccessToken)
	var user struct {
		Login string `json:"login"`
	}
	if err := g.getJSON(ctx, g.base("github-api", "https://api.github.com")+"/user", githubAuth(token), &user); err != nil || user.Login == "" {
		clear(token)
		return nil, "", errors.New("GitHub sign-in returned no user")
	}
	return token, user.Login, nil
}

func githubAuth(token []byte) func(*http.Request) {
	return func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+string(token))
		r.Header.Set("Accept", "application/vnd.github+json")
		r.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
}

func gitlabAuth(token []byte) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("PRIVATE-TOKEN", string(token)) }
}

func (g GitAPI) getJSON(ctx context.Context, rawURL string, auth func(*http.Request), into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	auth(req)
	resp, err := g.client().Do(req)
	if err != nil {
		return fmt.Errorf("Git provider unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrKeyRejected
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("Git provider returned HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(body, into)
}

type GitRepository struct {
	FullName, CloneURL, DefaultBranch string
	Private                           bool
}

// Repositories lists up to 100 repositories the token can reach, filtered by
// a case-insensitive substring of query.
func (g GitAPI) Repositories(ctx context.Context, host string, token []byte, query string) ([]GitRepository, error) {
	var out []GitRepository
	switch host {
	case "github.com":
		var repos []struct {
			FullName      string `json:"full_name"`
			CloneURL      string `json:"clone_url"`
			DefaultBranch string `json:"default_branch"`
			Private       bool   `json:"private"`
		}
		if err := g.getJSON(ctx, g.base("github-api", "https://api.github.com")+
			"/user/repos?per_page=100&sort=updated&affiliation=owner,collaborator,organization_member", githubAuth(token), &repos); err != nil {
			return nil, err
		}
		for _, r := range repos {
			out = append(out, GitRepository{r.FullName, r.CloneURL, r.DefaultBranch, r.Private})
		}
	case "gitlab.com":
		q := url.Values{"membership": {"true"}, "simple": {"true"}, "per_page": {"100"}, "order_by": {"last_activity_at"}}
		if query != "" {
			q.Set("search", query)
		}
		var repos []struct {
			PathWithNamespace string `json:"path_with_namespace"`
			HTTPURL           string `json:"http_url_to_repo"`
			DefaultBranch     string `json:"default_branch"`
			Visibility        string `json:"visibility"`
		}
		if err := g.getJSON(ctx, g.base("gitlab", "https://gitlab.com")+"/api/v4/projects?"+q.Encode(), gitlabAuth(token), &repos); err != nil {
			return nil, err
		}
		for _, r := range repos {
			out = append(out, GitRepository{r.PathWithNamespace, r.HTTPURL, r.DefaultBranch, r.Visibility != "public"})
		}
	default:
		return nil, ErrDenied
	}
	query = strings.ToLower(query)
	filtered := out[:0]
	for _, r := range out {
		if gitFullName.MatchString(r.FullName) && strings.HasPrefix(r.CloneURL, "https://"+host+"/") &&
			(query == "" || strings.Contains(strings.ToLower(r.FullName), query)) {
			filtered = append(filtered, r)
		}
	}
	return filtered, nil
}

// Branches lists up to 100 branch names of fullName.
func (g GitAPI) Branches(ctx context.Context, host string, token []byte, fullName string) ([]string, error) {
	if !gitFullName.MatchString(fullName) {
		return nil, ErrDenied
	}
	var branches []struct {
		Name string `json:"name"`
	}
	var err error
	switch host {
	case "github.com":
		err = g.getJSON(ctx, g.base("github-api", "https://api.github.com")+"/repos/"+fullName+"/branches?per_page=100",
			githubAuth(token), &branches)
	case "gitlab.com":
		err = g.getJSON(ctx, g.base("gitlab", "https://gitlab.com")+"/api/v4/projects/"+url.PathEscape(fullName)+
			"/repository/branches?per_page=100", gitlabAuth(token), &branches)
	default:
		return nil, ErrDenied
	}
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(branches))
	for _, b := range branches {
		if b.Name != "" && len(b.Name) <= 255 && !strings.ContainsAny(b.Name, "\r\n\x00") {
			out = append(out, b.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}
