package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/Willhong/claude-account-switch/internal/store"
	"github.com/Willhong/claude-account-switch/internal/target"
	"github.com/Willhong/claude-account-switch/internal/ui"
)

// CmdSwitch makes a slot the account Claude Code uses.
func CmdSwitch(args []string) error {
	fs := flag.NewFlagSet("cas switch", flag.ContinueOnError)
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: cas switch <slot|label|email>")
	}

	a, err := New()
	if err != nil {
		return err
	}
	defer a.Close()

	a.SyncActive()

	slot, err := a.ResolveSlot(positional[0])
	if err != nil {
		return err
	}
	if slot.N == a.State.ActiveSlot {
		ui.Infof("Already on slot %d (%s).", slot.N, slot.Email)
		return a.Save()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := a.activate(ctx, slot); err != nil {
		return err
	}
	return a.Save()
}

// activate installs a slot's credential as the live one and points
// ~/.claude.json at the matching account.
func (a *App) activate(ctx context.Context, slot *store.Slot) error {
	env, err := a.EnsureFresh(ctx, slot)
	if err != nil {
		return err
	}

	// Capture whatever the outgoing account looks like right now, so the slot
	// it belongs to keeps an accurate oauthAccount block.
	if prev := a.State.Active(); prev != nil && prev.N != slot.N {
		if raw, rerr := a.Target.ReadOAuthAccount(); rerr == nil && len(raw) > 0 {
			if target.AccountUUID(raw) == prev.AccountUUID || prev.AccountUUID == "" {
				prev.OAuthAccount = append([]byte(nil), raw...)
			}
		}
	}

	if err := a.Target.WriteCred(env); err != nil {
		return fmt.Errorf("install slot %d's credential: %w", slot.N, err)
	}

	if len(slot.OAuthAccount) > 0 {
		if err := a.Target.WriteOAuthAccount(slot.OAuthAccount, a.BackupDir()); err != nil {
			ui.Warnf("credential switched, but %s could not be updated: %v", a.Target.ConfigPath, err)
			ui.Warnf("Claude Code may still show the previous account's email until it refreshes its profile.")
		}
	} else {
		ui.Warnf("slot %d has no cached account profile; Claude Code may show a stale email until it refreshes.", slot.N)
	}

	a.State.ActiveSlot = slot.N
	a.State.SwitchedAt = time.Now()
	ui.OKf("Switched to slot %d — %s", slot.N, ui.Bold(slot.Email))

	// Sessions that were already running kept the outgoing account in memory,
	// and will write it back over this one the next time they refresh a token.
	if stale := a.staleSessions(); len(stale) > 0 {
		ui.Warnf("%d Claude Code session(s) are still running on the previous account; their next token refresh can undo this switch.", len(stale))
		ui.Warnf("run `cas sessions` to see them, or `cas reap` to close the ones nobody is using.")
	}
	return nil
}
