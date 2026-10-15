// Package codexstore persists the Codex account slot table and keeps each
// credential in a separate macOS Keychain item.
package codexstore

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Willhong/claude-account-switch/internal/codexauth"
	"github.com/Willhong/claude-account-switch/internal/keychain"
)

const (
	// SlotService is the Keychain service for parked Codex credentials.
	SlotService = "cas-codex-credentials"
	stateFile   = "codex-state.json"
	lockFile    = ".lock"
	// StateVersion is bumped when the Codex slot schema changes incompatibly.
	StateVersion = 1

	credentialManifestPrefix = "chunked-v1:"
	credentialChunkSize      = 1200
	maxCredentialChunks      = 100
)

// ErrNoSlot is returned when a Codex slot number does not exist.
var ErrNoSlot = errors.New("no such Codex slot")

// Slot is one registered Codex account.
type Slot struct {
	N             int       `json:"slot"`
	Label         string    `json:"label,omitempty"`
	Account       string    `json:"account"`
	Email         string    `json:"email,omitempty"`
	AccountID     string    `json:"accountId,omitempty"`
	Identity      string    `json:"identity"`
	AuthMode      string    `json:"authMode"`
	Plan          string    `json:"plan,omitempty"`
	ExpiresAt     time.Time `json:"expiresAt,omitzero"`
	LastRefresh   time.Time `json:"lastRefresh,omitzero"`
	LastSyncedAt  time.Time `json:"lastSyncedAt,omitzero"`
	CredentialTag string    `json:"credentialTag,omitempty"`
	AddedAt       time.Time `json:"addedAt,omitzero"`
}

// Name is the slot label, falling back to its account description.
func (s *Slot) Name() string {
	if s.Label != "" {
		return s.Label
	}
	return s.Account
}

// Apply copies safe metadata from a live credential into a slot.
func (s *Slot) Apply(doc *codexauth.Document) {
	s.Account = doc.DisplayAccount()
	s.Email = doc.Email
	s.AccountID = doc.AccountID
	s.Identity = doc.Identity()
	s.AuthMode = doc.AuthMode
	s.Plan = doc.Plan
	s.ExpiresAt = doc.ExpiresAt
	s.LastRefresh = doc.LastRefresh
	s.LastSyncedAt = time.Now()
	s.CredentialTag = doc.Fingerprint()
}

// State is the Codex account slot table.
type State struct {
	Version    int       `json:"version"`
	ActiveSlot int       `json:"activeSlot,omitempty"`
	SwitchedAt time.Time `json:"switchedAt,omitzero"`
	Slots      []*Slot   `json:"slots"`
}

// Find returns the slot with n.
func (s *State) Find(n int) (*Slot, error) {
	for _, slot := range s.Slots {
		if slot.N == n {
			return slot, nil
		}
	}
	return nil, fmt.Errorf("%w: %d", ErrNoSlot, n)
}

// Active returns the currently active slot, if it still exists.
func (s *State) Active() *Slot {
	if s.ActiveSlot == 0 {
		return nil
	}
	slot, err := s.Find(s.ActiveSlot)
	if err != nil {
		return nil
	}
	return slot
}

// FindByIdentity returns the slot for a stable Codex account identity.
func (s *State) FindByIdentity(identity string) *Slot {
	if identity == "" {
		return nil
	}
	for _, slot := range s.Slots {
		if slot.Identity == identity {
			return slot
		}
	}
	return nil
}

// Add assigns the smallest available positive slot number.
func (s *State) Add(slot *Slot) *Slot {
	used := map[int]bool{}
	for _, existing := range s.Slots {
		used[existing.N] = true
	}
	for n := 1; ; n++ {
		if !used[n] {
			slot.N = n
			break
		}
	}
	if slot.AddedAt.IsZero() {
		slot.AddedAt = time.Now()
	}
	s.Slots = append(s.Slots, slot)
	s.sort()
	return slot
}

// Remove drops one slot and clears the active marker when needed.
func (s *State) Remove(n int) bool {
	for i, slot := range s.Slots {
		if slot.N != n {
			continue
		}
		s.Slots = append(s.Slots[:i], s.Slots[i+1:]...)
		if s.ActiveSlot == n {
			s.ActiveSlot = 0
		}
		return true
	}
	return false
}

