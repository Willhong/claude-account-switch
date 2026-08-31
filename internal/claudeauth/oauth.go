package claudeauth

import (
	"bytes"
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
	"strings"
	"time"
)

// OAuth endpoints and the public client id Claude Code itself uses. These are
// read out of the shipped Claude Code binary; override them with the
// environment variables below if Anthropic moves them.
const (
	DefaultClientID      = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	DefaultTokenURL      = "https://platform.claude.com/v1/oauth/token"
	DefaultAuthorizeURL  = "https://claude.com/cai/oauth/authorize"
	DefaultAPIBase       = "https://api.anthropic.com"
	ManualRedirectURI    = "https://platform.claude.com/oauth/code/callback"
	defaultRefreshWindow = 30 * 24 * time.Hour // assumed when the server omits refresh_token_expires_in
)

// LoginScopes is the scope set Claude Code requests for a claude.ai login.
var LoginScopes = []string{
	"org:create_api_key",
	"user:profile",
	"user:inference",
	"user:sessions:claude_code",
	"user:mcp_servers",
	"user:file_upload",
}

// RefreshScopes is the (narrower) set Claude Code sends on a refresh grant.
var RefreshScopes = []string{
	"user:profile",
	"user:inference",
	"user:sessions:claude_code",
	"user:mcp_servers",
	"user:file_upload",
}

// ErrInvalidGrant means the refresh token is no longer accepted: the account
// has to log in again. `cas clean` treats this as a dead slot.
var ErrInvalidGrant = errors.New("refresh token rejected (invalid_grant)")

// Config carries the endpoint set, overridable for staging or self-hosted setups.
type Config struct {
	ClientID     string
	TokenURL     string
	AuthorizeURL string
	APIBase      string
	HTTP         *http.Client
}

// NewConfig returns the production configuration, honouring CAS_* overrides.
func NewConfig(env func(string) string) Config {
	pick := func(key, fallback string) string {
		if v := strings.TrimSpace(env(key)); v != "" {
			return v
		}
		return fallback
	}
	return Config{
		ClientID:     pick("CAS_OAUTH_CLIENT_ID", DefaultClientID),
		TokenURL:     pick("CAS_OAUTH_TOKEN_URL", DefaultTokenURL),
		AuthorizeURL: pick("CAS_OAUTH_AUTHORIZE_URL", DefaultAuthorizeURL),
		APIBase:      strings.TrimRight(pick("CAS_API_BASE", DefaultAPIBase), "/"),
		HTTP:         &http.Client{Timeout: 30 * time.Second},
	}
}

func (c Config) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// PKCE holds one authorization attempt's proof key and CSRF state.
type PKCE struct {
	Verifier  string
	Challenge string
	State     string
}

// NewPKCE generates a fresh S256 challenge pair and state value.
func NewPKCE() (*PKCE, error) {
	verifier, err := randomURLSafe(32)
	if err != nil {
		return nil, err
	}
	state, err := randomURLSafe(32)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(verifier))
	return &PKCE{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
		State:     state,
	}, nil
}

func randomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// AuthorizeURL builds the browser URL for an authorization request.
// loginHint pre-fills the email on the consent screen when non-empty.
func (c Config) AuthorizeURLFor(p *PKCE, redirectURI, loginHint string) string {
	u, err := url.Parse(c.AuthorizeURL)
	if err != nil {
		return c.AuthorizeURL
	}
	q := u.Query()
	q.Set("code", "true")
	q.Set("client_id", c.ClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", strings.Join(LoginScopes, " "))
	q.Set("code_challenge", p.Challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", p.State)
	if loginHint != "" {
		q.Set("login_hint", loginHint)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// tokenResponse is the shape of both the authorization_code and refresh_token
// grant responses.
type tokenResponse struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	ExpiresIn             int64  `json:"expires_in"`
	RefreshTokenExpiresIn *int64 `json:"refresh_token_expires_in"`
	Scope                 string `json:"scope"`
	Account               *struct {
		UUID         string `json:"uuid"`
		EmailAddress string `json:"email_address"`
	} `json:"account"`
	Organization *struct {
		UUID string `json:"uuid"`
	} `json:"organization"`
}

// TokenResult is a grant response translated into cas terms.
type TokenResult struct {
	Cred        *Cred
	Email       string
	AccountUUID string
	OrgUUID     string
}

func (r tokenResponse) toResult(prev *Cred, fallbackRefresh string) *TokenResult {
	now := time.Now()
	c := &Cred{
		AccessToken:  r.AccessToken,
		RefreshToken: firstNonEmpty(r.RefreshToken, fallbackRefresh),
		ExpiresAt:    now.Add(time.Duration(r.ExpiresIn) * time.Second).UnixMilli(),
		Scopes:       splitScopes(r.Scope),
	}
	if r.RefreshTokenExpiresIn != nil {
		c.RefreshTokenExpiresAt = now.Add(time.Duration(*r.RefreshTokenExpiresIn) * time.Second).UnixMilli()
	} else if r.RefreshToken != "" {
		// The server rotated the refresh token but did not state a lifetime.
		// Assume Claude Code's own 30-day default rather than carrying a stale one.
		c.RefreshTokenExpiresAt = now.Add(defaultRefreshWindow).UnixMilli()
	} else if prev != nil {
		c.RefreshTokenExpiresAt = prev.RefreshTokenExpiresAt
	}
	if len(c.Scopes) == 0 && prev != nil {
		c.Scopes = prev.Scopes
	}
	if prev != nil {
		c.SubscriptionType = prev.SubscriptionType
		c.RateLimitTier = prev.RateLimitTier
		c.extra = prev.extra
	}

	res := &TokenResult{Cred: c}
	if r.Account != nil {
		res.Email = r.Account.EmailAddress
		res.AccountUUID = r.Account.UUID
	}
	if r.Organization != nil {
		res.OrgUUID = r.Organization.UUID
	}
	return res
}

// Exchange trades an authorization code for a credential.
func (c Config) Exchange(ctx context.Context, code string, p *PKCE, redirectURI string) (*TokenResult, error) {
	// claude.ai hands back "code#state"; the code is the part before the hash.
	state := p.State
	if i := strings.IndexByte(code, '#'); i >= 0 {
		if got := code[i+1:]; got != "" && got != p.State {
			return nil, fmt.Errorf("state mismatch: the browser response does not belong to this login attempt")
		}
		code = code[:i]
	}
	body := map[string]any{
		"grant_type":    "authorization_code",
		"code":          strings.TrimSpace(code),
		"redirect_uri":  redirectURI,
		"client_id":     c.ClientID,
		"code_verifier": p.Verifier,
		"state":         state,
	}
	var out tokenResponse
	if err := c.postJSON(ctx, c.TokenURL, body, &out); err != nil {
		return nil, err
	}
	if out.AccessToken == "" {
		return nil, errors.New("token exchange returned no access token")
	}
	return out.toResult(nil, ""), nil
}

// Refresh exchanges a refresh token for a fresh credential. prev supplies the
// fields the grant response does not carry (plan, tier, unknown extras).
func (c Config) Refresh(ctx context.Context, prev *Cred) (*TokenResult, error) {
	if prev == nil || prev.RefreshToken == "" {
		return nil, errors.New("no refresh token available")
	}
	body := map[string]any{
		"grant_type":    "refresh_token",
		"refresh_token": prev.RefreshToken,
		"client_id":     c.ClientID,
		"scope":         strings.Join(RefreshScopes, " "),
	}
	var out tokenResponse
	if err := c.postJSON(ctx, c.TokenURL, body, &out); err != nil {
		return nil, err
	}
	if out.AccessToken == "" {
		return nil, errors.New("token refresh returned no access token")
	}
	return out.toResult(prev, prev.RefreshToken), nil
}

// Profile is the identity behind an access token.
type Profile struct {
	Account struct {
		UUID         string `json:"uuid"`
		FullName     string `json:"full_name"`
		DisplayName  string `json:"display_name"`
		Email        string `json:"email"`
		HasClaudeMax bool   `json:"has_claude_max"`
		HasClaudePro bool   `json:"has_claude_pro"`
		CreatedAt    string `json:"created_at"`
	} `json:"account"`
	Organization struct {
		UUID                  string          `json:"uuid"`
		Name                  string          `json:"name"`
		OrganizationType      string          `json:"organization_type"`
		BillingType           string          `json:"billing_type"`
		RateLimitTier         string          `json:"rate_limit_tier"`
		HasExtraUsageEnabled  bool            `json:"has_extra_usage_enabled"`
		SubscriptionStatus    string          `json:"subscription_status"`
		SubscriptionCreatedAt string          `json:"subscription_created_at"`
		CCOnboardingFlags     json.RawMessage `json:"cc_onboarding_flags"`
		ClaudeCodeTrialEndsAt json.RawMessage `json:"claude_code_trial_ends_at"`
	} `json:"organization"`
}

// SubscriptionType maps the profile onto the "max"/"pro" value Claude Code
// stores alongside the credential.
func (p *Profile) SubscriptionType() string {
	switch {
	case p.Account.HasClaudeMax:
		return "max"
	case p.Account.HasClaudePro:
		return "pro"
	}
	switch p.Organization.OrganizationType {
	case "claude_max":
		return "max"
	case "claude_pro":
		return "pro"
	}
	return p.Organization.OrganizationType
}

// FetchProfile resolves the account behind an access token.
func (c Config) FetchProfile(ctx context.Context, accessToken string) (*Profile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.APIBase+"/api/oauth/profile", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := c.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch profile: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch profile: HTTP %d: %s", resp.StatusCode, snippet(raw))
	}
	var p Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("parse profile: %w", err)
	}
	return &p, nil
}

// Validation is what the API reports about an access token.
type Validation struct {
	Valid            bool     `json:"valid"`
	AccountUUID      string   `json:"account_uuid"`
	OrganizationUUID string   `json:"organization_uuid"`
	Scopes           []string `json:"scopes"`
}

// Validate asks the API whether an access token is still live, and whose it is.
// Unlike a refresh it does not rotate anything, so it is safe to call on every
// slot and cheap enough to use as an identity check.
func (c Config) Validate(ctx context.Context, accessToken string) (*Validation, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.APIBase+"/api/oauth/validate", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("validate token: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &Validation{Valid: false}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("validate token: HTTP %d: %s", resp.StatusCode, snippet(raw))
	}
	var out Validation
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse validate response: %w", err)
	}
	return &out, nil
}

func (c Config) postJSON(ctx context.Context, endpoint string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK {
		if isInvalidGrant(resp.StatusCode, raw) {
			return ErrInvalidGrant
		}
		return fmt.Errorf("POST %s: HTTP %d: %s", endpoint, resp.StatusCode, snippet(raw))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("parse response from %s: %w", endpoint, err)
	}
	return nil
}

func isInvalidGrant(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusUnauthorized {
		return false
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err == nil && e.Error == "invalid_grant" {
		return true
	}
	return status == http.StatusUnauthorized || bytes.Contains(body, []byte("invalid_grant"))
}

func splitScopes(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Fields(s)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		return "(empty response)"
	}
	return s
}
