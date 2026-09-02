// Package store persists the cas slot table.
//
// Metadata (slot number, email, plan, expiry timestamps) lives in
// ~/.cas/state.json so `cas list` is a pure local read. The credentials
// themselves never touch that file: each slot's token blob is a separate
// macOS Keychain item.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/Willhong/claude-account-switch/internal/claudeauth"
	"github.com/Willhong/claude-account-switch/internal/keychain"
)

const (
	// SlotService is the keychain service under which cas keeps slot credentials.
	SlotService = "cas-credentials"
	stateFile   = "state.json"
	lockFile    = ".lock"
	// StateVersion is bumped when the on-disk schema changes incompatibly.
	StateVersion = 1
)

// ErrNoSlot is returned when a slot number does not exist.
var ErrNoSlot = errors.New("no such slot")

// Slot is one registered account.
type Slot struct {
	N                     int             `json:"slot"`
	Label                 string          `json:"label,omitempty"`
	Email                 string          `json:"email"`
	AccountUUID           string          `json:"accountUuid,omitempty"`
	OrgUUID               string          `json:"organizationUuid,omitempty"`
	OrgName               string          `json:"organizationName,omitempty"`
	DisplayName           string          `json:"displayName,omitempty"`
	SubscriptionType      string          `json:"subscriptionType,omitempty"`
	RateLimitTier         string          `json:"rateLimitTier,omitempty"`
	Scopes                []string        `json:"scopes,omitempty"`
	ExpiresAt             int64           `json:"expiresAt,omitempty"`
	RefreshTokenExpiresAt int64           `json:"refreshTokenExpiresAt,omitempty"`
	AddedAt               time.Time       `json:"addedAt,omitzero"`
	LastRefreshedAt       time.Time       `json:"lastRefreshedAt,omitzero"`
	LastError             string          `json:"lastError,omitempty"`
	Revoked               bool            `json:"revoked,omitempty"`
	OAuthAccount          json.RawMessage `json:"oauthAccount,omitempty"`
}

// Name is the slot's human label, falling back to the email.
func (s *Slot) Name() string {
	if s.Label != "" {
		return s.Label
	}
	return s.Email
}

// AccessExpiresAt returns the access-token expiry as a time.
func (s *Slot) AccessExpiresAt() time.Time { return millis(s.ExpiresAt) }

// RefreshExpiresAt returns the refresh-token expiry as a time.
func (s *Slot) RefreshExpiresAt() time.Time { return millis(s.RefreshTokenExpiresAt) }

func millis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// ApplyCred copies the parts of a credential that belong in the slot metadata.
func (s *Slot) ApplyCred(c *claudeauth.Cred) {
	s.ExpiresAt = c.ExpiresAt
	s.RefreshTokenExpiresAt = c.RefreshTokenExpiresAt
	if len(c.Scopes) > 0 {
		s.Scopes = c.Scopes
	}
	if c.SubscriptionType != "" {
		s.SubscriptionType = c.SubscriptionType
	}
	if c.RateLimitTier != "" {
		s.RateLimitTier = c.RateLimitTier
	}
}

// State is the whole slot table.
type State struct {
	Version    int       `json:"version"`
	ActiveSlot int       `json:"activeSlot,omitempty"`
	SwitchedAt time.Time `json:"switchedAt,omitzero"`
	Slots      []*Slot   `json:"slots"`
}

// Find returns the slot with the given number.
func (st *State) Find(n int) (*Slot, error) {
	for _, s := range st.Slots {
		if s.N == n {
			return s, nil
		}
	}
	return nil, fmt.Errorf("%w: %d", ErrNoSlot, n)
}

// Active returns the currently switched-in slot, or nil.
func (st *State) Active() *Slot {
	if st.ActiveSlot == 0 {
		return nil
	}
	s, err := st.Find(st.ActiveSlot)
	if err != nil {
		return nil
	}
	return s
}

// FindByAccountUUID returns the slot registered for an account uuid.
func (st *State) FindByAccountUUID(uuid string) *Slot {
	if uuid == "" {
		return nil
	}
	for _, s := range st.Slots {
		if s.AccountUUID == uuid {
			return s
		}
	}
	return nil
}

// FindByEmail returns the slot registered for an email address.
func (st *State) FindByEmail(email string) *Slot {
	if email == "" {
		return nil
	}
	for _, s := range st.Slots {
		if s.Email == email {
			return s
		}
	}
	return nil
}

