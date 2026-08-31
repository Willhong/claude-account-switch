package claudeauth

import (
	"encoding/json"
	"fmt"
	"time"
)

// Cred mirrors the object Claude Code stores under the "claudeAiOauth" key of
// its credential blob. Fields it does not know about are preserved verbatim so
// a round-trip through cas never drops data a newer Claude Code added.
type Cred struct {
	AccessToken           string   `json:"accessToken"`
	RefreshToken          string   `json:"refreshToken"`
	ExpiresAt             int64    `json:"expiresAt"`                       // unix millis
	RefreshTokenExpiresAt int64    `json:"refreshTokenExpiresAt,omitempty"` // unix millis
	Scopes                []string `json:"scopes,omitempty"`
	SubscriptionType      string   `json:"subscriptionType,omitempty"`
	RateLimitTier         string   `json:"rateLimitTier,omitempty"`

	extra map[string]json.RawMessage
}

type credAlias Cred

var knownCredKeys = map[string]bool{
	"accessToken": true, "refreshToken": true, "expiresAt": true,
	"refreshTokenExpiresAt": true, "scopes": true,
	"subscriptionType": true, "rateLimitTier": true,
}

func (c *Cred) UnmarshalJSON(b []byte) error {
	var alias credAlias
	if err := json.Unmarshal(b, &alias); err != nil {
		return err
	}
	*c = Cred(alias)

	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	for k, v := range all {
		if knownCredKeys[k] {
			continue
		}
		if c.extra == nil {
			c.extra = map[string]json.RawMessage{}
		}
		c.extra[k] = v
	}
	return nil
}

func (c Cred) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(credAlias(c))
	if err != nil {
		return nil, err
	}
	if len(c.extra) == 0 {
		return b, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(b, &merged); err != nil {
		return nil, err
	}
	for k, v := range c.extra {
		if _, clash := merged[k]; !clash {
			merged[k] = v
		}
	}
	return json.Marshal(merged)
}

// ExpiresAtTime returns the access-token expiry, or the zero time if unset.
func (c Cred) ExpiresAtTime() time.Time { return millisToTime(c.ExpiresAt) }

// RefreshExpiresAtTime returns the refresh-token expiry, or the zero time if unset.
func (c Cred) RefreshExpiresAtTime() time.Time { return millisToTime(c.RefreshTokenExpiresAt) }

// AccessExpired reports whether the access token is expired, treating anything
// within skew of expiry as already expired.
func (c Cred) AccessExpired(skew time.Duration) bool {
	if c.ExpiresAt == 0 {
		return true
	}
	return time.Now().Add(skew).After(c.ExpiresAtTime())
}

// RefreshExpired reports whether the refresh token is past its expiry. An
// unknown expiry is treated as still valid — the server is the real authority.
func (c Cred) RefreshExpired() bool {
	if c.RefreshTokenExpiresAt == 0 {
		return false
	}
	return time.Now().After(c.RefreshExpiresAtTime())
}

func millisToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// Envelope is the top-level credential document: {"claudeAiOauth": {...}}.
// Sibling keys (other credential kinds Claude Code may store) are preserved.
type Envelope struct {
	OAuth *Cred
	extra map[string]json.RawMessage
}

const oauthKey = "claudeAiOauth"

func (e *Envelope) UnmarshalJSON(b []byte) error {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	e.extra = map[string]json.RawMessage{}
	for k, v := range all {
		if k == oauthKey {
			var c Cred
			if err := json.Unmarshal(v, &c); err != nil {
				return fmt.Errorf("parse %s: %w", oauthKey, err)
			}
			e.OAuth = &c
			continue
		}
		e.extra[k] = v
	}
	return nil
}

func (e Envelope) MarshalJSON() ([]byte, error) {
	out := map[string]json.RawMessage{}
	for k, v := range e.extra {
		out[k] = v
	}
	if e.OAuth != nil {
		b, err := json.Marshal(e.OAuth)
		if err != nil {
			return nil, err
		}
		out[oauthKey] = b
	}
	return json.Marshal(out)
}

// NewEnvelope wraps a credential in a fresh envelope.
func NewEnvelope(c *Cred) *Envelope {
	return &Envelope{OAuth: c, extra: map[string]json.RawMessage{}}
}

// ParseEnvelope decodes a stored credential blob.
func ParseEnvelope(s string) (*Envelope, error) {
	var env Envelope
	if err := json.Unmarshal([]byte(s), &env); err != nil {
		return nil, fmt.Errorf("parse credential blob: %w", err)
	}
	if env.OAuth == nil || env.OAuth.AccessToken == "" {
		return nil, fmt.Errorf("credential blob has no %s access token", oauthKey)
	}
	return &env, nil
}

// Encode renders the envelope back into the on-disk/keychain form.
func (e *Envelope) Encode() (string, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Clone returns a deep copy, so mutating one slot's credential can never alias
// another's.
func (e *Envelope) Clone() *Envelope {
	b, err := json.Marshal(e)
	if err != nil {
		return e
	}
	var out Envelope
	if err := json.Unmarshal(b, &out); err != nil {
		return e
	}
	return &out
}
