package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Willhong/claude-account-switch/internal/codexappserver"
	"github.com/Willhong/claude-account-switch/internal/codexauth"
	"github.com/Willhong/claude-account-switch/internal/codexstore"
	"github.com/Willhong/claude-account-switch/internal/codextarget"
	"github.com/Willhong/claude-account-switch/internal/ui"
)

type codexCommand struct {
	name    string
	aliases []string
	summary string
	run     func([]string) error
}

var codexCommands = []codexCommand{
	{"list", []string{"ls"}, "show every registered Codex account", cmdCodexList},
	{"login", nil, "run Codex login and register the resulting account", cmdCodexLogin},
	{"adopt", []string{"import"}, "register the account Codex is already using", cmdCodexAdopt},
	{"switch", []string{"use"}, "make a slot the account Codex uses", cmdCodexSwitch},
	{"current", []string{"who"}, "show which Codex account is active", cmdCodexCurrent},
	{"limit", []string{"limits", "usage"}, "show each Codex account's current usage limits", cmdCodexLimit},
	{"refresh", nil, "refresh parked Codex ChatGPT credentials", cmdCodexRefresh},
	{"clean", nil, "remove Codex slots whose credentials are definitely dead", cmdCodexClean},
	{"sessions", []string{"ps"}, "list running Codex CLI and app-server processes", cmdCodexSessions},
	{"reap", []string{"kill"}, "close idle standalone Codex CLI sessions", cmdCodexReap},
	{"daemon", nil, "manage the Codex background refresh agent", cmdCodexDaemon},
	{"doctor", nil, "diagnose Codex paths, credentials, sessions, and daemon", cmdCodexDoctor},
	{"remove", []string{"rm", "forget"}, "forget a Codex slot without logging it out", cmdCodexRemove},
	{"label", []string{"rename"}, "give a Codex slot a short name", cmdCodexLabel},
}

// CmdCodex dispatches the Codex-specific account commands. Existing top-level
// commands retain their Claude behavior for backward compatibility.
func CmdCodex(args []string) error {
	if len(args) == 0 {
		codexUsage(os.Stdout)
		return nil
	}
	name := args[0]
	if name == "-h" || name == "--help" || name == "help" {
		codexUsage(os.Stdout)
		return nil
	}
	for _, command := range codexCommands {
		if command.name == name || stringSliceContains(command.aliases, name) {
			return command.run(args[1:])
		}
	}
	return fmt.Errorf("unknown Codex command %q; run `cas codex help`", name)
}

func stringSliceContains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func codexUsage(w *os.File) {
	fmt.Fprintln(w, `cas codex — switch between Codex CLI accounts

Usage:
  cas codex <command> [flags]

Commands:`)
	for _, command := range codexCommands {
		name := command.name
		if len(command.aliases) > 0 {
			name += ", " + strings.Join(command.aliases, ", ")
		}
		fmt.Fprintf(w, "  %-22s %s\n", name, command.summary)
	}
	fmt.Fprintln(w, `
Getting started:
  cas codex adopt              register the current Codex login
  cas codex login              add another ChatGPT account
  cas codex list               see all Codex slots
  cas codex switch 2           activate the second account

Codex switching requires file-backed credentials in CODEX_HOME/auth.json.`)
}

type codexApp struct {
	Store  *codexstore.Store
	State  *codexstore.State
	Target *codextarget.Target

	unlock func()
}

func newCodexApp() (*codexApp, error) {
	store, err := codexstore.Open()
	if err != nil {
		return nil, err
	}
	unlock, err := store.Lock()
	if err != nil {
		return nil, err
	}
	state, err := store.Load()
	if err != nil {
		unlock()
		return nil, err
	}
	target, err := codextarget.Detect()
	if err != nil {
		unlock()
		return nil, err
	}
	return &codexApp{Store: store, State: state, Target: target, unlock: unlock}, nil
}

func (a *codexApp) Close() {
	if a.unlock != nil {
		a.unlock()
		a.unlock = nil
	}
}

func (a *codexApp) Save() error { return a.Store.Save(a.State) }

// SyncActive saves tokens Codex refreshed while a slot was active and follows
// the live file if another known account replaced it.
func (a *codexApp) SyncActive() bool {
	live, err := a.Target.Read()
	if err != nil || len(a.State.Slots) == 0 {
		return false
	}
	if active := a.State.Active(); active != nil && active.Identity == live.Identity() {
		if active.CredentialTag == live.Fingerprint() {
			return false
		}
		if err := a.Store.WriteCredential(active, live); err != nil {
			ui.Warnf("could not sync Codex slot %d from the live credential: %v", active.N, err)
			return false
		}
		active.Apply(live)
		return true
	}
	if slot := a.State.FindByIdentity(live.Identity()); slot != nil {
		if err := a.Store.WriteCredential(slot, live); err != nil {
			ui.Warnf("could not sync Codex slot %d from the live credential: %v", slot.N, err)
			return false
		}
		slot.Apply(live)
		a.State.ActiveSlot = slot.N
		a.State.SwitchedAt = time.Now()
		return true
	}
	if a.State.ActiveSlot != 0 {
		a.State.ActiveSlot = 0
		return true
	}
	return false
}

