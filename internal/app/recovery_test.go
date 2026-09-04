package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Willhong/claude-account-switch/internal/claudeauth"
	"github.com/Willhong/claude-account-switch/internal/store"
	"github.com/Willhong/claude-account-switch/internal/target"
)

func TestRecoverLiveCredentialAcceptsValidatedCredentialForSameAccount(t *testing.T) {
	const accountUUID = "account-2"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/validate" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"valid":        true,
			"account_uuid": accountUUID,
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	credsPath := filepath.Join(dir, ".credentials.json")
	writeTestCredential(t, credsPath, "new-access", "new-refresh", time.Now().Add(time.Hour))

	a := &App{
		Target: &target.Target{
			Service:   "cas-recovery-test-not-present",
			Account:   "nobody",
			CredsPath: credsPath,
		},
		OAuth: claudeauth.Config{APIBase: server.URL, HTTP: server.Client()},
	}
	slot := &store.Slot{N: 2, AccountUUID: accountUUID}

	recovered := a.recoverLiveCredential(context.Background(), slot, "old-refresh")
	if recovered == nil {
		t.Fatal("expected the live credential to be recovered")
	}
	if recovered.OAuth.RefreshToken != "new-refresh" {
		t.Fatalf("refresh token = %q, want the newer credential", recovered.OAuth.RefreshToken)
	}
}

func TestRecoverLiveCredentialRejectsAnotherAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"valid":        true,
			"account_uuid": "account-1",
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	credsPath := filepath.Join(dir, ".credentials.json")
	writeTestCredential(t, credsPath, "other-access", "other-refresh", time.Now().Add(time.Hour))

	a := &App{
		Target: &target.Target{
			Service:   "cas-recovery-test-not-present",
			Account:   "nobody",
			CredsPath: credsPath,
		},
		OAuth: claudeauth.Config{APIBase: server.URL, HTTP: server.Client()},
	}
	slot := &store.Slot{N: 2, AccountUUID: "account-2"}

	if recovered := a.recoverLiveCredential(context.Background(), slot, "old-refresh"); recovered != nil {
		t.Fatal("must not absorb a credential belonging to another account")
	}
}

func TestRecoverLiveCredentialRejectsTheFailedRefreshToken(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, ".credentials.json")
	writeTestCredential(t, credsPath, "new-access", "rejected-refresh", time.Now().Add(time.Hour))

	a := &App{
		Target: &target.Target{
			Service:   "cas-recovery-test-not-present",
			Account:   "nobody",
			CredsPath: credsPath,
		},
	}

	if recovered := a.recoverLiveCredential(context.Background(), &store.Slot{N: 2}, "rejected-refresh"); recovered != nil {
		t.Fatal("must not retry the credential whose refresh token was rejected")
	}
}

func writeTestCredential(t *testing.T, path, accessToken, refreshToken string, expiresAt time.Time) {
	t.Helper()
	env := claudeauth.NewEnvelope(&claudeauth.Cred{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt.UnixMilli(),
	})
	raw, err := env.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}
