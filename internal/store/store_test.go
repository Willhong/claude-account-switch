package store

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Willhong/claude-account-switch/internal/claudeauth"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("CAS_HOME", t.TempDir())
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLoadOnAFreshHomeReturnsAnEmptyTable(t *testing.T) {
	st, err := newStore(t).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Slots) != 0 || st.ActiveSlot != 0 {
		t.Errorf("expected an empty table, got %+v", st)
	}
}

func TestSaveIsAtomicAndPrivate(t *testing.T) {
	s := newStore(t)
	st := &State{}
	st.Add(&Slot{Email: "a@example.com"})
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(s.Path("state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}

	entries, err := os.ReadDir(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if len(e.Name()) > 4 && e.Name()[len(e.Name())-4:] == ".tmp" {
			t.Errorf("temp file %s was left behind", e.Name())
		}
	}
}

func TestSlotNumbersFillTheLowestGap(t *testing.T) {
	st := &State{}
	st.Add(&Slot{Email: "a@example.com"})
	st.Add(&Slot{Email: "b@example.com"})
	st.Add(&Slot{Email: "c@example.com"})

	if !st.Remove(2) {
		t.Fatal("Remove(2) reported nothing removed")
	}
	got := st.Add(&Slot{Email: "d@example.com"})
	if got.N != 2 {
		t.Errorf("new slot number = %d, want the freed 2", got.N)
	}
	if st.Slots[1].Email != "d@example.com" {
		t.Errorf("slots are out of order: %v", st.Slots[1])
	}
}

func TestRemovingTheActiveSlotClearsTheActiveMarker(t *testing.T) {
	st := &State{}
	s := st.Add(&Slot{Email: "a@example.com"})
	st.ActiveSlot = s.N

	st.Remove(s.N)
	if st.ActiveSlot != 0 {
		t.Errorf("ActiveSlot = %d, want 0", st.ActiveSlot)
	}
	if st.Active() != nil {
		t.Error("Active() must be nil once the slot is gone")
	}
}

func TestActiveIsNilWhenTheSlotIsMissing(t *testing.T) {
	st := &State{ActiveSlot: 7}
	if st.Active() != nil {
		t.Error("a dangling ActiveSlot must not resolve")
	}
}

func TestRoundTripKeepsTheOAuthAccountBlock(t *testing.T) {
	s := newStore(t)
	st := &State{}
	slot := st.Add(&Slot{
		Email:        "a@example.com",
		OAuthAccount: json.RawMessage(`{"emailAddress":"a@example.com","ccOnboardingFlags":{}}`),
		AddedAt:      time.Now(),
	})
	st.ActiveSlot = slot.N

	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	got := loaded.Active()
	if got == nil {
		t.Fatal("active slot did not survive the round trip")
	}
	if AccountEmailOf(t, got.OAuthAccount) != "a@example.com" {
		t.Errorf("oauthAccount was mangled: %s", got.OAuthAccount)
	}
}

func TestApplyCredPreservesKnownRefreshExpiryWhenClaudeOmitsIt(t *testing.T) {
	const knownExpiry = int64(1790895899971)
	slot := &Slot{RefreshTokenExpiresAt: knownExpiry}

	slot.ApplyCred(&claudeauth.Cred{ExpiresAt: 123, RefreshToken: "rotated"})

	if slot.RefreshTokenExpiresAt != knownExpiry {
		t.Fatalf("refresh expiry = %d, want preserved value %d", slot.RefreshTokenExpiresAt, knownExpiry)
	}
}

func TestApplyCredReplacesKnownRefreshExpiryWhenProvided(t *testing.T) {
	slot := &Slot{RefreshTokenExpiresAt: 100}

	slot.ApplyCred(&claudeauth.Cred{RefreshTokenExpiresAt: 200})

	if slot.RefreshTokenExpiresAt != 200 {
		t.Fatalf("refresh expiry = %d, want 200", slot.RefreshTokenExpiresAt)
	}
}

func TestLoadRejectsANewerStateVersion(t *testing.T) {
	s := newStore(t)
	if err := os.WriteFile(s.Path("state.json"), []byte(`{"version":99,"slots":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil {
		t.Error("expected a refusal to read a newer schema")
	}
}

func TestLockIsReleasedByTheReturnedFunc(t *testing.T) {
	s := newStore(t)
	unlock, err := s.Lock()
	if err != nil {
		t.Fatal(err)
	}
	unlock()

	done := make(chan struct{})
	go func() {
		u2, err := s.Lock()
		if err == nil {
			u2()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the lock was not released")
	}
}

// AccountEmailOf keeps the store test free of a dependency on internal/target.
func AccountEmailOf(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var a struct {
		EmailAddress string `json:"emailAddress"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	return a.EmailAddress
}