func (a *codexApp) registerLive(doc *codexauth.Document, label string) (*codexstore.Slot, bool, error) {
	slot := a.State.FindByIdentity(doc.Identity())
	isNew := slot == nil
	if isNew {
		slot = a.State.Add(&codexstore.Slot{})
	}
	if label != "" {
		slot.Label = label
	}
	slot.Apply(doc)
	if err := a.Store.WriteCredential(slot, doc); err != nil {
		if isNew {
			a.State.Remove(slot.N)
		}
		return nil, false, err
	}
	a.State.ActiveSlot = slot.N
	a.State.SwitchedAt = time.Now()
	return slot, isNew, nil
}

func cmdCodexAdopt(args []string) error {
	fs := flag.NewFlagSet("cas codex adopt", flag.ContinueOnError)
	label := fs.String("label", "", "short name for the Codex slot")
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}
	a, err := newCodexApp()
	if err != nil {
		return err
	}
	defer a.Close()

	doc, err := a.Target.Read()
	if err != nil {
		if errors.Is(err, codextarget.ErrNoCredential) {
			return codexFileCredentialError("run `codex login` first")
		}
		return err
	}
	slot, isNew, err := a.registerLive(doc, *label)
	if err != nil {
		return err
	}
	if err := a.Save(); err != nil {
		return err
	}
	if isNew {
		ui.OKf("Registered %s as Codex slot %d.", ui.Bold(slot.Account), slot.N)
	} else {
		ui.OKf("Updated Codex slot %d (%s).", slot.N, ui.Bold(slot.Account))
	}
	return nil
}

func cmdCodexLogin(args []string) error {
	fs := flag.NewFlagSet("cas codex login", flag.ContinueOnError)
	label := fs.String("label", "", "short name for the new Codex slot")
	withAPIKey := fs.Bool("with-api-key", false, "read an OpenAI API key from stdin")
	withAccessToken := fs.Bool("with-access-token", false, "read a Codex access token from stdin")
	deviceAuth := fs.Bool("device-auth", false, "use the Codex device authorization flow")
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}
	modes := 0
	for _, enabled := range []bool{*withAPIKey, *withAccessToken, *deviceAuth} {
		if enabled {
			modes++
		}
	}
	if modes > 1 {
		return errors.New("choose only one of --with-api-key, --with-access-token, or --device-auth")
	}

	a, err := newCodexApp()
	if err != nil {
		return err
	}
	defer a.Close()
	beforeFingerprint := ""
	beforeModTime := time.Time{}
	if before, readErr := a.Target.Read(); readErr == nil {
		beforeFingerprint = before.Fingerprint()
		if info, statErr := os.Stat(a.Target.AuthPath); statErr == nil {
			beforeModTime = info.ModTime()
		}
	}

	if a.SyncActive() {
		if err := a.Save(); err != nil {
			return err
		}
	}
	// Preserve an untracked current login before Codex replaces auth.json.
	if live, readErr := a.Target.Read(); readErr == nil && a.State.FindByIdentity(live.Identity()) == nil {
		slot, _, registerErr := a.registerLive(live, "")
		if registerErr != nil {
			return fmt.Errorf("preserve the current Codex login: %w", registerErr)
		}
		if err := a.Save(); err != nil {
			return err
		}
		ui.Infof("Saved the current login as Codex slot %d before signing in.", slot.N)
	}

	codexBin, err := exec.LookPath("codex")
	if err != nil {
		return errors.New("could not find `codex` on PATH")
	}
	loginArgs := []string{"login"}
	switch {
	case *withAPIKey:
		loginArgs = append(loginArgs, "--with-api-key")
	case *withAccessToken:
		loginArgs = append(loginArgs, "--with-access-token")
	case *deviceAuth:
		loginArgs = append(loginArgs, "--device-auth")
	}
	command := exec.Command(codexBin, loginArgs...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("Codex login failed: %w", err)
	}

	doc, err := a.Target.Read()
	if err != nil {
		if errors.Is(err, codextarget.ErrNoCredential) {
			return codexFileCredentialError("Codex login completed but did not write auth.json")
		}
		return err
	}
	if beforeFingerprint != "" && doc.Fingerprint() == beforeFingerprint {
		if info, statErr := os.Stat(a.Target.AuthPath); statErr == nil && info.ModTime().Equal(beforeModTime) {
			return codexFileCredentialError("Codex login did not update auth.json")
		}
	}
	slot, isNew, err := a.registerLive(doc, *label)
	if err != nil {
		return err
	}
	if err := a.Save(); err != nil {
		return err
	}
	if isNew {
		ui.OKf("Registered %s as Codex slot %d.", ui.Bold(slot.Account), slot.N)
	} else {
		ui.OKf("Refreshed Codex slot %d (%s).", slot.N, ui.Bold(slot.Account))
	}
	return nil
}

