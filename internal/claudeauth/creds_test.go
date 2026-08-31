package claudeauth

import (
	"encoding/json"
	"testing"
	"time"
)

// The exact blob Claude Code stores, plus a field cas does not know about.
const sampleBlob = `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-aaa","refreshToken":"sk-ant-ort01-bbb","expiresAt":1788164959808,"refreshTokenExpiresAt":1790556179808,"scopes":["user:file_upload","user:inference"],"subscriptionType":"max","rateLimitTier":"default_claude_max_20x","somethingNew":{"a":1}},"otherCredentialKind":{"k":"v"}}`

func TestEnvelopeRoundTripPreservesUnknownFields(t *testing.T) {
	env, err := ParseEnvelope(sampleBlob)
	if err != nil {
		t.Fatal(err)
	}
	if env.OAuth.AccessToken != "sk-ant-oat01-aaa" {
		t.Errorf("accessToken = %q", env.OAuth.AccessToken)
	}
	if env.OAuth.SubscriptionType != "max" {
		t.Errorf("subscriptionType = %q", env.OAuth.SubscriptionType)
	}

	out, err := env.Encode()
	if err != nil {
		t.Fatal(err)
	}

	var got, want map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(sampleBlob), &want); err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(got, want) {
		t.Errorf("round trip changed the blob:\n got %s\nwant %s", out, sampleBlob)
	}
}

func TestParseEnvelopeRejectsBlobWithoutAccessToken(t *testing.T) {
	for _, blob := range []string{`{}`, `{"claudeAiOauth":{}}`, `{"claudeAiOauth":{"refreshToken":"x"}}`} {
		if _, err := ParseEnvelope(blob); err == nil {
			t.Errorf("%s: expected an error", blob)
		}
	}
}

func TestCloneIsDeep(t *testing.T) {
	env, err := ParseEnvelope(sampleBlob)
	if err != nil {
		t.Fatal(err)
	}
	clone := env.Clone()
	clone.OAuth.AccessToken = "changed"
	if env.OAuth.AccessToken == "changed" {
		t.Error("Clone aliased the original credential")
	}
}

func TestAccessExpiredHonoursSkew(t *testing.T) {
	c := Cred{ExpiresAt: time.Now().Add(20 * time.Minute).UnixMilli()}
	if c.AccessExpired(0) {
		t.Error("a token with 20 minutes left is not expired")
	}
	if !c.AccessExpired(30 * time.Minute) {
		t.Error("a 30-minute skew should treat 20 minutes of life as expired")
	}
}

func TestRefreshExpiredTreatsUnknownExpiryAsValid(t *testing.T) {
	if (Cred{}).RefreshExpired() {
		t.Error("an unset refresh expiry must not be reported as expired")
	}
	past := Cred{RefreshTokenExpiresAt: time.Now().Add(-time.Hour).UnixMilli()}
	if !past.RefreshExpired() {
		t.Error("a past refresh expiry must be reported as expired")
	}
}

func TestToResultAssumesADefaultLifetimeForRotatedRefreshTokens(t *testing.T) {
	resp := tokenResponse{AccessToken: "new", RefreshToken: "rotated", ExpiresIn: 3600, Scope: "user:inference"}
	prev := &Cred{RefreshToken: "old", RefreshTokenExpiresAt: time.Now().Add(time.Hour).UnixMilli()}

	got := resp.toResult(prev, prev.RefreshToken)
	if got.Cred.RefreshToken != "rotated" {
		t.Errorf("refresh token = %q, want the rotated one", got.Cred.RefreshToken)
	}
	// Carrying the old one-hour expiry forward would make the slot look nearly
	// dead; the 30-day assumption is what Claude Code itself uses.
	if remaining := time.Until(time.UnixMilli(got.Cred.RefreshTokenExpiresAt)); remaining < 29*24*time.Hour {
		t.Errorf("refresh expiry only %s away; the stale one was carried over", remaining)
	}
}

func TestToResultKeepsPreviousRefreshTokenWhenTheServerOmitsIt(t *testing.T) {
	resp := tokenResponse{AccessToken: "new", ExpiresIn: 3600}
	prev := &Cred{RefreshToken: "old", RefreshTokenExpiresAt: 999, SubscriptionType: "max", RateLimitTier: "tier"}

	got := resp.toResult(prev, prev.RefreshToken)
	if got.Cred.RefreshToken != "old" {
		t.Errorf("refresh token = %q, want the previous one", got.Cred.RefreshToken)
	}
	if got.Cred.RefreshTokenExpiresAt != 999 {
		t.Errorf("refresh expiry = %d, want the previous one", got.Cred.RefreshTokenExpiresAt)
	}
	if got.Cred.SubscriptionType != "max" || got.Cred.RateLimitTier != "tier" {
		t.Error("plan metadata was dropped; the grant response does not carry it")
	}
}

func TestProfileSubscriptionType(t *testing.T) {
	var p Profile
	p.Account.HasClaudeMax = true
	if got := p.SubscriptionType(); got != "max" {
		t.Errorf("got %q, want max", got)
	}

	p = Profile{}
	p.Account.HasClaudePro = true
	if got := p.SubscriptionType(); got != "pro" {
		t.Errorf("got %q, want pro", got)
	}

	p = Profile{}
	p.Organization.OrganizationType = "claude_max"
	if got := p.SubscriptionType(); got != "max" {
		t.Errorf("got %q, want max", got)
	}
}

func TestNewPKCEProducesDistinctS256Challenges(t *testing.T) {
	a, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if a.Verifier == b.Verifier || a.State == b.State {
		t.Error("two attempts produced the same verifier or state")
	}
	if a.Challenge == a.Verifier {
		t.Error("challenge must be the hash of the verifier, not the verifier itself")
	}
	if len(a.Challenge) != 43 {
		t.Errorf("challenge length = %d, want 43 (base64url of a SHA-256 digest)", len(a.Challenge))
	}
}

func TestAuthorizeURLCarriesThePKCEParameters(t *testing.T) {
	cfg := NewConfig(func(string) string { return "" })
	p := &PKCE{Verifier: "v", Challenge: "chal", State: "st"}

	got := cfg.AuthorizeURLFor(p, "http://localhost:1234/callback", "me@example.com")
	for _, want := range []string{
		"code_challenge=chal",
		"code_challenge_method=S256",
		"state=st",
		"client_id=" + DefaultClientID,
		"login_hint=me%40example.com",
		"redirect_uri=http%3A%2F%2Flocalhost%3A1234%2Fcallback",
	} {
		if !contains(got, want) {
			t.Errorf("authorize URL is missing %s:\n%s", want, got)
		}
	}
}

func TestExchangeRejectsAMismatchedState(t *testing.T) {
	cfg := NewConfig(func(string) string { return "" })
	p := &PKCE{Verifier: "v", Challenge: "c", State: "expected"}

	if _, err := cfg.Exchange(nil, "somecode#attacker", p, "http://localhost/callback"); err == nil {
		t.Error("expected a state mismatch to be refused before any network call")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}
