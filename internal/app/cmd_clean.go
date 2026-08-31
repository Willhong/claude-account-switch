package app

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Willhong/claude-account-switch/internal/claudeauth"
	"github.com/Willhong/claude-account-switch/internal/store"
	"github.com/Willhong/claude-account-switch/internal/ui"
)

// CmdClean removes slots whose credentials can no longer be revived.
//
// A slot is dead when its refresh token has expired, when its keychain item is
// gone, or when Anthropic rejects the refresh token outright. Slots that are
// merely stale are refreshed rather than removed.
func CmdClean(args []string) error {
	fs := flag.NewFlagSet("cas clean", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "report what would be removed without removing it")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}

	a, err := New()
	if err != nil {
		return err
	}
	defer a.Close()

	a.SyncActive()

	if len(a.State.Slots) == 0 {
		ui.Infof("No accounts registered.")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ui.Infof("Checking %d account(s)…", len(a.State.Slots))
	verdicts := a.checkSlots(ctx)

	var dead []*store.Slot
	for _, v := range verdicts {
		switch {
		case v.dead:
			dead = append(dead, v.slot)
			fmt.Printf("  %s slot %d  %-32s %s\n", ui.Red("✗"), v.slot.N, v.slot.Email, ui.Dim(v.reason))
		default:
			fmt.Printf("  %s slot %d  %-32s %s\n", ui.Green("✓"), v.slot.N, v.slot.Email, ui.Dim(v.reason))
		}
	}

	if len(dead) == 0 {
		ui.OKf("Every account still has a usable credential.")
		return a.Save()
	}

	if *dryRun {
		ui.Infof("\n%d account(s) would be removed. Re-run without --dry-run to apply.", len(dead))
		return a.Save()
	}

	if !*yes {
		var names []string
		for _, s := range dead {
			names = append(names, fmt.Sprintf("%d (%s)", s.N, s.Email))
		}
		fmt.Fprintf(os.Stderr, "\nRemove %d account(s): %s? [y/N] ", len(dead), strings.Join(names, ", "))
		if !confirmed() {
			ui.Infof("Nothing removed.")
			return a.Save()
		}
	}

	for _, s := range dead {
		wasActive := a.State.ActiveSlot == s.N
		if err := a.Store.DeleteCred(s.N); err != nil {
			ui.Warnf("could not delete slot %d's keychain item: %v", s.N, err)
		}
		a.State.Remove(s.N)
		ui.OKf("Removed slot %d (%s).", s.N, s.Email)
		if wasActive {
			ui.Warnf("that was the active account; Claude Code is now signed in with an expired credential. Run `cas switch <n>` or `cas login`.")
		}
	}
	return a.Save()
}

type verdict struct {
	slot   *store.Slot
	dead   bool
	reason string
}

// checkSlots probes every slot concurrently. Live access tokens are checked
// with the non-rotating /validate endpoint; expired ones are proven dead or
// alive by attempting a refresh, and a successful refresh is kept.
func (a *App) checkSlots(ctx context.Context) []verdict {
	verdicts := make([]verdict, len(a.State.Slots))
	type refreshed struct {
		env *claudeauth.Envelope
		ok  bool
	}
	results := make([]refreshed, len(a.State.Slots))

	var wg sync.WaitGroup
	for i, s := range a.State.Slots {
		verdicts[i].slot = s

		env, err := a.Store.ReadCred(s.N)
		if err != nil {
			verdicts[i].dead = true
			verdicts[i].reason = "credential missing from the keychain"
			continue
		}
		if env.OAuth.RefreshExpired() {
			verdicts[i].dead = true
			verdicts[i].reason = "refresh token expired " + ui.Duration(time.Since(env.OAuth.RefreshExpiresAtTime())) + " ago"
			continue
		}

		wg.Add(1)
		go func(i int, s *store.Slot, env *claudeauth.Envelope) {
			defer wg.Done()

			if !env.OAuth.AccessExpired(0) {
				v, err := a.OAuth.Validate(ctx, env.OAuth.AccessToken)
				if err == nil && v.Valid {
					verdicts[i].reason = "access token valid, " + ui.RelativeTime(env.OAuth.ExpiresAtTime())
					return
				}
				if err != nil {
					verdicts[i].reason = "could not reach Anthropic: " + err.Error()
					return
				}
			}

			res, err := a.OAuth.Refresh(ctx, env.OAuth)
			switch {
			case errors.Is(err, claudeauth.ErrInvalidGrant):
				verdicts[i].dead = true
				verdicts[i].reason = "refresh token rejected by Anthropic"
			case err != nil:
				// An unreachable API is not evidence that a credential is dead.
				verdicts[i].reason = "could not verify: " + err.Error()
			default:
				next := env.Clone()
				next.OAuth = res.Cred
				results[i] = refreshed{env: next, ok: true}
				verdicts[i].reason = "renewed, " + ui.RelativeTime(res.Cred.ExpiresAtTime())
			}
		}(i, s, env)
	}
	wg.Wait()

	for i, r := range results {
		if !r.ok {
			continue
		}
		s := verdicts[i].slot
		if err := a.Store.WriteCred(s.N, s.Label, r.env); err != nil {
			ui.Warnf("slot %d renewed but could not be saved: %v", s.N, err)
			continue
		}
		s.ApplyCred(r.env.OAuth)
		s.LastRefreshedAt = time.Now()
		s.Revoked = false
		s.LastError = ""
		if a.State.ActiveSlot == s.N {
			if err := a.Target.WriteCred(r.env); err != nil {
				ui.Warnf("slot %d renewed, but writing it back to Claude Code failed: %v", s.N, err)
			}
		}
	}

	for i := range verdicts {
		if verdicts[i].dead {
			verdicts[i].slot.Revoked = true
			verdicts[i].slot.LastError = verdicts[i].reason
		}
	}
	return verdicts
}

func confirmed() bool {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}
