package app

import (
	"testing"

	"github.com/Willhong/claude-account-switch/internal/codexappserver"
	"github.com/Willhong/claude-account-switch/internal/codexstore"
)

func TestCodexAccountMatchesChatGPTEmail(t *testing.T) {
	slot := &codexstore.Slot{AuthMode: "chatgpt", Email: "person@example.com"}
	if !codexAccountMatches(slot, codexappserver.Account{Type: "chatgpt", Email: "PERSON@example.com"}) {
		t.Fatal("same ChatGPT email should match")
	}
	if codexAccountMatches(slot, codexappserver.Account{Type: "chatgpt", Email: "other@example.com"}) {
		t.Fatal("another ChatGPT email must not match")
	}
}

func TestCodexAccountMatchesAPIKeyMode(t *testing.T) {
	slot := &codexstore.Slot{AuthMode: "apikey"}
	if !codexAccountMatches(slot, codexappserver.Account{Type: "apiKey"}) {
		t.Fatal("API key account should match API key slot")
	}
}

func TestCodexWindowLabelsMatchCLIStatusWindows(t *testing.T) {
	fiveHours := int64(300)
	week := int64(10080)
	bucket := codexappserver.RateLimitSnapshot{LimitID: "codex"}
	if got := codexWindowLabel("codex", bucket, &codexappserver.RateLimitWindow{WindowDurationMins: &fiveHours}, "PRIMARY"); got != "5H" {
		t.Fatalf("five-hour label = %q", got)
	}
	if got := codexWindowLabel("codex", bucket, &codexappserver.RateLimitWindow{WindowDurationMins: &week}, "SECONDARY"); got != "WK" {
		t.Fatalf("weekly label = %q", got)
	}
}

func TestDefiniteCodexAuthFailureDoesNotTreatNetworkErrorsAsDead(t *testing.T) {
	if !definiteCodexAuthFailure(assertiveError("refresh token was revoked")) {
		t.Fatal("revoked refresh token should be definitely dead")
	}
	if definiteCodexAuthFailure(assertiveError("connection reset by peer")) {
		t.Fatal("a network error must not remove a Codex slot")
	}
}

type assertiveError string

func (e assertiveError) Error() string { return string(e) }
