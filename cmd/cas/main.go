// Command cas switches between Claude Code login accounts.
//
//	cas list             show the registered accounts
//	cas login            register another account
//	cas switch <n>       make one of them the account Claude Code uses
//	cas clean            drop accounts whose credentials cannot be revived
//	cas refresh          rotate every account's access token
//	cas sessions         list the running Claude Code sessions
//	cas reap             close the idle ones that keep cycling the credential
package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/Willhong/claude-account-switch/internal/app"
	"github.com/Willhong/claude-account-switch/internal/ui"
)

type command struct {
	name    string
	aliases []string
	summary string
	run     func([]string) error
}

var commands = []command{
	{"list", []string{"ls"}, "show every registered account and its token expiry", app.CmdList},
	{"login", nil, "sign in to another account and register it in a new slot", app.CmdLogin},
	{"adopt", []string{"import"}, "register the account Claude Code is already signed in as", app.CmdAdopt},
	{"switch", []string{"use"}, "make a slot the account Claude Code uses", app.CmdSwitch},
	{"current", []string{"who"}, "show which account is active right now", app.CmdCurrent},
	{"limit", []string{"limits", "usage"}, "show each account's 5h, weekly and per-model usage", app.CmdLimit},
	{"refresh", nil, "rotate access tokens so parked accounts stay usable", app.CmdRefresh},
	{"clean", nil, "remove accounts whose credentials have expired for good", app.CmdClean},
	{"remove", []string{"rm", "forget"}, "forget a slot without touching the account", app.CmdRemove},
	{"label", []string{"rename"}, "give a slot a short name", app.CmdLabel},
	{"sessions", []string{"ps"}, "list the running Claude Code sessions and how idle they are", app.CmdSessions},
	{"reap", []string{"kill"}, "close idle sessions that keep cycling the credential", app.CmdReap},
	{"daemon", nil, "manage the background refresh and reap agent", app.CmdDaemon},
	{"doctor", nil, "report where cas reads and writes, and what looks wrong", app.CmdDoctor},
	{"version", nil, "print the cas version", app.CmdVersion},
}

func main() {
	if runtime.GOOS != "darwin" {
		ui.Errorf("cas needs the macOS Keychain; it does not run on %s yet.", runtime.GOOS)
		os.Exit(1)
	}

	args := os.Args[1:]
	if len(args) == 0 {
		usage(os.Stdout)
		os.Exit(0)
	}

	name := args[0]
	switch name {
	case "-h", "--help", "help":
		usage(os.Stdout)
		return
	case "-v", "--version":
		name = "version"
	}

	for _, c := range commands {
		if c.name != name && !contains(c.aliases, name) {
			continue
		}
		if err := c.run(args[1:]); err != nil {
			// flag.ContinueOnError has already printed its own message.
			if !strings.HasPrefix(err.Error(), "flag provided but not defined") &&
				err.Error() != "flag: help requested" {
				ui.Errorf("%v", err)
			}
			os.Exit(1)
		}
		return
	}

	ui.Errorf("unknown command %q", name)
	fmt.Fprintln(os.Stderr)
	usage(os.Stderr)
	os.Exit(1)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func usage(w *os.File) {
	fmt.Fprintf(w, `cas — Claude Account Switch %s

Switch between several Claude Code logins without signing in and out.
Every registered account's token is kept refreshed in the background, so a
switch never lands on a dead session.

Usage:
  cas <command> [flags]

Commands:
`, app.Version)
	for _, c := range commands {
		name := c.name
		if len(c.aliases) > 0 {
			name += ", " + strings.Join(c.aliases, ", ")
		}
		fmt.Fprintf(w, "  %-22s %s\n", name, c.summary)
	}
	fmt.Fprintf(w, `
Getting started:
  cas adopt              register the login you already have
  cas login              add a second account
  cas list               see both, with their token expiries
  cas limit              see how much of each account's quota is spent
  cas switch 2           hand the second one to Claude Code
  cas daemon install     keep every account's token alive in the background
  cas sessions           see which Claude Code sessions are still running
  cas reap               close the idle ones before they undo a switch
  cas daemon install --reap stale
                         let the agent do that sweep every 30 minutes

Run `+"`cas <command> -h`"+` for a command's flags.

Environment:
  CAS_HOME               where cas keeps its state (default ~/.cas)
  CAS_REFRESH_THRESHOLD  how close to expiry a token is refreshed (default 45m)
  CAS_REAP_IDLE          how long untouched a session must be to be reaped (default 2h)
  CAS_KEYCHAIN_SERVICE   the Claude Code keychain item to target
  CLAUDE_CONFIG_DIR      honoured exactly as Claude Code honours it
`)
}
