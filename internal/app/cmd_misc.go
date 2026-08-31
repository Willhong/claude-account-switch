package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/Willhong/claude-account-switch/internal/keychain"
	"github.com/Willhong/claude-account-switch/internal/store"
	"github.com/Willhong/claude-account-switch/internal/target"
	"github.com/Willhong/claude-account-switch/internal/ui"
)

// CmdVersion prints the build version.
func CmdVersion([]string) error {
	fmt.Printf("cas %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
	return nil
}

// CmdDoctor reports where cas is reading and writing, and flags the mismatches
// that make switching silently fail — a non-default CLAUDE_CONFIG_DIR being
// the usual one.
func CmdDoctor([]string) error {
	a, err := New()
	if err != nil {
		return err
	}
	defer a.Close()

	fmt.Println(ui.Bold("Paths"))
	fmt.Printf("  cas home            %s\n", a.Store.Dir())
	fmt.Printf("  Claude config       %s %s\n", a.Target.ConfigPath, existsNote(a.Target.ConfigPath))
	fmt.Printf("  Claude creds file   %s %s\n", a.Target.CredsPath, existsNote(a.Target.CredsPath))

	fmt.Println("\n" + ui.Bold("Keychain"))
	fmt.Printf("  live service        %s\n", a.Target.Service)
	fmt.Printf("  live account        %s\n", a.Target.Account)
	if keychain.Exists(a.Target.Service, a.Target.Account) {
		fmt.Printf("  live item           %s\n", ui.Green("present"))
	} else {
		fmt.Printf("  live item           %s\n", ui.Yellow("missing"))
	}
	fmt.Printf("  cas slot service    %s\n", store.SlotService)

	if found := keychain.ListServices("Claude Code-credentials"); len(found) > 1 {
		fmt.Println()
		ui.Warnf("several Claude Code credential items exist in your keychain:")
		for _, s := range found {
			marker := " "
			if s == a.Target.Service {
				marker = "*"
			}
			fmt.Fprintf(os.Stderr, "    %s %s\n", marker, s)
		}
		ui.Warnf("the suffixed ones belong to other CLAUDE_CONFIG_DIR setups; set CAS_KEYCHAIN_SERVICE to target one of them.")
	}

	fmt.Println("\n" + ui.Bold("Live credential"))
	env, err := a.Target.ReadCred()
	switch {
	case errors.Is(err, target.ErrNoCredential):
		fmt.Printf("  %s\n", ui.Yellow("Claude Code has no credential stored"))
	case err != nil:
		fmt.Printf("  %s %v\n", ui.Red("unreadable:"), err)
	default:
		fmt.Printf("  access token        %s\n", ui.RelativeTime(env.OAuth.ExpiresAtTime()))
		fmt.Printf("  refresh token       %s\n", ui.RelativeTime(env.OAuth.RefreshExpiresAtTime()))
		fmt.Printf("  plan                %s\n", orDash(env.OAuth.SubscriptionType))

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		v, verr := a.OAuth.Validate(ctx, env.OAuth.AccessToken)
		switch {
		case verr != nil:
			fmt.Printf("  Anthropic check     %s %v\n", ui.Yellow("unreachable:"), verr)
		case v.Valid:
			fmt.Printf("  Anthropic check     %s\n", ui.Green("token accepted"))
		default:
			fmt.Printf("  Anthropic check     %s\n", ui.Red("token rejected"))
		}
	}

	raw, _ := a.Target.ReadOAuthAccount()
	fmt.Println("\n" + ui.Bold("Claude config account"))
	if len(raw) == 0 {
		fmt.Printf("  %s\n", ui.Yellow("no oauthAccount block in "+a.Target.ConfigPath))
	} else {
		fmt.Printf("  email               %s\n", orDash(target.AccountEmail(raw)))
		fmt.Printf("  account uuid        %s\n", orDash(target.AccountUUID(raw)))
	}

	fmt.Println("\n" + ui.Bold("Slots"))
	fmt.Printf("  registered          %d\n", len(a.State.Slots))
	if active := a.State.Active(); active != nil {
		fmt.Printf("  active              %s\n", slotSummary(active))
	} else {
		fmt.Printf("  active              %s\n", ui.Yellow("none tracked by cas"))
	}
	var missing []int
	for _, s := range a.State.Slots {
		if !a.Store.HasCred(s.N) {
			missing = append(missing, s.N)
		}
	}
	if len(missing) > 0 {
		ui.Warnf("slots %v have no keychain item; run `cas clean` to drop them.", missing)
	}

	fmt.Println("\n" + ui.Bold("Background refresh"))
	if err := daemonStatus(); err != nil {
		return err
	}

	if pids := target.RunningClaudePIDs(); len(pids) > 0 {
		fmt.Println()
		ui.Infof("%d Claude Code session(s) running (pids %v) — they cache the credential in memory.", len(pids), pids)
	}
	return nil
}

func existsNote(path string) string {
	if _, err := os.Stat(path); err == nil {
		return ui.Dim("(exists)")
	}
	return ui.Dim("(absent)")
}

func orDash(s string) string {
	if s == "" {
		return ui.Dim("—")
	}
	return s
}

func secs(n int) time.Duration { return time.Duration(n) * time.Second }

func sinceMod(info os.FileInfo) time.Duration { return time.Since(info.ModTime()) }
