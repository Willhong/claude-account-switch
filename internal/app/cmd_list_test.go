package app

import (
	"strings"
	"testing"

	"github.com/Willhong/claude-account-switch/internal/store"
)

func TestRefreshCellDistinguishesPresentTokenWithUnknownExpiry(t *testing.T) {
	got := refreshCell(&store.Slot{}, true)
	if !strings.Contains(got, "present (expiry unknown)") {
		t.Fatalf("refresh cell = %q, want token presence with unknown expiry", got)
	}
}

func TestRefreshCellReportsUnknownWhenNoTokenIsPresent(t *testing.T) {
	got := refreshCell(&store.Slot{}, false)
	if strings.Contains(got, "present") {
		t.Fatalf("refresh cell = %q, must not claim a token is present", got)
	}
}