func cmdCodexList(args []string) error {
	fs := flag.NewFlagSet("cas codex list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print Codex slots as JSON")
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
	if *asJSON {
		return writeJSON(os.Stdout, a.State)
	}
	if len(a.State.Slots) == 0 {
		fmt.Fprintln(os.Stdout, "No Codex accounts registered yet.")
		if _, err := a.Target.Read(); err == nil {
			fmt.Fprintln(os.Stdout, "\nCodex is signed in. Run `cas codex adopt` to register it as slot 1.")
		} else {
			fmt.Fprintln(os.Stdout, "\nRun `cas codex login` to sign in and register one.")
		}
		return nil
	}

	table := &ui.Table{Header: []string{"", "SLOT", "ACCOUNT", "PLAN", "METHOD", "ACCESS TOKEN", "LABEL"}}
	for _, slot := range a.State.Slots {
		marker := " "
		if slot.N == a.State.ActiveSlot {
			marker = ui.Green("*")
		}
		table.Rows = append(table.Rows, []string{
			marker,
			fmt.Sprintf("%d", slot.N),
			slot.Account,
			codexValueOrDash(slot.Plan),
			slot.AuthMode,
			codexAccessCell(slot),
			slot.Label,
		})
	}
	table.Render(os.Stdout)
	if a.State.ActiveSlot == 0 {
		fmt.Fprintln(os.Stdout)
		ui.Infof("The live Codex login is not registered. Run `cas codex adopt` to keep it.")
	}
	return nil
}

func cmdCodexCurrent(args []string) error {
	fs := flag.NewFlagSet("cas codex current", flag.ContinueOnError)
	quiet := fs.Bool("q", false, "print only the account")
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
	slot := a.State.Active()
	if slot == nil {
		live, readErr := a.Target.Read()
		if readErr == nil {
			if *quiet {
				fmt.Println(live.DisplayAccount())
				return nil
			}
			fmt.Printf("%s (not registered with cas; run `cas codex adopt`)\n", live.DisplayAccount())
			return nil
		}
		if errors.Is(readErr, codextarget.ErrNoCredential) {
			return codexFileCredentialError("Codex is not signed in")
		}
		return readErr
	}
	if *quiet {
		fmt.Println(slot.Account)
		return nil
	}
	fmt.Printf("slot %d  %s\n", slot.N, ui.Bold(slot.Account))
	if slot.Label != "" {
		fmt.Printf("  label          %s\n", slot.Label)
	}
	if slot.Plan != "" {
		fmt.Printf("  plan           %s\n", slot.Plan)
	}
	fmt.Printf("  method         %s\n", slot.AuthMode)
	if !slot.ExpiresAt.IsZero() {
		fmt.Printf("  access token   %s\n", ui.RelativeTime(slot.ExpiresAt))
	}
	return nil
}

func cmdCodexSwitch(args []string) error {
	fs := flag.NewFlagSet("cas codex switch", flag.ContinueOnError)
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: cas codex switch <slot|label|email>")
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
	slot, err := a.State.Resolve(positional[0])
	if err != nil {
		return err
	}
	if slot.N == a.State.ActiveSlot {
		ui.Infof("The file-backed Codex login is already on slot %d (%s).", slot.N, slot.Account)
		warnCodexRuntimeScope()
		return nil
	}
	doc, err := a.Store.ReadCredential(slot.N)
	if err != nil {
		return err
	}
	previous, _ := a.Target.Read()
	if err := a.Target.Write(doc); err != nil {
		return fmt.Errorf("install Codex slot %d: %w", slot.N, err)
	}
	verifyCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	account, verifyErr := (codexappserver.Client{Bin: codexBinOverride(), Home: a.Target.Home}).ReadAccount(verifyCtx)
	cancel()
	if verifyErr == nil && !codexAccountMatches(slot, account) {
		if previous != nil {
			_ = a.Target.Write(previous)
		}
		return fmt.Errorf("Codex still sees %s after auth.json was replaced; its effective credential store is not file-backed", codexAccountDescription(account))
	}
	a.State.ActiveSlot = slot.N
	a.State.SwitchedAt = time.Now()
	if live, readErr := a.Target.Read(); readErr == nil && live.Identity() == slot.Identity && live.Fingerprint() != doc.Fingerprint() {
		if writeErr := a.Store.WriteCredential(slot, live); writeErr == nil {
			slot.Apply(live)
		}
	}
	if err := a.Save(); err != nil {
		return err
	}
	ui.OKf("Switched Codex to slot %d — %s", slot.N, ui.Bold(slot.Account))
	if verifyErr != nil {
		ui.Warnf("the credential file changed, but a fresh Codex process could not verify it: %v", verifyErr)
	} else {
		ui.Infof("Verified that a fresh Codex process sees %s.", codexAccountDescription(account))
	}
	warnCodexRuntimeScope()
	return nil
}