func (s *State) sort() {
	sort.Slice(s.Slots, func(i, j int) bool { return s.Slots[i].N < s.Slots[j].N })
}

// Resolve accepts a number, label, email, account id, or unique substring.
func (s *State) Resolve(ref string) (*Slot, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("no Codex slot given")
	}
	if n, err := strconv.Atoi(ref); err == nil {
		return s.Find(n)
	}
	for _, slot := range s.Slots {
		if slot.Label == ref || slot.Email == ref || slot.AccountID == ref {
			return slot, nil
		}
	}
	needle := strings.ToLower(ref)
	var matches []*Slot
	for _, slot := range s.Slots {
		if strings.Contains(strings.ToLower(slot.Label), needle) ||
			strings.Contains(strings.ToLower(slot.Account), needle) ||
			strings.Contains(strings.ToLower(slot.AccountID), needle) {
			matches = append(matches, slot)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return nil, fmt.Errorf("no Codex slot matches %q; run `cas codex list`", ref)
	default:
		names := make([]string, 0, len(matches))
		for _, slot := range matches {
			names = append(names, fmt.Sprintf("%d (%s)", slot.N, slot.Name()))
		}
		return nil, fmt.Errorf("%q matches several Codex slots: %s", ref, strings.Join(names, ", "))
	}
}

// Store owns the Codex state file and slot credentials.
type Store struct{ dir string }

// Open prepares the shared cas home directory.
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

// Dir returns the cas home directory.
func (s *Store) Dir() string { return s.dir }

// Path joins a name to the cas home directory.
func (s *Store) Path(name string) string { return filepath.Join(s.dir, name) }

// Load reads the Codex state file.
func (s *Store) Load() (*State, error) {
	raw, err := os.ReadFile(s.Path(stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return &State{Version: StateVersion}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.Path(stateFile), err)
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.Path(stateFile), err)
	}
	if state.Version > StateVersion {
		return nil, fmt.Errorf("%s was written by a newer cas (version %d); upgrade cas", s.Path(stateFile), state.Version)
	}
	state.Version = StateVersion
	state.sort()
	return &state, nil
}

// Save atomically writes non-secret Codex slot metadata.
func (s *Store) Save(state *State) error {
	state.Version = StateVersion
	state.sort()
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(s.dir, ".codex-state-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, s.Path(stateFile)); err != nil {
		return fmt.Errorf("replace %s: %w", s.Path(stateFile), err)
	}
	return nil
}

