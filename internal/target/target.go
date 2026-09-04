// Package target reads and writes the credential slot Claude Code itself uses:
// the "Claude Code-credentials" keychain item, the optional
// .credentials.json fallback file, and the oauthAccount block in ~/.claude.json.
package target

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/Willhong/claude-account-switch/internal/claudeauth"
	"github.com/Willhong/claude-account-switch/internal/jsonpatch"
	"github.com/Willhong/claude-account-switch/internal/keychain"
)

// DefaultService is the keychain service Claude Code writes on macOS. A
// non-default CLAUDE_CONFIG_DIR makes Claude Code append a hash suffix; set
// CAS_KEYCHAIN_SERVICE to point cas at that item instead.
const DefaultService = "Claude Code-credentials"

// ErrNoCredential means Claude Code currently has no stored login.
var ErrNoCredential = errors.New("no Claude Code credential is currently stored")

// Target describes where Claude Code keeps its state.
type Target struct {
	Service    string
	Account    string
	ConfigDir  string
	ConfigPath string
	CredsPath  string
}

// Detect resolves the target from the environment.
func Detect() (*Target, error) {
	service := strings.TrimSpace(os.Getenv("CAS_KEYCHAIN_SERVICE"))
	if service == "" {
		service = DefaultService
	}

	account := strings.TrimSpace(os.Getenv("CAS_KEYCHAIN_ACCOUNT"))
	if account == "" {
		u, err := user.Current()
		if err != nil {
			return nil, fmt.Errorf("determine current user: %w", err)
		}
		account = u.Username
	}

	configDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locate home directory: %w", err)
		}
		configDir = home
		return &Target{
			Service:    service,
			Account:    account,
			ConfigDir:  configDir,
			ConfigPath: filepath.Join(home, ".claude.json"),
			CredsPath:  filepath.Join(home, ".claude", ".credentials.json"),
		}, nil
	}
	return &Target{
		Service:    service,
		Account:    account,
		ConfigDir:  configDir,
		ConfigPath: filepath.Join(configDir, ".claude.json"),
		CredsPath:  filepath.Join(configDir, ".credentials.json"),
	}, nil
}

// ProjectsDir is where Claude Code keeps its session transcripts, one
// directory per project. It sits beside the credentials file in both the
// default layout and a CLAUDE_CONFIG_DIR one.
func (t *Target) ProjectsDir() string {
	return filepath.Join(filepath.Dir(t.CredsPath), "projects")
}

// CredentialCandidate is one place where Claude Code may have persisted a
// credential. Keychain and the fallback file can diverge when an already
// running session refreshes through a different storage backend.
type CredentialCandidate struct {
	Env    *claudeauth.Envelope
	Source string
}

// ReadCred loads the credential Claude Code is currently using. Keychain is
// preferred, but an empty or malformed Keychain value must not hide a valid
// fallback file.
func (t *Target) ReadCred() (*claudeauth.Envelope, error) {
	raw, keyErr := keychain.Get(t.Service, t.Account)
	if keyErr == nil {
		env, parseErr := claudeauth.ParseEnvelope(raw)
		if parseErr == nil {
			return env, nil
		}
		if env, fileErr := readCredFile(t.CredsPath); fileErr == nil {
			if repairErr := t.writeKeychainCred(env); repairErr != nil {
				return nil, fmt.Errorf("repair the live Keychain credential from %s: %w", t.CredsPath, repairErr)
			}
			return env, nil
		}
		return nil, parseErr
	}
	if !errors.Is(keyErr, keychain.ErrNotFound) {
		return nil, keyErr
	}
	if env, fileErr := readCredFile(t.CredsPath); fileErr == nil {
		if repairErr := t.writeKeychainCred(env); repairErr != nil {
			return nil, fmt.Errorf("restore the live Keychain credential from %s: %w", t.CredsPath, repairErr)
		}
		return env, nil
	}
	return nil, ErrNoCredential
}

// ReadCredCandidates reads Keychain and the fallback file independently. It is
// used after invalid_grant to find a newer credential written by another live
// Claude Code session. Invalid and duplicate candidates are omitted.
func (t *Target) ReadCredCandidates() []CredentialCandidate {
	var out []CredentialCandidate
	seen := map[string]bool{}
	add := func(env *claudeauth.Envelope, source string) {
		if env == nil || env.OAuth == nil || seen[env.OAuth.AccessToken] {
			return
		}
		seen[env.OAuth.AccessToken] = true
		out = append(out, CredentialCandidate{Env: env, Source: source})
	}

	if raw, err := keychain.Get(t.Service, t.Account); err == nil {
		if env, err := claudeauth.ParseEnvelope(raw); err == nil {
			add(env, "Keychain")
		}
	}
	if env, err := readCredFile(t.CredsPath); err == nil {
		add(env, t.CredsPath)
	}
	return out
}

func readCredFile(path string) (*claudeauth.Envelope, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return claudeauth.ParseEnvelope(string(b))
}

