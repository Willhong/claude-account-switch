package app

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Willhong/claude-account-switch/internal/codexappserver"
	"github.com/Willhong/claude-account-switch/internal/codexstore"
	"github.com/Willhong/claude-account-switch/internal/codextarget"
	"github.com/Willhong/claude-account-switch/internal/ui"
)

func cmdCodexDoctor(args []string) error {
	fs := flag.NewFlagSet("cas codex doctor", flag.ContinueOnError)
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}
	a, err := newCodexApp()
	if err != nil {
		return err
	}
	defer a.Close()
	changed := a.SyncActive()
	if changed {
		if err := a.Save(); err != nil {
			return err
		}
	}

	configPath := filepath.Join(a.Target.Home, "config.toml")
	storeMode := codexCredentialStoreMode(configPath)
	storeDisplay := storeMode
	if storeDisplay == "" {
		if _, err := os.Stat(a.Target.AuthPath); err == nil {
			storeDisplay = "default (auth.json active)"
		} else {
			storeDisplay = "default (no auth.json)"
		}
	}
	fmt.Println(ui.Bold("Paths"))
	fmt.Printf("  cas home            %s\n", a.Store.Dir())
	fmt.Printf("  CODEX_HOME          %s\n", a.Target.Home)
	fmt.Printf("  auth file           %s %s\n", a.Target.AuthPath, existsNote(a.Target.AuthPath))
	fmt.Printf("  config              %s %s\n", configPath, existsNote(configPath))
	fmt.Printf("  credential store    %s\n", storeDisplay)
	if storeMode != "" && storeMode != "file" {
		ui.Warnf("cli_auth_credentials_store=%q can make Codex ignore auth.json; account switching requires file storage.", storeMode)
	}

	fmt.Println("\n" + ui.Bold("Codex executable"))
	bin := codexBinOverride()
	if bin == "" {
		bin, _ = exec.LookPath("codex")
	}
	if bin == "" {
		fmt.Printf("  %s\n", ui.Red("not found on PATH"))
	} else {
		fmt.Printf("  path                %s\n", bin)
		if output, err := exec.Command(bin, "--version").Output(); err == nil {
			fmt.Printf("  version             %s\n", strings.TrimSpace(string(output)))
		}
	}

	fmt.Println("\n" + ui.Bold("Live credential"))
	live, liveErr := a.Target.Read()
	switch {
	case errors.Is(liveErr, codextarget.ErrNoCredential):
		fmt.Printf("  %s\n", ui.Yellow("no file-backed credential"))
	case liveErr != nil:
		fmt.Printf("  %s %v\n", ui.Red("unreadable:"), liveErr)
	default:
		fmt.Printf("  account             %s\n", live.DisplayAccount())
		fmt.Printf("  method              %s\n", live.AuthMode)
		fmt.Printf("  plan                %s\n", codexValueOrDash(live.Plan))
		if !live.ExpiresAt.IsZero() {
			fmt.Printf("  access token        %s\n", ui.RelativeTime(live.ExpiresAt))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		account, verifyErr := (codexappserver.Client{Bin: bin, Home: a.Target.Home}).ReadAccount(ctx)
		cancel()
		if verifyErr != nil {
			fmt.Printf("  fresh process       %s %v\n", ui.Yellow("unverified:"), verifyErr)
		} else if active := a.State.Active(); active != nil && codexAccountMatches(active, account) {
			fmt.Printf("  fresh process       %s (%s)\n", ui.Green("matches active slot"), codexAccountDescription(account))
		} else {
			fmt.Printf("  fresh process       %s (%s)\n", ui.Yellow("not matched to active slot"), codexAccountDescription(account))
		}
	}

	fmt.Println("\n" + ui.Bold("Slots"))
	fmt.Printf("  registered          %d\n", len(a.State.Slots))
	if active := a.State.Active(); active != nil {
		fmt.Printf("  active              Codex slot %d (%s)\n", active.N, active.Account)
	} else {
		fmt.Printf("  active              %s\n", ui.Yellow("none tracked by cas"))
	}
	var missing []int
	for _, slot := range a.State.Slots {
		if !a.Store.HasCredential(slot.N) {
			missing = append(missing, slot.N)
		}
	}
	if len(missing) > 0 {
		ui.Warnf("Codex slots %v have no Keychain manifest; run `cas codex clean`.", missing)
	}
	fmt.Printf("  Keychain service    %s\n", codexstore.SlotService)

	fmt.Println("\n" + ui.Bold("Runtime scope"))
	if os.Getenv("CODEX_POOL_AUTH_TOKEN") != "" {
		fmt.Printf("  BB account pool     %s\n", ui.Yellow("active; this thread ignores auth.json switches"))
	} else {
		fmt.Printf("  BB account pool     %s\n", ui.Dim("not detected"))
	}
	found, sessionErr := a.sessions()
	if sessionErr != nil {
		fmt.Printf("  sessions            %s %v\n", ui.Yellow("unavailable:"), sessionErr)
	} else {
		var cli, managed int
		for _, process := range found {
			if process.AppServer {
				managed++
			} else {
				cli++
			}
		}
		fmt.Printf("  standalone CLI      %d\n", cli)
		fmt.Printf("  managed app-server  %d %s\n", managed, ui.Dim("(protected from reap)"))
	}

	fmt.Println("\n" + ui.Bold("Background refresh"))
	return codexDaemonStatus()
}

func codexCredentialStoreMode(configPath string) string {
	file, err := os.Open(configPath)
	if err != nil {
		return ""
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(strings.SplitN(scanner.Text(), "#", 2)[0])
		if strings.HasPrefix(line, "[") {
			return ""
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "cli_auth_credentials_store" {
			continue
		}
		value = strings.TrimSpace(value)
		if unquoted, err := strconv.Unquote(value); err == nil {
			return unquoted
		}
		return strings.Trim(value, `"'`)
	}
	return ""
}