// nextFreeSlot returns the smallest unused positive slot number, so numbers
// stay small and predictable after a `cas clean`.
func (st *State) nextFreeSlot() int {
	used := map[int]bool{}
	for _, s := range st.Slots {
		used[s.N] = true
	}
	for n := 1; ; n++ {
		if !used[n] {
			return n
		}
	}
}

// Add appends a new slot, assigning it the next free number.
func (st *State) Add(s *Slot) *Slot {
	s.N = st.nextFreeSlot()
	if s.AddedAt.IsZero() {
		s.AddedAt = time.Now()
	}
	st.Slots = append(st.Slots, s)
	st.sort()
	return s
}

// Remove drops a slot from the table.
func (st *State) Remove(n int) bool {
	for i, s := range st.Slots {
		if s.N != n {
			continue
		}
		st.Slots = append(st.Slots[:i], st.Slots[i+1:]...)
		if st.ActiveSlot == n {
			st.ActiveSlot = 0
		}
		return true
	}
	return false
}

func (st *State) sort() {
	sort.Slice(st.Slots, func(i, j int) bool { return st.Slots[i].N < st.Slots[j].N })
}

// Store is a handle on the cas home directory.
type Store struct {
	dir string
}

// Open prepares the cas home directory (default ~/.cas, or $CAS_HOME).
func Open() (*Store, error) {
	dir := os.Getenv("CAS_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locate home directory: %w", err)
		}
		dir = filepath.Join(home, ".cas")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// Dir is the cas home directory.
func (s *Store) Dir() string { return s.dir }

// Path joins a name onto the cas home directory.
func (s *Store) Path(name string) string { return filepath.Join(s.dir, name) }

// Load reads the slot table, returning an empty one if it does not exist yet.
func (s *Store) Load() (*State, error) {
	b, err := os.ReadFile(s.Path(stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return &State{Version: StateVersion}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.Path(stateFile), err)
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.Path(stateFile), err)
	}
	if st.Version > StateVersion {
		return nil, fmt.Errorf("%s was written by a newer cas (version %d); upgrade cas", s.Path(stateFile), st.Version)
	}
	st.Version = StateVersion
	st.sort()
	return &st, nil
}

// Save writes the slot table atomically with 0600 permissions.
func (s *Store) Save(st *State) error {
	st.Version = StateVersion
	st.sort()
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	final := s.Path(stateFile)
	tmp, err := os.CreateTemp(s.dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp state file: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp state file: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("replace %s: %w", final, err)
	}
	return nil
}

// Lock takes an exclusive advisory lock over the cas home directory so the
// launchd refresh agent and an interactive command cannot interleave writes.
// The returned function releases it.
func (s *Store) Lock() (func(), error) {
	f, err := os.OpenFile(s.Path(lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", s.Path(lockFile), err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// slotAccount is the keychain account name for a slot's credential item.
func slotAccount(n int) string { return fmt.Sprintf("slot-%d", n) }

// ReadCred loads a slot's credential from the keychain.
func (s *Store) ReadCred(n int) (*claudeauth.Envelope, error) {
	raw, err := keychain.Get(SlotService, slotAccount(n))
	if err != nil {
		if errors.Is(err, keychain.ErrNotFound) {
			return nil, fmt.Errorf("slot %d has no stored credential (keychain item %s/%s is missing)", n, SlotService, slotAccount(n))
		}
		return nil, err
	}
	return claudeauth.ParseEnvelope(raw)
}

// WriteCred stores a slot's credential in the keychain.
func (s *Store) WriteCred(n int, label string, env *claudeauth.Envelope) error {
	blob, err := env.Encode()
	if err != nil {
		return err
	}
	itemLabel := fmt.Sprintf("cas — slot %d", n)
	if label != "" {
		itemLabel = fmt.Sprintf("cas — slot %d (%s)", n, label)
	}
	return keychain.Set(SlotService, slotAccount(n), itemLabel, blob)
}

// DeleteCred removes a slot's credential from the keychain.
func (s *Store) DeleteCred(n int) error {
	return keychain.Delete(SlotService, slotAccount(n))
}

// HasCred reports whether a slot's keychain item exists.
func (s *Store) HasCred(n int) bool {
	return keychain.Exists(SlotService, slotAccount(n))
}
