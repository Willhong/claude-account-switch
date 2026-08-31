package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Willhong/claude-account-switch/internal/store"
	"github.com/Willhong/claude-account-switch/internal/ui"
)

// CmdRefresh rotates access tokens so no slot goes stale while it is parked.
// With no arguments it refreshes every slot that is close to expiry, which is
// also what the launchd agent runs.
func CmdRefresh(args []string) error {
	fs := flag.NewFlagSet("cas refresh", flag.ContinueOnError)
	force := fs.Bool("force", false, "refresh every slot even if its token has plenty of life left")
	quiet := fs.Bool("quiet", false, "only report failures (used by the background agent)")
	profile := fs.Bool("profile", false, "also re-read each account's profile (email, plan, organization)")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}

	a, err := New()
	if err != nil {
		return err
	}
	defer a.Close()

	a.SyncActive()

	slots := a.State.Slots
	if len(positional) > 0 {
		slots = nil
		for _, ref := range positional {
			s, err := a.ResolveSlot(ref)
			if err != nil {
				return err
			}
			slots = append(slots, s)
		}
	}
	if len(slots) == 0 {
		if !*quiet {
			ui.Infof("No accounts registered.")
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	outcomes := a.Refresh(ctx, slots, RefreshOptions{
		Force:       *force,
		Threshold:   RefreshThreshold(),
		WithProfile: *profile,
	})

	var failed int
	for _, o := range outcomes {
		switch {
		case o.Err != nil:
			failed++
			ui.Errorf("slot %d (%s): %v", o.Slot.N, o.Slot.Name(), o.Err)
		case o.Skipped:
			if !*quiet {
				fmt.Printf("  %s slot %d  %-32s %s\n", ui.Dim("·"), o.Slot.N, o.Slot.Email,
					ui.Dim("still fresh, "+ui.RelativeTime(o.Slot.AccessExpiresAt())))
			}
		default:
			if !*quiet {
				fmt.Printf("  %s slot %d  %-32s %s\n", ui.Green("✓"), o.Slot.N, o.Slot.Email,
					"renewed, "+ui.RelativeTime(o.Slot.AccessExpiresAt()))
			}
		}
	}

	if err := a.Save(); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d account(s) could not be refreshed", failed)
	}
	return nil
}

// CmdRemove forgets a slot without touching the account itself.
func CmdRemove(args []string) error {
	fs := flag.NewFlagSet("cas remove", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: cas remove <slot|label|email>")
	}

	a, err := New()
	if err != nil {
		return err
	}
	defer a.Close()

	slot, err := a.ResolveSlot(positional[0])
	if err != nil {
		return err
	}
	if !*yes {
		fmt.Fprintf(os.Stderr, "Remove slot %d (%s)? [y/N] ", slot.N, slot.Email)
		if !confirmed() {
			ui.Infof("Nothing removed.")
			return nil
		}
	}

	wasActive := a.State.ActiveSlot == slot.N
	if err := a.Store.DeleteCred(slot.N); err != nil {
		ui.Warnf("could not delete slot %d's keychain item: %v", slot.N, err)
	}
	a.State.Remove(slot.N)
	if err := a.Save(); err != nil {
		return err
	}
	ui.OKf("Removed slot %d (%s).", slot.N, slot.Email)
	if wasActive {
		ui.Warnf("that was the active account; Claude Code still holds its credential. Run `cas switch <n>` to move to another.")
	}
	return nil
}

// CmdLabel renames a slot.
func CmdLabel(args []string) error {
	fs := flag.NewFlagSet("cas label", flag.ContinueOnError)
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		return errors.New("usage: cas label <slot|label|email> <new-label>")
	}

	a, err := New()
	if err != nil {
		return err
	}
	defer a.Close()

	slot, err := a.ResolveSlot(positional[0])
	if err != nil {
		return err
	}
	slot.Label = positional[1]

	// Keep the keychain item's display name in step.
	if env, err := a.Store.ReadCred(slot.N); err == nil {
		if err := a.Store.WriteCred(slot.N, slot.Label, env); err != nil {
			ui.Warnf("could not rename slot %d's keychain item: %v", slot.N, err)
		}
	}
	if err := a.Save(); err != nil {
		return err
	}
	ui.OKf("Slot %d is now labelled %q.", slot.N, slot.Label)
	return nil
}

// slotSummary is a one-line description used in a few places.
func slotSummary(s *store.Slot) string {
	if s.Label != "" {
		return fmt.Sprintf("slot %d (%s — %s)", s.N, s.Label, s.Email)
	}
	return fmt.Sprintf("slot %d (%s)", s.N, s.Email)
}
