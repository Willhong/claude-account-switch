package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Willhong/claude-account-switch/internal/claudeauth"
	"github.com/Willhong/claude-account-switch/internal/store"
	"github.com/Willhong/claude-account-switch/internal/target"
	"github.com/Willhong/claude-account-switch/internal/ui"
)

// Version is stamped at build time (see the Makefile).
var Version = "dev"

// DefaultRefreshThreshold is how close to expiry an access token has to be
// before cas proactively rotates it. Claude Code's own access tokens last
// about 8 hours, so a generous window costs nothing and keeps every slot warm.
const DefaultRefreshThreshold = 45 * time.Minute

// switchSkew is the freshness cas insists on before handing a credential to
// Claude Code, so a switch never lands on a token that expires seconds later.
const switchSkew = 10 * time.Minute

// App carries everything a command needs. It holds an exclusive lock on the
// cas home directory for its whole lifetime.
type App struct {
	Store  *store.Store
	State  *store.State
	Target *target.Target
	OAuth  claudeauth.Config

	unlock func()
}

// New opens the store, takes the lock and loads state.
func New() (*App, error) {
	st, err := store.Open()
	if err != nil {
		return nil, err
	}
	unlock, err := st.Lock()
	if err != nil {
		return nil, err
	}
	state, err := st.Load()
	if err != nil {
		unlock()
		return nil, err
	}
	tgt, err := target.Detect()
	if err != nil {
		unlock()
		return nil, err
	}
	return &App{
		Store:  st,
		State:  state,
		Target: tgt,
		OAuth:  claudeauth.NewConfig(os.Getenv),
		unlock: unlock,
	}, nil
}

// Close releases the lock.
func (a *App) Close() {
	if a.unlock != nil {
		a.unlock()
		a.unlock = nil
	}
}

// Save persists the slot table.
func (a *App) Save() error { return a.Store.Save(a.State) }

// BackupDir is where cas keeps its one-time copy of ~/.claude.json.
func (a *App) BackupDir() string { return a.Store.Path("backups") }

// RefreshThreshold reads CAS_REFRESH_THRESHOLD, falling back to the default.
func RefreshThreshold() time.Duration {
	v := strings.TrimSpace(os.Getenv("CAS_REFRESH_THRESHOLD"))
	if v == "" {
		return DefaultRefreshThreshold
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		ui.Warnf("ignoring invalid CAS_REFRESH_THRESHOLD=%q", v)
		return DefaultRefreshThreshold
	}
	return d
}

// ResolveSlot accepts a slot number, a label, or an email (exact match first,
// then a unique case-insensitive substring).
func (a *App) ResolveSlot(ref string) (*store.Slot, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("no slot given")
	}
	if n, err := strconv.Atoi(ref); err == nil {
		return a.State.Find(n)
	}
	for _, s := range a.State.Slots {
		if s.Label == ref || s.Email == ref {
			return s, nil
		}
	}
	needle := strings.ToLower(ref)
	var matches []*store.Slot
	for _, s := range a.State.Slots {
		if strings.Contains(strings.ToLower(s.Label), needle) || strings.Contains(strings.ToLower(s.Email), needle) {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return nil, fmt.Errorf("no slot matches %q — run `cas list`", ref)
	default:
		var names []string
		for _, s := range matches {
			names = append(names, fmt.Sprintf("%d (%s)", s.N, s.Name()))
		}
		return nil, fmt.Errorf("%q matches several slots: %s", ref, strings.Join(names, ", "))
	}
}

// SyncActive reconciles the slot table with whatever Claude Code currently has
// stored. Claude Code rotates its own access token in the background, so the
// live credential is often newer than the copy cas holds for the active slot.
//
// It returns true when the state changed and needs saving.
func (a *App) SyncActive() bool {
	live, err := a.Target.ReadCred()
	if err != nil {
		if !errors.Is(err, target.ErrNoCredential) {
			ui.Warnf("could not read the live Claude Code credential: %v", err)
		}
		return false
	}

	if len(a.State.Slots) == 0 {
		return false
	}

	active := a.State.Active()
	if active != nil {
		if stored, serr := a.Store.ReadCred(active.N); serr == nil &&
			stored.OAuth.AccessToken == live.OAuth.AccessToken {
			return false // nothing has moved since cas last looked
		}
	}

	// The live token is not the one cas installed. Establish whose it is before
	// copying it anywhere: a Claude Code session that was already running when
	// the user switched still holds the previous account and can write that
	// account's refreshed token back over ours.
	rawAccount, _ := a.Target.ReadOAuthAccount()
	owner := target.AccountUUID(rawAccount)
	if confirmed := a.confirmOwner(live.OAuth.AccessToken); confirmed != "" {
		owner = confirmed
	}

	if active != nil && (owner == "" || active.AccountUUID == "" || owner == active.AccountUUID) {
		return a.adoptLiveInto(active, live, rawAccount)
	}

	// Claude Code is on an account cas did not switch to — most likely the user
	// ran `claude /login` directly, or a stale session wrote its own token back.
	if s := a.State.FindByAccountUUID(owner); s != nil {
		a.State.ActiveSlot = s.N
		a.adoptLiveInto(s, live, rawAccount)
		return true
	}
	if a.State.ActiveSlot != 0 {
		a.State.ActiveSlot = 0
		return true
	}
	return false
}

