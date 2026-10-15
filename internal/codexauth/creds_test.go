package codexauth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseChatGPTCredentialExtractsStableMetadataAndPreservesBytes(t *testing.T) {
	idToken := testJWT(t, map[string]any{
		"email": "person@example.com",
		"name":  "Person",
		"sub":   "user-1",
		"exp":   int64(2_000_000_000),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "account-1",
			"chatgpt_plan_type":  "pro",
		},
	})
	accessToken := testJWT(t, map[string]any{"exp": int64(2_000_000_100)})
	raw := []byte(`{
  "auth_mode": "chatgpt",
  "tokens": {"id_token":"` + idToken + `","access_token":"` + accessToken + `","refresh_token":"refresh","account_id":"account-1"},
  "last_refresh": "2026-09-21T01:02:03.123456Z",
  "future_field": {"keep": true}
}`)

	doc, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Email != "person@example.com" || doc.AccountID != "account-1" || doc.Plan != "pro" {
		t.Fatalf("unexpected metadata: %+v", doc)
	}
	if doc.Identity() != "account:account-1" {
		t.Fatalf("identity = %q", doc.Identity())
	}
	if !doc.ExpiresAt.Equal(time.Unix(2_000_000_100, 0)) {
		t.Fatalf("expiry = %v", doc.ExpiresAt)
	}
	if string(doc.Encode()) != string(raw) {
		t.Fatal("credential bytes changed during the round trip")
	}
}

func TestAPIKeyIdentityDoesNotExposeTheKey(t *testing.T) {
	raw := []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"sk-secret-value"}`)
	doc, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(doc.Identity(), "sk-secret") || strings.Contains(doc.DisplayAccount(), "sk-secret") {
		t.Fatal("API key leaked through display metadata")
	}
	if !strings.HasPrefix(doc.Identity(), "api:") {
		t.Fatalf("identity = %q", doc.Identity())
	}
}

func TestParseRejectsDocumentWithoutCredential(t *testing.T) {
	if _, err := Parse([]byte(`{"auth_mode":"chatgpt"}`)); err == nil {
		t.Fatal("expected an invalid credential error")
	}
	if _, err := Parse([]byte(`{"auth_mode":"chatgpt","personal_access_token":null}`)); err == nil {
		t.Fatal("null personal_access_token must not count as a credential")
	}
}

func TestAccessExpiredSkipsAPIKeys(t *testing.T) {
	doc, err := Parse([]byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"sk-test"}`))
	if err != nil {
		t.Fatal(err)
	}
	if doc.AccessExpired(time.Hour) {
		t.Fatal("API key credentials do not have an OAuth access expiry")
	}
}

func testJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	b, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(b) + ".signature"
}
