package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Willhong/claude-account-switch/internal/store"
	"github.com/Willhong/claude-account-switch/internal/target"
	"github.com/Willhong/claude-account-switch/internal/ui"
)

// CmdList prints the slot table.
func CmdList(args []string) error {
	fs := flag.NewFlagSet("cas list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the slot table as JSON")
	noRefresh := fs.Bool("no-refresh", false, "skip the opportunistic token refresh")
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}

	a, err := New()
	if err != nil {
		return err
	}
	defer a.Close()

	changed := a.SyncActive()
	if a.AutoRefresh(context.Background(), !*noRefresh) {
		changed = true
	}
	if changed {
		if err := a.Save(); err != nil {
			return err
		}
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(a.State)
	}

	if len(a.State.Slots) == 0 {
		fmt.Fprintln(os.Stdout, "No accounts registered yet.")
		if _, err := a.Target.ReadCred(); err == nil {
			fmt.Fprintln(os.Stdout, "\nClaude Code is signed in. Run `cas adopt` to register that login as slot 1.")
		} else {
			fmt.Fprintln(os.Stdout, "\nRun `cas login` to register one.")
		}
		return nil
	}

	t := &ui.Table{Header: []string{"", "SLOT", "EMAIL", "PLAN", "ACCESS TOKEN", "REFRESH TOKEN", "LABEL"}}
	for _, s := range a.State.Slots {
		marker := " "
		if s.N == a.State.ActiveSlot {
			marker = ui.Green("*")
		}
		t.Rows = append(t.Rows, []string{
			marker,
			fmt.Sprintf("%d", s.N),
			s.Email,
			planLabel(s),
			accessCell(s),
			refreshCell(s),
			s.Label,
		})
	}
	t.Render(os.Stdout)

	if a.State.ActiveSlot == 0 {
		fmt.Fprintln(os.Stdout)
		if _, err := a.Target.ReadCred(); errors.Is(err, target.ErrNoCredential) {
			ui.Infof("Claude Code has no credential stored. Run `cas switch <n>` to install one.")
		} else {
			ui.Infof("Claude Code is signed in to an account cas does not track. Run `cas adopt` to register it.")
		}
	}
	for _, s := range a.State.Slots {
		if s.Revoked {
			ui.Warnf("slot %d (%s) needs a fresh login: %s", s.N, s.Name(), s.LastError)
		}
	}
	return nil
}

func planLabel(s *store.Slot) string {
	if s.SubscriptionType == "" {
		return ui.Dim("—")
	}
	return s.SubscriptionType
}

func accessCell(s *store.Slot) string {
	if s.Revoked {
		return ui.Red("revoked")
	}
	t := s.AccessExpiresAt()
	text := ui.RelativeTime(t)
	switch {
	case t.IsZero():
		return ui.Dim(text)
	case time.Until(t) <= 0:
		return ui.Yellow(text)
	default:
		return ui.Green(text)
	}
}

func refreshCell(s *store.Slot) string {
	t := s.RefreshExpiresAt()
	text := ui.RelativeTime(t)
	switch {
	case t.IsZero():
		return ui.Dim(text)
	case time.Until(t) <= 0:
		return ui.Red(text)
	case time.Until(t) < 72*time.Hour:
		return ui.Yellow(text)
	default:
		return text
	}
}

// CmdCurrent prints the account Claude Code is using right now.
func CmdCurrent(args []string) error {
	fs := flag.NewFlagSet("cas current", flag.ContinueOnError)
	quiet := fs.Bool("q", false, "print only the email")
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}

	a, err := New()
	if err != nil {
		return err
	}
	defer a.Close()

	if a.SyncActive() {
		if err := a.Save(); err != nil {
			return err
		}
	}

	active := a.State.Active()
	if active == nil {
		raw, _ := a.Target.ReadOAuthAccount()
		if email := target.AccountEmail(raw); email != "" {
			if *quiet {
				fmt.Println(email)
				return nil
			}
			fmt.Printf("%s (not registered with cas — run `cas adopt`)\n", email)
			return nil
		}
		return errors.New("Claude Code has no account signed in")
	}
	if *quiet {
		fmt.Println(active.Email)
		return nil
	}

	fmt.Printf("slot %d  %s\n", active.N, ui.Bold(active.Email))
	if active.Label != "" {
		fmt.Printf("  label          %s\n", active.Label)
	}
	if active.OrgName != "" {
		fmt.Printf("  organization   %s\n", active.OrgName)
	}
	if active.SubscriptionType != "" {
		fmt.Printf("  plan           %s\n", active.SubscriptionType)
	}
	if active.RateLimitTier != "" {
		fmt.Printf("  rate limit     %s\n", active.RateLimitTier)
	}
	fmt.Printf("  access token   %s\n", ui.RelativeTime(active.AccessExpiresAt()))
	fmt.Printf("  refresh token  %s\n", ui.RelativeTime(active.RefreshExpiresAt()))
	return nil
}