// confirmOwner asks Anthropic which account an access token belongs to. It is
// best-effort: an unreachable API yields "", and the caller falls back to the
// account recorded in ~/.claude.json. Only reached when the live token has
// actually changed, so it costs a request a few times a day at most.
func (a *App) confirmOwner(accessToken string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	v, err := a.OAuth.Validate(ctx, accessToken)
	if err != nil || v == nil || !v.Valid {
		return ""
	}
	return v.AccountUUID
}

// adoptLiveInto copies the live credential into a slot when it is newer.
func (a *App) adoptLiveInto(s *store.Slot, live *claudeauth.Envelope, rawAccount []byte) bool {
	stored, err := a.Store.ReadCred(s.N)
	if err == nil && stored.OAuth.AccessToken == live.OAuth.AccessToken {
		return false
	}
	if err := a.Store.WriteCred(s.N, s.Label, live); err != nil {
		ui.Warnf("could not sync slot %d from the live credential: %v", s.N, err)
		return false
	}
	s.ApplyCred(live.OAuth)
	s.Revoked = false
	s.LastError = ""

	// Only carry the config's account block over when it really describes this
	// slot, so a mismatched ~/.claude.json cannot rewrite the wrong identity.
	if len(rawAccount) > 0 {
		if uuid := target.AccountUUID(rawAccount); uuid == s.AccountUUID || s.AccountUUID == "" {
			s.OAuthAccount = append([]byte(nil), rawAccount...)
		}
	}
	return true
}

// refreshOutcome is one slot's result from a refresh pass.
type refreshOutcome struct {
	Slot    *store.Slot
	Env     *claudeauth.Envelope
	Profile *claudeauth.Profile
	Roles   *claudeauth.Roles
	Err     error
	Revoked bool
	Skipped bool
}

// RefreshOptions tunes a refresh pass.
type RefreshOptions struct {
	// Force refreshes every slot regardless of how much life its token has left.
	Force bool
	// Threshold is how close to expiry a token must be to be refreshed.
	Threshold time.Duration
	// WithProfile also re-reads the account profile, keeping emails and plan
	// labels current.
	WithProfile bool
}

// Refresh rotates the access tokens of the given slots. Network calls run
// concurrently; all writes happen afterwards on this goroutine, in slot order.
func (a *App) Refresh(ctx context.Context, slots []*store.Slot, opts RefreshOptions) []refreshOutcome {
	outcomes := make([]refreshOutcome, len(slots))
	var wg sync.WaitGroup

	for i, s := range slots {
		outcomes[i].Slot = s

		env, err := a.Store.ReadCred(s.N)
		if err != nil {
			outcomes[i].Err = err
			continue
		}
		if !opts.Force && !env.OAuth.AccessExpired(opts.Threshold) {
			outcomes[i].Skipped = true
			outcomes[i].Env = env
			continue
		}
		if env.OAuth.RefreshExpired() {
			outcomes[i].Revoked = true
			outcomes[i].Err = fmt.Errorf("refresh token expired on %s", env.OAuth.RefreshExpiresAtTime().Format(time.RFC3339))
			continue
		}

		wg.Add(1)
		go func(i int, s *store.Slot, env *claudeauth.Envelope) {
			defer wg.Done()
			res, err := a.OAuth.Refresh(ctx, env.OAuth)
			if err != nil {
				outcomes[i].Err = err
				outcomes[i].Revoked = errors.Is(err, claudeauth.ErrInvalidGrant)
				return
			}
			next := env.Clone()
			next.OAuth = res.Cred
			outcomes[i].Env = next

			if opts.WithProfile {
				profile, perr := a.OAuth.FetchProfile(ctx, res.Cred.AccessToken)
				if perr == nil {
					outcomes[i].Profile = profile
					if roles, rerr := a.OAuth.FetchRoles(ctx, res.Cred.AccessToken); rerr == nil {
						outcomes[i].Roles = roles
					}
				}
			}
			if res.Email != "" {
				s.Email = res.Email
			}
			if res.AccountUUID != "" {
				s.AccountUUID = res.AccountUUID
			}
			if res.OrgUUID != "" {
				s.OrgUUID = res.OrgUUID
			}
		}(i, s, env)
	}
	wg.Wait()

	for i := range outcomes {
		o := &outcomes[i]
		s := o.Slot
		switch {
		case o.Skipped:
			continue
		case o.Revoked:
			s.Revoked = true
			s.LastError = o.Err.Error()
		case o.Err != nil:
			s.LastError = o.Err.Error()
		default:
			if o.Profile != nil {
				applyProfile(s, o.Env.OAuth, o.Profile, o.Roles)
			}
			if err := a.Store.WriteCred(s.N, s.Label, o.Env); err != nil {
				o.Err = err
				s.LastError = err.Error()
				continue
			}
			s.ApplyCred(o.Env.OAuth)
			s.LastRefreshedAt = time.Now()
			s.Revoked = false
			s.LastError = ""

			// Keep Claude Code itself on the rotated token.
			if a.State.ActiveSlot == s.N {
				if err := a.Target.WriteCred(o.Env); err != nil {
					ui.Warnf("slot %d refreshed, but writing it back to Claude Code failed: %v", s.N, err)
				}
			}
		}
	}
	return outcomes
}