// Lock serializes Claude and Codex cas commands with the shared lock file.
func (s *Store) Lock() (func(), error) {
	f, err := os.OpenFile(s.Path(lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func slotAccount(n int) string { return fmt.Sprintf("slot-%d", n) }

func chunkAccount(n int, digest string, part int) string {
	return fmt.Sprintf("slot-%d-%s-part-%d", n, digest, part)
}

type credentialManifest struct {
	Count  int
	Digest string
}

func parseManifest(value string) (credentialManifest, bool) {
	if !strings.HasPrefix(value, credentialManifestPrefix) {
		return credentialManifest{}, false
	}
	parts := strings.Split(strings.TrimPrefix(value, credentialManifestPrefix), ":")
	if len(parts) != 2 {
		return credentialManifest{}, false
	}
	count, err := strconv.Atoi(parts[0])
	decodedDigest, digestErr := hex.DecodeString(parts[1])
	if err != nil || count < 1 || count > maxCredentialChunks || digestErr != nil || len(decodedDigest) != sha256.Size {
		return credentialManifest{}, false
	}
	return credentialManifest{Count: count, Digest: parts[1]}, true
}

// ReadCredential loads a parked Codex credential from Keychain.
func (s *Store) ReadCredential(n int) (*codexauth.Document, error) {
	value, err := keychain.Get(SlotService, slotAccount(n))
	if err != nil {
		if errors.Is(err, keychain.ErrNotFound) {
			return nil, fmt.Errorf("Codex slot %d has no stored credential", n)
		}
		return nil, err
	}
	manifest, chunked := parseManifest(value)
	if !chunked {
		// Accept the single-item shape used by development builds before
		// credentials were chunked.
		return codexauth.Parse([]byte(value))
	}
	var encoded strings.Builder
	for part := 0; part < manifest.Count; part++ {
		chunk, err := keychain.Get(SlotService, chunkAccount(n, manifest.Digest, part))
		if err != nil {
			return nil, fmt.Errorf("read Codex slot %d credential part %d: %w", n, part+1, err)
		}
		encoded.WriteString(chunk)
	}
	raw, err := decodeCredential(encoded.String(), manifest.Digest)
	if err != nil {
		return nil, fmt.Errorf("decode Codex slot %d credential: %w", n, err)
	}
	return codexauth.Parse(raw)
}

// WriteCredential saves one complete Codex auth document in Keychain.
func (s *Store) WriteCredential(slot *Slot, doc *codexauth.Document) error {
	label := keychainLabel(slot)
	raw := doc.Encode()
	encoded, digest, err := encodeCredential(raw)
	if err != nil {
		return err
	}
	count := (len(encoded) + credentialChunkSize - 1) / credentialChunkSize
	if count < 1 || count > maxCredentialChunks {
		return fmt.Errorf("Codex credential needs %d Keychain parts; maximum is %d", count, maxCredentialChunks)
	}

	oldValue, _ := keychain.Get(SlotService, slotAccount(slot.N))
	oldManifest, hadOldManifest := parseManifest(oldValue)
	written := 0
	for part := 0; part < count; part++ {
		start := part * credentialChunkSize
		end := min(start+credentialChunkSize, len(encoded))
		partLabel := fmt.Sprintf("%s — credential part %d", label, part+1)
		if err := keychain.SetStdinOnly(SlotService, chunkAccount(slot.N, digest, part), partLabel, encoded[start:end]); err != nil {
			if !hadOldManifest || oldManifest.Digest != digest {
				deleteCredentialParts(slot.N, digest, written)
			}
			return err
		}
		written++
	}
	manifestValue := fmt.Sprintf("%s%d:%s", credentialManifestPrefix, count, digest)
	if err := keychain.SetStdinOnly(SlotService, slotAccount(slot.N), label, manifestValue); err != nil {
		if !hadOldManifest || oldManifest.Digest != digest {
			deleteCredentialParts(slot.N, digest, written)
		}
		return err
	}
	if hadOldManifest && oldManifest.Digest != digest {
		deleteCredentialParts(slot.N, oldManifest.Digest, oldManifest.Count)
	}
	return nil
}

// DeleteCredential removes one parked Codex credential.
func (s *Store) DeleteCredential(n int) error {
	if value, err := keychain.Get(SlotService, slotAccount(n)); err == nil {
		if manifest, ok := parseManifest(value); ok {
			deleteCredentialParts(n, manifest.Digest, manifest.Count)
		}
	}
	return keychain.Delete(SlotService, slotAccount(n))
}

// HasCredential reports whether the slot Keychain item exists.
func (s *Store) HasCredential(n int) bool {
	return keychain.Exists(SlotService, slotAccount(n))
}

func deleteCredentialParts(n int, digest string, count int) {
	for part := 0; part < count; part++ {
		_ = keychain.Delete(SlotService, chunkAccount(n, digest, part))
	}
}

func keychainLabel(slot *Slot) string {
	label := fmt.Sprintf("cas codex — slot %d", slot.N)
	if slot.Label == "" {
		return label
	}
	safeLabel := strings.NewReplacer("\r", " ", "\n", " ").Replace(slot.Label)
	runes := []rune(safeLabel)
	if len(runes) > 80 {
		runes = runes[:80]
	}
	return label + " (" + string(runes) + ")"
}

func encodeCredential(raw []byte) (encoded string, digest string, err error) {
	digest = fmt.Sprintf("%x", sha256.Sum256(raw))
	var compressed bytes.Buffer
	writer, err := gzip.NewWriterLevel(&compressed, gzip.BestCompression)
	if err != nil {
		return "", "", err
	}
	if _, err := writer.Write(raw); err != nil {
		return "", "", err
	}
	if err := writer.Close(); err != nil {
		return "", "", err
	}
	return base64.RawStdEncoding.EncodeToString(compressed.Bytes()), digest, nil
}

func decodeCredential(encoded, wantDigest string) ([]byte, error) {
	compressed, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(reader, 10<<20))
	closeErr := reader.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if digest := fmt.Sprintf("%x", sha256.Sum256(raw)); digest != wantDigest {
		return nil, errors.New("credential checksum mismatch")
	}
	return raw, nil
}