// WriteCred makes env the credential Claude Code will use. The keychain is
// authoritative; the fallback file is only updated when it already exists, so
// cas never introduces a plaintext copy that was not already there.
func (t *Target) WriteCred(env *claudeauth.Envelope) error {
	if err := t.writeKeychainCred(env); err != nil {
		return err
	}
	blob, err := env.Encode()
	if err != nil {
		return err
	}
	if _, err := os.Stat(t.CredsPath); err == nil {
		if err := writeFileAtomic(t.CredsPath, []byte(blob), 0o600); err != nil {
			return fmt.Errorf("update %s: %w", t.CredsPath, err)
		}
	}
	return nil
}

func (t *Target) writeKeychainCred(env *claudeauth.Envelope) error {
	blob, err := env.Encode()
	if err != nil {
		return err
	}
	return keychain.Set(t.Service, t.Account, t.Service, blob)
}

// ReadOAuthAccount returns the oauthAccount block of ~/.claude.json, or nil if
// the file or the key is absent.
func (t *Target) ReadOAuthAccount() (json.RawMessage, error) {
	b, err := os.ReadFile(t.ConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", t.ConfigPath, err)
	}
	var cfg struct {
		OAuthAccount json.RawMessage `json:"oauthAccount"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", t.ConfigPath, err)
	}
	return cfg.OAuthAccount, nil
}

// WriteOAuthAccount replaces the oauthAccount block of ~/.claude.json, leaving
// every other byte of the file untouched. A timestamped backup is taken the
// first time cas rewrites the file.
func (t *Target) WriteOAuthAccount(account json.RawMessage, backupDir string) error {
	if len(account) == 0 {
		return nil
	}
	b, err := os.ReadFile(t.ConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s does not exist; run `claude` once before switching accounts", t.ConfigPath)
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", t.ConfigPath, err)
	}

	// Re-indent the block so the rewritten file still reads cleanly.
	var pretty any
	if err := json.Unmarshal(account, &pretty); err != nil {
		return fmt.Errorf("parse oauthAccount payload: %w", err)
	}
	value, err := jsonpatch.MarshalIndentNoEscape(pretty, "  ")
	if err != nil {
		return err
	}

	patched, err := jsonpatch.SetTopLevelKey(b, "oauthAccount", value)
	if err != nil {
		return fmt.Errorf("patch %s: %w", t.ConfigPath, err)
	}
	if !json.Valid(patched) {
		return fmt.Errorf("refusing to write %s: the patched document is not valid JSON", t.ConfigPath)
	}

	if backupDir != "" {
		if err := backupOnce(t.ConfigPath, backupDir); err != nil {
			return err
		}
	}
	info, err := os.Stat(t.ConfigPath)
	mode := os.FileMode(0o600)
	if err == nil {
		mode = info.Mode().Perm()
	}
	return writeFileAtomic(t.ConfigPath, patched, mode)
}

// backupOnce copies src into dir the first time cas touches it.
func backupOnce(src, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	marker := filepath.Join(dir, filepath.Base(src)+".original")
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("back up %s: %w", src, err)
	}
	if err := writeFileAtomic(marker, b, 0o600); err != nil {
		return fmt.Errorf("back up %s: %w", src, err)
	}
	return nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)

	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// BuildOAuthAccount renders the oauthAccount block Claude Code expects from a
// freshly fetched profile. roles may be nil.
func BuildOAuthAccount(p *claudeauth.Profile, roles *claudeauth.Roles) json.RawMessage {
	acct := map[string]any{
		"accountUuid":               p.Account.UUID,
		"emailAddress":              p.Account.Email,
		"organizationUuid":          p.Organization.UUID,
		"hasExtraUsageEnabled":      p.Organization.HasExtraUsageEnabled,
		"billingType":               p.Organization.BillingType,
		"accountCreatedAt":          p.Account.CreatedAt,
		"subscriptionCreatedAt":     p.Organization.SubscriptionCreatedAt,
		"ccOnboardingFlags":         rawOrDefault(p.Organization.CCOnboardingFlags, "{}"),
		"claudeCodeTrialEndsAt":     rawOrDefault(p.Organization.ClaudeCodeTrialEndsAt, "null"),
		"displayName":               p.Account.DisplayName,
		"fullName":                  p.Account.FullName,
		"organizationName":          p.Organization.Name,
		"organizationType":          p.Organization.OrganizationType,
		"organizationRateLimitTier": p.Organization.RateLimitTier,
		"profileFetchedAt":          time.Now().UnixMilli(),
	}
	if roles != nil {
		acct["organizationRole"] = roles.OrganizationRole
		acct["workspaceRole"] = roles.WorkspaceRole
	}
	b, err := json.Marshal(acct)
	if err != nil {
		return nil
	}
	return b
}

func rawOrDefault(raw json.RawMessage, fallback string) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(fallback)
	}
	return raw
}

// AccountEmail pulls the email out of an oauthAccount block.
func AccountEmail(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var a struct {
		EmailAddress string `json:"emailAddress"`
	}
	_ = json.Unmarshal(raw, &a)
	return a.EmailAddress
}

// AccountUUID pulls the account uuid out of an oauthAccount block.
func AccountUUID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var a struct {
		AccountUUID string `json:"accountUuid"`
	}
	_ = json.Unmarshal(raw, &a)
	return a.AccountUUID
}
