// Package codextarget reads and writes the credential file used by Codex CLI.
package codextarget

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Willhong/claude-account-switch/internal/codexauth"
)

// ErrNoCredential means Codex has no file-backed login to adopt or switch.
var ErrNoCredential = errors.New("Codex has no file-backed credential")

// Target describes the active Codex credential location.
type Target struct {
	Home     string
	AuthPath string
}

// Detect resolves CODEX_HOME, defaulting to ~/.codex like Codex CLI.
func Detect() (*Target, error) {
	home := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locate home directory: %w", err)
		}
		home = filepath.Join(userHome, ".codex")
	}
	if !filepath.IsAbs(home) {
		abs, err := filepath.Abs(home)
		if err != nil {
			return nil, fmt.Errorf("resolve CODEX_HOME: %w", err)
		}
		home = abs
	}
	return &Target{Home: home, AuthPath: filepath.Join(home, "auth.json")}, nil
}

// Read loads the active file-backed Codex credential.
func (t *Target) Read() (*codexauth.Document, error) {
	raw, err := os.ReadFile(t.AuthPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoCredential
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", t.AuthPath, err)
	}
	doc, err := codexauth.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", t.AuthPath, err)
	}
	return doc, nil
}

// Write atomically replaces auth.json with a private copy of doc.
func (t *Target) Write(doc *codexauth.Document) error {
	if doc == nil {
		return errors.New("cannot write an empty Codex credential")
	}
	if err := os.MkdirAll(t.Home, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", t.Home, err)
	}

	tmp, err := os.CreateTemp(t.Home, ".auth-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary Codex credential: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(doc.Encode()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, t.AuthPath); err != nil {
		return fmt.Errorf("replace %s: %w", t.AuthPath, err)
	}
	return nil
}
