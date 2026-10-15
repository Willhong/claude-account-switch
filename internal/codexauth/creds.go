// Package codexauth reads the credential document used by Codex CLI.
//
// The document is deliberately kept opaque. Codex adds authentication modes
// and fields over time, so cas only decodes the small amount of metadata it
// needs for account selection and writes the original bytes back unchanged.
package codexauth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrInvalidCredential means a document is valid JSON but does not contain a
// credential form cas can safely identify and switch.
var ErrInvalidCredential = errors.New("invalid Codex credential")

// Document is one complete Codex auth.json payload.
type Document struct {
	raw []byte

	AuthMode string
	APIKey   string
	Tokens   Tokens

	Email       string
	DisplayName string
	AccountID   string
	Subject     string
	Plan        string
	ExpiresAt   time.Time
	LastRefresh time.Time
}

// Tokens is the ChatGPT token bundle currently written by Codex CLI.
type Tokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
}

type authFile struct {
	AuthMode            string          `json:"auth_mode"`
	APIKey              string          `json:"OPENAI_API_KEY"`
	Tokens              Tokens          `json:"tokens"`
	LastRefresh         string          `json:"last_refresh"`
	PersonalAccessToken json.RawMessage `json:"personal_access_token"`
}

type jwtClaims struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	Sub   string `json:"sub"`
	Exp   int64  `json:"exp"`
	Auth  struct {
		ChatGPTAccountID string `json:"chatgpt_account_id"`
		ChatGPTPlanType  string `json:"chatgpt_plan_type"`
		UserID           string `json:"user_id"`
	} `json:"https://api.openai.com/auth"`
}

// Parse validates and inspects a Codex auth.json document.
func Parse(raw []byte) (*Document, error) {
	if !json.Valid(raw) {
		return nil, fmt.Errorf("%w: auth.json is not valid JSON", ErrInvalidCredential)
	}
	var f authFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCredential, err)
	}
	if strings.TrimSpace(f.AuthMode) == "" {
		return nil, fmt.Errorf("%w: auth_mode is missing", ErrInvalidCredential)
	}
	if f.APIKey == "" && f.Tokens.IDToken == "" && f.Tokens.AccessToken == "" &&
		f.Tokens.RefreshToken == "" && !hasJSONValue(f.PersonalAccessToken) {
		return nil, fmt.Errorf("%w: no token or API key is present", ErrInvalidCredential)
	}

	d := &Document{
		raw:      append([]byte(nil), raw...),
		AuthMode: f.AuthMode,
		APIKey:   f.APIKey,
		Tokens:   f.Tokens,
	}
	if t, err := time.Parse(time.RFC3339Nano, f.LastRefresh); err == nil {
		d.LastRefresh = t
	}

	claims, _ := parseJWT(f.Tokens.IDToken)
	d.Email = claims.Email
	d.DisplayName = claims.Name
	d.Subject = claims.Sub
	d.Plan = claims.Auth.ChatGPTPlanType
	d.AccountID = firstNonEmpty(f.Tokens.AccountID, claims.Auth.ChatGPTAccountID, claims.Auth.UserID)

	if accessClaims, err := parseJWT(f.Tokens.AccessToken); err == nil && accessClaims.Exp != 0 {
		d.ExpiresAt = time.Unix(accessClaims.Exp, 0)
	} else if claims.Exp != 0 {
		d.ExpiresAt = time.Unix(claims.Exp, 0)
	}
	return d, nil
}

func hasJSONValue(raw json.RawMessage) bool {
	value := bytes.TrimSpace(raw)
	return len(value) > 0 && !bytes.Equal(value, []byte("null"))
}

func parseJWT(token string) (jwtClaims, error) {
	var claims jwtClaims
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return claims, errors.New("not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims, err
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return claims, err
	}
	return claims, nil
}

// Encode returns a copy of the exact credential document Codex wrote.
func (d *Document) Encode() []byte { return append([]byte(nil), d.raw...) }

// Identity is stable across token refreshes and safe to store as metadata.
func (d *Document) Identity() string {
	switch {
	case d.AccountID != "":
		return "account:" + d.AccountID
	case d.Subject != "":
		return "subject:" + d.Subject
	case d.Email != "":
		return "email:" + strings.ToLower(d.Email)
	case d.APIKey != "":
		return "api:" + shortHash([]byte(d.APIKey))
	default:
		return "credential:" + shortHash(d.raw)
	}
}

// Fingerprint changes whenever Codex rotates or replaces the live credential.
func (d *Document) Fingerprint() string { return shortHash(d.raw) }

// AccessExpired reports whether the access token is expired or close enough to
// expiry that a parked login should be refreshed proactively.
func (d *Document) AccessExpired(skew time.Duration) bool {
	if d.APIKey != "" {
		return false
	}
	if d.ExpiresAt.IsZero() {
		return true
	}
	return time.Now().Add(skew).After(d.ExpiresAt)
}

// DisplayAccount returns a human-readable, non-secret account description.
func (d *Document) DisplayAccount() string {
	switch {
	case d.Email != "":
		return d.Email
	case d.DisplayName != "":
		return d.DisplayName
	case d.APIKey != "":
		return "API key " + shortHash([]byte(d.APIKey))
	case d.AccountID != "":
		return d.AccountID
	default:
		return d.AuthMode + " credential"
	}
}

func shortHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:6])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
