package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Willhong/claude-account-switch/internal/codexappserver"
	"github.com/Willhong/claude-account-switch/internal/codexauth"
	"github.com/Willhong/claude-account-switch/internal/codexstore"
	"github.com/Willhong/claude-account-switch/internal/codextarget"
	"github.com/Willhong/claude-account-switch/internal/ui"
)

func cmdCodexRefresh(args []string) error {
	fs := flag.NewFlagSet("cas codex refresh", flag.ContinueOnError)
	force := fs.Bool("force", false, "refresh every ChatGPT slot regardless of access-token expiry")
	quiet := fs.Bool("quiet", false, "only report failures")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	a, err := newCodexApp()
	if err != nil {
		return err
	}
	defer a.Close()
	if a.SyncActive() {
		if err := a.Save(); err != nil {
			return err
		}
	}

	slots, err := resolveCodexSlots(a.State, positional)
	if err != nil {
		return err
	}
	if len(slots) == 0 {
		if !*quiet {
			ui.Infof("No Codex accounts registered.")
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	failed := 0
	for _, slot := range slots {
		outcome := a.refreshCodexSlot(ctx, slot, *force)
		switch {
		case outcome.Err != nil:
			failed++
			ui.Errorf("Codex slot %d (%s): %v", slot.N, slot.Name(), outcome.Err)
		case outcome.Skipped:
			if !*quiet {
				reason := "still fresh"
				if outcome.Doc != nil && outcome.Doc.APIKey != "" {
					reason = "API key needs no refresh"
				} else if outcome.Doc != nil {
					reason += ", " + ui.RelativeTime(outcome.Doc.ExpiresAt)
				}
				fmt.Printf("  %s Codex slot %d  %-32s %s\n", ui.Dim("·"), slot.N, slot.Account, ui.Dim(reason))
			}
		default:
			if !*quiet {
				detail := "credential verified"
				if outcome.Changed {
					detail = "renewed, " + ui.RelativeTime(outcome.Doc.ExpiresAt)
				}
				fmt.Printf("  %s Codex slot %d  %-32s %s\n", ui.Green("✓"), slot.N, slot.Account, detail)
			}
		}
	}
	if err := a.Save(); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d Codex account(s) could not be refreshed", failed)
	}
	return nil
}

type codexRefreshOutcome struct {
	Doc     *codexauth.Document
	Err     error
	Skipped bool
	Changed bool
}

func (a *codexApp) refreshCodexSlot(ctx context.Context, slot *codexstore.Slot, force bool) codexRefreshOutcome {
	doc, err := a.Store.ReadCredential(slot.N)
	if err != nil {
		return codexRefreshOutcome{Err: err}
	}
	if doc.APIKey != "" || (!force && !doc.AccessExpired(codexRefreshThreshold())) {
		return codexRefreshOutcome{Doc: doc, Skipped: true}
	}
	home, err := os.MkdirTemp("", fmt.Sprintf("cas-codex-refresh-%d-*", slot.N))
	if err != nil {
		return codexRefreshOutcome{Doc: doc, Err: err}
	}
	defer os.RemoveAll(home)
	target := &codextarget.Target{Home: home, AuthPath: filepath.Join(home, "auth.json")}
	if err := target.Write(doc); err != nil {
		return codexRefreshOutcome{Doc: doc, Err: err}
	}
	account, err := (codexappserver.Client{Bin: codexBinOverride(), Home: home}).Refresh(ctx)
	if err != nil {
		return codexRefreshOutcome{Doc: doc, Err: err}
	}
	if !codexAccountMatches(slot, account) {
		return codexRefreshOutcome{Doc: doc, Err: fmt.Errorf("refreshed credential belongs to %s", codexAccountDescription(account))}
	}
	updated, err := target.Read()
	if err != nil {
		return codexRefreshOutcome{Doc: doc, Err: err}
	}
	if updated.Identity() != doc.Identity() {
		return codexRefreshOutcome{Doc: doc, Err: errors.New("Codex refresh changed the account identity")}
	}
	if err := a.Store.WriteCredential(slot, updated); err != nil {
		return codexRefreshOutcome{Doc: doc, Err: err}
	}
	changed := updated.Fingerprint() != doc.Fingerprint()
	slot.Apply(updated)
	if account.PlanType != "" {
		slot.Plan = account.PlanType
	}
	if slot.N == a.State.ActiveSlot {
		if err := a.Target.Write(updated); err != nil {
			return codexRefreshOutcome{Doc: updated, Changed: changed, Err: err}
		}
	}
	return codexRefreshOutcome{Doc: updated, Changed: changed}
}

func resolveCodexSlots(state *codexstore.State, refs []string) ([]*codexstore.Slot, error) {
	if len(refs) == 0 {
		return state.Slots, nil
	}
	slots := make([]*codexstore.Slot, 0, len(refs))
	for _, ref := range refs {
		slot, err := state.Resolve(ref)
		if err != nil {
			return nil, err
		}
		slots = append(slots, slot)
	}
	return slots, nil
}

func codexRefreshThreshold() time.Duration {
	value := strings.TrimSpace(os.Getenv("CAS_CODEX_REFRESH_THRESHOLD"))
	if value == "" {
		return DefaultRefreshThreshold
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < 0 {
		ui.Warnf("ignoring invalid CAS_CODEX_REFRESH_THRESHOLD=%q", value)
		return DefaultRefreshThreshold
	}
	return duration
}

func codexBinOverride() string { return strings.TrimSpace(os.Getenv("CAS_CODEX_BIN")) }
