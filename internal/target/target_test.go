package target

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realisticConfig mirrors the shape of ~/.claude.json: a large object where
// oauthAccount is one key among many, and most of the bulk is unrelated.
const realisticConfig = `{
  "numStartups": 4239,
  "installMethod": "native",
  "autoUpdates": false,
  "oauthAccount": {
    "accountUuid": "aaaaaaaa-0000-0000-0000-000000000000",
    "emailAddress": "old@example.com",
    "organizationUuid": "bbbbbbbb-0000-0000-0000-000000000000",
    "hasExtraUsageEnabled": false,
    "ccOnboardingFlags": {},
    "claudeCodeTrialEndsAt": null
  },
  "projects": {
    "/Users/me/work": {
      "allowedTools": ["Bash(git status)"],
      "history": [{"display": "hello & goodbye <x>"}]
    }
  },
  "userID": "0000000000000000000000000000000000000000000000000000000000000000"
}`

func newTarget(t *testing.T) (*Target, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(path, []byte(realisticConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Target{ConfigPath: path, CredsPath: filepath.Join(dir, ".credentials.json")}, dir
}

func TestWriteOAuthAccountSwapsTheAccountAndLeavesEverythingElseAlone(t *testing.T) {
	tgt, dir := newTarget(t)
	next := json.RawMessage(`{"accountUuid":"cccccccc-0000-0000-0000-000000000000","emailAddress":"new@example.com","organizationUuid":"dddddddd-0000-0000-0000-000000000000"}`)

	if err := tgt.WriteOAuthAccount(next, filepath.Join(dir, "backups")); err != nil {
		t.Fatal(err)
	}

	out, err := os.ReadFile(tgt.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(out) {
		t.Fatalf("config is no longer valid JSON:\n%s", out)
	}

	got, err := tgt.ReadOAuthAccount()
	if err != nil {
		t.Fatal(err)
	}
	if email := AccountEmail(got); email != "new@example.com" {
		t.Errorf("emailAddress = %q, want new@example.com", email)
	}
	if uuid := AccountUUID(got); uuid != "cccccccc-0000-0000-0000-000000000000" {
		t.Errorf("accountUuid = %q", uuid)
	}

	// Unrelated keys must survive byte-for-byte — this file is 150 KB of user
	// state and reserialising it would churn every line.
	for _, want := range []string{
		`"numStartups": 4239`,
		`"allowedTools": ["Bash(git status)"]`,
		`"history": [{"display": "hello & goodbye <x>"}]`,
		`"userID": "0000000000000000000000000000000000000000000000000000000000000000"`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("untouched content was rewritten; missing %s", want)
		}
	}
	if strings.Contains(string(out), "old@example.com") {
		t.Errorf("the previous account survived:\n%s", out)
	}
}

func TestWriteOAuthAccountBacksUpTheOriginalOnce(t *testing.T) {
	tgt, dir := newTarget(t)
	backups := filepath.Join(dir, "backups")

	first := json.RawMessage(`{"emailAddress":"first@example.com"}`)
	if err := tgt.WriteOAuthAccount(first, backups); err != nil {
		t.Fatal(err)
	}
	second := json.RawMessage(`{"emailAddress":"second@example.com"}`)
	if err := tgt.WriteOAuthAccount(second, backups); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(backups, ".claude.json.original"))
	if err != nil {
		t.Fatalf("no backup was taken: %v", err)
	}
	// The backup must still hold the pre-cas state, not the first rewrite.
	if !strings.Contains(string(b), "old@example.com") {
		t.Error("backup was overwritten by a later switch")
	}
}

func TestWriteOAuthAccountPreservesFilePermissions(t *testing.T) {
	tgt, dir := newTarget(t)
	if err := os.Chmod(tgt.ConfigPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tgt.WriteOAuthAccount(json.RawMessage(`{"emailAddress":"x@y.z"}`), filepath.Join(dir, "backups")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(tgt.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %o, want 644", got)
	}
}

func TestWriteOAuthAccountRefusesAMissingConfig(t *testing.T) {
	dir := t.TempDir()
	tgt := &Target{ConfigPath: filepath.Join(dir, ".claude.json")}
	if err := tgt.WriteOAuthAccount(json.RawMessage(`{}`), ""); err == nil {
		t.Error("expected an error when ~/.claude.json does not exist")
	}
}

func TestWriteOAuthAccountIsANoOpForAnEmptyPayload(t *testing.T) {
	tgt, _ := newTarget(t)
	before, _ := os.ReadFile(tgt.ConfigPath)
	if err := tgt.WriteOAuthAccount(nil, ""); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(tgt.ConfigPath)
	if string(before) != string(after) {
		t.Error("an empty payload must leave the file untouched")
	}
}

func TestReadOAuthAccountOnAMissingFile(t *testing.T) {
	tgt := &Target{ConfigPath: filepath.Join(t.TempDir(), "nope.json")}
	got, err := tgt.ReadOAuthAccount()
	if err != nil {
		t.Fatalf("a missing config is not an error: %v", err)
	}
	if got != nil {
		t.Errorf("got %s, want nil", got)
	}
}

func TestDetectHonoursClaudeConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("CAS_KEYCHAIN_SERVICE", "")
	t.Setenv("CAS_KEYCHAIN_ACCOUNT", "tester")

	tgt, err := Detect()
	if err != nil {
		t.Fatal(err)
	}
	if tgt.ConfigPath != filepath.Join(dir, ".claude.json") {
		t.Errorf("ConfigPath = %s", tgt.ConfigPath)
	}
	if tgt.CredsPath != filepath.Join(dir, ".credentials.json") {
		t.Errorf("CredsPath = %s", tgt.CredsPath)
	}
	if tgt.Service != DefaultService {
		t.Errorf("Service = %s", tgt.Service)
	}
	if tgt.Account != "tester" {
		t.Errorf("Account = %s", tgt.Account)
	}
}