func warnCodexRuntimeScope() {
	ui.Warnf("already-running Codex sessions keep their previous account; restart those sessions to apply the switch.")
	if os.Getenv("CODEX_POOL_AUTH_TOKEN") != "" {
		ui.Warnf("this BB session uses the independent bb-account-pool credential, so switching auth.json does not change the account for this thread.")
	}
}

func codexAccountMatches(slot *codexstore.Slot, account codexappserver.Account) bool {
	mode := strings.ToLower(slot.AuthMode)
	if strings.Contains(mode, "api") {
		return account.Type == "apiKey"
	}
	if account.Type != "chatgpt" {
		return false
	}
	if slot.Email == "" || account.Email == "" {
		return true
	}
	return strings.EqualFold(slot.Email, account.Email)
}

func codexAccountDescription(account codexappserver.Account) string {
	if account.Email != "" {
		return account.Email
	}
	if account.Type != "" {
		return account.Type
	}
	return "another account"
}

func cmdCodexRemove(args []string) error {
	fs := flag.NewFlagSet("cas codex remove", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: cas codex remove <slot|label|email>")
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
	slot, err := a.State.Resolve(positional[0])
	if err != nil {
		return err
	}
	if !*yes {
		fmt.Fprintf(os.Stderr, "Remove Codex slot %d (%s)? [y/N] ", slot.N, slot.Account)
		if !confirmed() {
			ui.Infof("Nothing removed.")
			return nil
		}
	}
	wasActive := a.State.ActiveSlot == slot.N
	if err := a.Store.DeleteCredential(slot.N); err != nil {
		ui.Warnf("could not delete Codex slot %d's Keychain item: %v", slot.N, err)
	}
	a.State.Remove(slot.N)
	if err := a.Save(); err != nil {
		return err
	}
	ui.OKf("Removed Codex slot %d (%s).", slot.N, slot.Account)
	if wasActive {
		ui.Warnf("Codex still holds that live credential. Run `cas codex switch <n>` to activate another slot.")
	}
	return nil
}

func cmdCodexLabel(args []string) error {
	fs := flag.NewFlagSet("cas codex label", flag.ContinueOnError)
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		return errors.New("usage: cas codex label <slot|label|email> <new-label>")
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
	slot, err := a.State.Resolve(positional[0])
	if err != nil {
		return err
	}
	slot.Label = positional[1]
	if doc, err := a.Store.ReadCredential(slot.N); err == nil {
		if err := a.Store.WriteCredential(slot, doc); err != nil {
			ui.Warnf("could not rename Codex slot %d's Keychain item: %v", slot.N, err)
		}
	}
	if err := a.Save(); err != nil {
		return err
	}
	ui.OKf("Codex slot %d is now labelled %q.", slot.N, slot.Label)
	return nil
}

func codexFileCredentialError(detail string) error {
	return fmt.Errorf("%s; cas currently needs Codex file storage at CODEX_HOME/auth.json (set cli_auth_credentials_store = \"file\")", detail)
}

func codexValueOrDash(value string) string {
	if value == "" {
		return ui.Dim("—")
	}
	return value
}

func codexAccessCell(slot *codexstore.Slot) string {
	if slot.AuthMode == "apikey" || slot.AuthMode == "api_key" {
		return ui.Dim("API key")
	}
	if slot.ExpiresAt.IsZero() {
		return ui.Dim("unknown")
	}
	text := ui.RelativeTime(slot.ExpiresAt)
	if time.Until(slot.ExpiresAt) <= 0 {
		return ui.Yellow(text)
	}
	return ui.Green(text)
}

// writeJSON lives here to keep cmd_list.go's concrete encoder private to the
// Claude command while sharing its stable pretty-printing behavior.
func writeJSON(file *os.File, value any) error {
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