// applyProfile folds a freshly fetched profile into a slot and its credential.
func applyProfile(s *store.Slot, c *claudeauth.Cred, p *claudeauth.Profile, roles *claudeauth.Roles) {
	if p.Account.Email != "" {
		s.Email = p.Account.Email
	}
	if p.Account.UUID != "" {
		s.AccountUUID = p.Account.UUID
	}
	if p.Organization.UUID != "" {
		s.OrgUUID = p.Organization.UUID
	}
	s.OrgName = p.Organization.Name
	s.DisplayName = p.Account.DisplayName
	if plan := p.SubscriptionType(); plan != "" {
		s.SubscriptionType = plan
		c.SubscriptionType = plan
	}
	if tier := p.Organization.RateLimitTier; tier != "" {
		s.RateLimitTier = tier
		c.RateLimitTier = tier
	}
	if acct := target.BuildOAuthAccount(p, roles); len(acct) > 0 {
		s.OAuthAccount = acct
	}
}

// AutoRefresh runs the opportunistic pass that every interactive command
// performs, so a slot is never stale by the time the user switches to it.
func (a *App) AutoRefresh(ctx context.Context, enabled bool) bool {
	if !enabled || len(a.State.Slots) == 0 {
		return false
	}
	threshold := RefreshThreshold()

	var due []*store.Slot
	for _, s := range a.State.Slots {
		if s.Revoked {
			continue
		}
		env, err := a.Store.ReadCred(s.N)
		if err != nil {
			continue
		}
		if env.OAuth.AccessExpired(threshold) && !env.OAuth.RefreshExpired() {
			due = append(due, s)
		}
	}
	if len(due) == 0 {
		return false
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	for _, o := range a.Refresh(ctx, due, RefreshOptions{Threshold: threshold}) {
		if o.Err != nil {
			ui.Warnf("slot %d (%s): %v", o.Slot.N, o.Slot.Name(), o.Err)
		}
	}
	return true
}

// EnsureFresh rotates one slot's token if it is close to expiry, returning the
// credential that should be handed to Claude Code.
func (a *App) EnsureFresh(ctx context.Context, s *store.Slot) (*claudeauth.Envelope, error) {
	env, err := a.Store.ReadCred(s.N)
	if err != nil {
		return nil, err
	}
	if !env.OAuth.AccessExpired(switchSkew) {
		return env, nil
	}
	if env.OAuth.RefreshExpired() {
		return nil, fmt.Errorf("slot %d (%s) has an expired refresh token — run `cas login` to sign in again", s.N, s.Name())
	}

	res, err := a.OAuth.Refresh(ctx, env.OAuth)
	if err != nil {
		if errors.Is(err, claudeauth.ErrInvalidGrant) {
			s.Revoked = true
			s.LastError = err.Error()
			return nil, fmt.Errorf("slot %d (%s) was rejected by Anthropic — run `cas login` to sign in again", s.N, s.Name())
		}
		// A network blip should not block a switch: the stored token may still
		// have minutes of life left.
		if !env.OAuth.AccessExpired(0) {
			ui.Warnf("could not refresh slot %d before switching (%v); using the stored token", s.N, err)
			return env, nil
		}
		return nil, err
	}

	next := env.Clone()
	next.OAuth = res.Cred
	if err := a.Store.WriteCred(s.N, s.Label, next); err != nil {
		return nil, err
	}
	s.ApplyCred(res.Cred)
	s.LastRefreshedAt = time.Now()
	s.Revoked = false
	s.LastError = ""
	return next, nil
}
