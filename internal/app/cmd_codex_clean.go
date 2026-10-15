package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Willhong/claude-account-switch/internal/codexstore"
	"github.com/Willhong/claude-account-switch/internal/ui"
)

func cmdCodexClean(args []string) error {
	fs := flag.NewFlagSet("cas codex clean", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "report dead Codex slots without removing them")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if err := parseNoArgs(fs, args); err != nil {
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
	if len(a.State.Slots) == 0 {
		ui.Infof("No Codex accounts registered.")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ui.Infof("Checking %d Codex account(s)…", len(a.State.Slots))
	var dead []*codexstore.Slot
	for _, slot := range a.State.Slots {
		doc, readErr := a.Store.ReadCredential(slot.N)
		if readErr != nil {
			dead = append(dead, slot)
			fmt.Printf("  %s Codex slot %d  %-32s %s\n", ui.Red("✗"), slot.N, slot.Account, ui.Dim("credential missing from Keychain"))
			continue
		}
		if doc.APIKey != "" {
			fmt.Printf("  %s Codex slot %d  %-32s %s\n", ui.Green("✓"), slot.N, slot.Account, ui.Dim("API key stored; remote validity not probed"))
			continue
		}
		outcome := a.refreshCodexSlot(ctx, slot, true)
		switch {
		case outcome.Err == nil:
			fmt.Printf("  %s Codex slot %d  %-32s %s\n", ui.Green("✓"), slot.N, slot.Account, ui.Dim("credential refreshed successfully"))
		case definiteCodexAuthFailure(outcome.Err):
			dead = append(dead, slot)
			fmt.Printf("  %s Codex slot %d  %-32s %s\n", ui.Red("✗"), slot.N, slot.Account, ui.Dim(outcome.Err.Error()))
		default:
			fmt.Printf("  %s Codex slot %d  %-32s %s\n", ui.Yellow("?"), slot.N, slot.Account, ui.Dim("could not verify: "+outcome.Err.Error()))
		}
	}

	if len(dead) == 0 {
		if err := a.Save(); err != nil {
			return err
		}
		ui.OKf("No definitely dead Codex credentials were found.")
		return nil
	}
	if *dryRun {
		ui.Infof("%d Codex account(s) would be removed. Re-run without --dry-run to apply.", len(dead))
		return a.Save()
	}
	if !*yes {
		names := make([]string, 0, len(dead))
		for _, slot := range dead {
			names = append(names, fmt.Sprintf("%d (%s)", slot.N, slot.Account))
		}
		fmt.Fprintf(os.Stderr, "\nRemove %d Codex account(s): %s? [y/N] ", len(dead), strings.Join(names, ", "))
		if !confirmed() {
			ui.Infof("Nothing removed.")
			return a.Save()
		}
	}
	for _, slot := range dead {
		wasActive := a.State.ActiveSlot == slot.N
		if err := a.Store.DeleteCredential(slot.N); err != nil {
			ui.Warnf("could not delete Codex slot %d's Keychain items: %v", slot.N, err)
		}
		a.State.Remove(slot.N)
		ui.OKf("Removed Codex slot %d (%s).", slot.N, slot.Account)
		if wasActive {
			ui.Warnf("that was the active file-backed account; run `cas codex switch <n>` or `cas codex login`.")
		}
	}
	return a.Save()
}

func definiteCodexAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"refresh token was rejected",
		"refresh token has expired",
		"refresh token was already used",
		"refresh token was revoked",
		"invalid_grant",
		"unauthorized",
		"not signed in",
		"no signed-in account",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return errors.Is(err, os.ErrNotExist)
}
