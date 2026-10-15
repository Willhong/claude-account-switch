package codexstore

import (
	"os"
	"strings"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("CAS_HOME", t.TempDir())
	store, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestStateUsesIndependentSmallestAvailableSlotNumbers(t *testing.T) {
	state := &State{}
	state.Add(&Slot{Account: "one", Identity: "account:1"})
	state.Add(&Slot{Account: "two", Identity: "account:2"})
	state.Remove(1)
	if got := state.Add(&Slot{Account: "three", Identity: "account:3"}); got.N != 1 {
		t.Fatalf("slot = %d, want 1", got.N)
	}
}

func TestStateRoundTripAndPrivatePermissions(t *testing.T) {
	store := newStore(t)
	state := &State{}
	slot := state.Add(&Slot{Account: "person@example.com", Email: "person@example.com", Identity: "account:1"})
	state.ActiveSlot = slot.N
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Active() == nil || loaded.Active().Email != "person@example.com" {
		t.Fatalf("state did not round trip: %+v", loaded)
	}
	info, err := os.Stat(store.Path("codex-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestResolveUsesLabelEmailAndUniqueSubstring(t *testing.T) {
	state := &State{}
	state.Add(&Slot{Label: "work", Account: "one@company.test", Email: "one@company.test", Identity: "account:1"})
	state.Add(&Slot{Label: "home", Account: "two@example.test", Email: "two@example.test", Identity: "account:2"})
	for _, ref := range []string{"1", "work", "one@company.test", "company"} {
		slot, err := state.Resolve(ref)
		if err != nil || slot.N != 1 {
			t.Fatalf("Resolve(%q) = %+v, %v", ref, slot, err)
		}
	}
}

func TestCredentialEncodingRoundTripsLargeDocuments(t *testing.T) {
	raw := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"` + strings.Repeat("token-data-", 500) + `"},"future":true}`)
	encoded, digest, err := encodeCredential(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) == 0 || len(encoded) >= len(raw) {
		t.Fatalf("encoded length = %d, raw length = %d", len(encoded), len(raw))
	}
	got, err := decodeCredential(encoded, digest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(raw) {
		t.Fatal("credential changed after compression and transport encoding")
	}
	if _, err := decodeCredential(encoded, strings.Repeat("0", 64)); err == nil {
		t.Fatal("expected checksum mismatch to be rejected")
	}
}

func TestManifestRejectsNonHexDigest(t *testing.T) {
	if _, ok := parseManifest("chunked-v1:2:" + strings.Repeat("z", 64)); ok {
		t.Fatal("non-hex digest must not be accepted as a credential manifest")
	}
}
