package codextarget

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Willhong/claude-account-switch/internal/codexauth"
)

func TestDetectHonoursCodexHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	tgt, err := Detect()
	if err != nil {
		t.Fatal(err)
	}
	if tgt.AuthPath != filepath.Join(home, "auth.json") {
		t.Fatalf("AuthPath = %q", tgt.AuthPath)
	}
}

func TestWriteIsPrivateAndRoundTripsUnknownFields(t *testing.T) {
	home := t.TempDir()
	tgt := &Target{Home: home, AuthPath: filepath.Join(home, "auth.json")}
	raw := []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"sk-test","future":true}`)
	doc, err := codexauth.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := tgt.Write(doc); err != nil {
		t.Fatal(err)
	}
	got, err := tgt.Read()
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Encode()) != string(raw) {
		t.Fatalf("round trip changed bytes: %s", got.Encode())
	}
	info, err := os.Stat(tgt.AuthPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestWriteTightensAnExistingAuthFilePermission(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "auth.json")
	if err := os.WriteFile(path, []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"old"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := codexauth.Parse([]byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"new"}`))
	if err != nil {
		t.Fatal(err)
	}
	tgt := &Target{Home: home, AuthPath: path}
	if err := tgt.Write(doc); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestReadMissingCredential(t *testing.T) {
	home := t.TempDir()
	tgt := &Target{Home: home, AuthPath: filepath.Join(home, "auth.json")}
	if _, err := tgt.Read(); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("error = %v, want ErrNoCredential", err)
	}
}
