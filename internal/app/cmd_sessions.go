package app

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Willhong/claude-account-switch/internal/session"
	"github.com/Willhong/claude-account-switch/internal/ui"
)

// DefaultReapIdle is how long a Claude Code session has to sit untouched
// before cas treats it as abandoned. Claude Code's access tokens last about
// eight hours, so a session parked for a couple of hours is one that will
// almost certainly rotate a token before anyone types into it again.
const DefaultReapIdle = 2 * time.Hour

// ReapIdle reads CAS_REAP_IDLE, falling back to the default.
func ReapIdle() time.Duration {
	v := strings.TrimSpace(os.Getenv("CAS_REAP_IDLE"))
	if v == "" {
		return DefaultReapIdle
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		ui.Warnf("ignoring invalid CAS_REAP_IDLE=%q", v)
		return DefaultReapIdle
	}
	return d
}

// liveSession is a running Claude Code process seen through cas's eyes: it
// also knows whether the process predates the last account switch, which is
// what makes it dangerous rather than merely old.
type liveSession struct {
	*session.Session
	// Stale means the session started before cas last switched accounts, so it
	// is holding the previous account's credential in memory.
	Stale bool
	// StaleKnown is false when cas has never recorded a switch.
	StaleKnown bool
}

// sessions lists the running Claude Code processes.
func (a *App) sessions() ([]*liveSession, error) {
	found, err := session.List(session.Options{ProjectsDir: a.Target.ProjectsDir()})
	if err != nil {
		return nil, fmt.Errorf("list Claude Code processes: %w", err)
	}
	switched := a.State.SwitchedAt

	out := make([]*liveSession, 0, len(found))
	for _, s := range found {
		out = append(out, &liveSession{
			Session:    s,
			Stale:      !switched.IsZero() && s.Started.Before(switched),
			StaleKnown: !switched.IsZero(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Uptime > out[j].Uptime })
	return out, nil
}

// staleSessions counts the running sessions that predate the last switch.
func (a *App) staleSessions() []*liveSession {
	all, err := a.sessions()
	if err != nil {
		return nil
	}
	var stale []*liveSession
	for _, s := range all {
		if s.Stale && !s.Ours {
			stale = append(stale, s)
		}
	}
	return stale
}

// CmdSessions lists the Claude Code processes running on this machine.
func CmdSessions(args []string) error {
	fs := flag.NewFlagSet("cas sessions", flag.ContinueOnError)
	idle := fs.Duration("idle", ReapIdle(), "how long untouched counts as idle")
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}

	a, err := New()
	if err != nil {
		return err
	}
	defer a.Close()

	found, err := a.sessions()
	if err != nil {
		return err
	}
	if len(found) == 0 {
		ui.Infof("No Claude Code sessions are running.")
		return nil
	}

	t := &ui.Table{Header: []string{"PID", "TTY", "IDLE", "UPTIME", "CREDENTIAL", "DIRECTORY", ""}}
	for _, s := range found {
		t.Rows = append(t.Rows, []string{
			strconv.Itoa(s.PID),
			s.TTYName(),
			idleCell(s, *idle),
			ui.Duration(s.Uptime),
			credentialCell(s),
			shortenHome(s.CWD),
			note(s),
		})
	}
	t.Render(os.Stdout)

	fmt.Println()
	if active := a.State.Active(); active != nil && !a.State.SwitchedAt.IsZero() {
		ui.Infof("cas switched to slot %d (%s) %s ago.", active.N, active.Name(), ui.Duration(time.Since(a.State.SwitchedAt)))
	}
	summarise(found, *idle)
	return nil
}

// CmdReap shuts down the Claude Code sessions that are quietly cycling
// credentials in the background.
func CmdReap(args []string) error {
	fs := flag.NewFlagSet("cas reap", flag.ContinueOnError)
	idle := fs.Duration("idle", ReapIdle(), "close sessions untouched for at least this long")
	stale := fs.Bool("stale", false, "narrow the selection to sessions that predate the last account switch")
	all := fs.Bool("all", false, "ignore the idle threshold and consider every session")
	pidList := fs.String("pid", "", "close these pids only (comma separated)")
	force := fs.Bool("force", false, "send SIGKILL instead of asking the session to exit")
	dryRun := fs.Bool("dry-run", false, "report what would be closed without closing it")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}

	a, err := New()
	if err != nil {
		return err
	}
	defer a.Close()

	found, err := a.sessions()
	if err != nil {
		return err
	}
	if len(found) == 0 {
		ui.Infof("No Claude Code sessions are running.")
		return nil
	}

	explicit, err := parsePIDs(*pidList)
	if err != nil {
		return err
	}

	var doomed []*liveSession
	for _, s := range found {
		if s.Ours {
			if explicit[s.PID] {
				return fmt.Errorf("pid %d is the session cas is running inside; refusing to close it", s.PID)
			}
			continue
		}
		if len(explicit) > 0 {
			if explicit[s.PID] {
				doomed = append(doomed, s)
			}
			continue
		}
		// --all drops the idle requirement; --stale narrows what is left to the
		// sessions that are actually holding the wrong account.
		if !*all && !(s.IdleKnown && s.Idle >= *idle) {
			continue
		}
		if *stale && !s.Stale {
			continue
		}
		doomed = append(doomed, s)
	}

	if len(explicit) > 0 {
		for pid := range explicit {
			if !containsPID(doomed, pid) {
				return fmt.Errorf("pid %d is not a running Claude Code session", pid)
			}
		}
	}

	if len(doomed) == 0 {
		summarise(found, *idle)
		switch {
		case len(explicit) > 0 || *all:
			ui.OKf("Nothing to reap.")
		case *stale:
			ui.OKf("Nothing to reap: no session idle for %s predates the last switch.", ui.Duration(*idle))
		default:
			ui.OKf("Nothing to reap: no session has been idle for %s.", ui.Duration(*idle))
		}
		return nil
	}

	fmt.Println(ui.Bold("Sessions to close"))
	plan := &ui.Table{}
	for _, s := range doomed {
		plan.Rows = append(plan.Rows, []string{
			"  " + ui.Red("✗") + fmt.Sprintf(" pid %d", s.PID),
			s.TTYName(),
			"idle " + idleCell(s, *idle),
			"up " + ui.Duration(s.Uptime),
			credentialCell(s),
			ui.Dim(shortenHome(s.CWD)),
		})
	}
	plan.Render(os.Stdout)

	if *dryRun {
		ui.Infof("\n%d session(s) would be closed. Re-run without --dry-run to apply.", len(doomed))
		return nil
	}
	if !*yes {
		fmt.Fprintf(os.Stderr, "\nClose %d Claude Code session(s)? Unsaved work in them is lost. [y/N] ", len(doomed))
		if !confirmed() {
			ui.Infof("Nothing closed.")
			return nil
		}
	}

	sig := syscall.SIGTERM
	if *force {
		sig = syscall.SIGKILL
	}
	var survivors []*liveSession
	for _, s := range doomed {
		if err := s.Signal(sig); err != nil {
			ui.Warnf("could not signal pid %d: %v", s.PID, err)
			continue
		}
		survivors = append(survivors, s)
	}

	// SIGTERM lets Claude Code save its transcript on the way out, so give it a
	// moment before reporting anything as stuck.
	deadline := time.Now().Add(5 * time.Second)
	for len(survivors) > 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		var left []*liveSession
		for _, s := range survivors {
			if s.Alive() {
				left = append(left, s)
				continue
			}
			ui.OKf("Closed pid %d%s.", s.PID, whereClosed(s))
		}
		survivors = left
	}
	for _, s := range survivors {
		ui.Warnf("pid %d is still running; re-run with --force to send SIGKILL.", s.PID)
	}
	if len(survivors) < len(doomed) {
		ui.Infof("The switched-in credential is now the only one in play.")
	}
	return nil
}

func summarise(found []*liveSession, idle time.Duration) {
	var idleCount, staleCount int
	for _, s := range found {
		if s.Ours {
			continue
		}
		if s.IdleKnown && s.Idle >= idle {
			idleCount++
		}
		if s.Stale {
			staleCount++
		}
	}
	if staleCount > 0 {
		ui.Warnf("%d session(s) started before the last switch: each still holds the previous account and can reinstall it on its next token refresh.", staleCount)
	}
	if idleCount > 0 {
		ui.Infof("%d session(s) idle for %s or more — run `cas reap` to close them.", idleCount, ui.Duration(idle))
	}
}

// whereClosed names a session in a sentence, without inventing a terminal for
// one that never had one.
func whereClosed(s *liveSession) string {
	if s.Detached() {
		return ""
	}
	return " on " + s.TTY
}

func idleCell(s *liveSession, threshold time.Duration) string {
	if !s.IdleKnown {
		return ui.Dim("?")
	}
	text := ui.Duration(s.Idle)
	if s.Idle >= threshold {
		return ui.Yellow(text)
	}
	return text
}

func credentialCell(s *liveSession) string {
	switch {
	case !s.StaleKnown:
		return ui.Dim("?")
	case s.Stale:
		return ui.Red("stale")
	default:
		return ui.Green("current")
	}
}

func note(s *liveSession) string {
	switch {
	case s.Ours:
		return ui.Dim("this session")
	case s.Detached():
		return ui.Dim("no terminal")
	default:
		return ""
	}
}

// shortenHome renders a path with ~ for the home directory.
func shortenHome(path string) string {
	if path == "" {
		return ui.Dim("—")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if rel := strings.TrimPrefix(path, home+string(filepath.Separator)); rel != path {
		return "~" + string(filepath.Separator) + rel
	}
	return path
}

func parsePIDs(s string) (map[int]bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	pids := map[int]bool{}
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		n, err := strconv.Atoi(f)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid pid %q", f)
		}
		pids[n] = true
	}
	return pids, nil
}

func containsPID(list []*liveSession, pid int) bool {
	for _, s := range list {
		if s.PID == pid {
			return true
		}
	}
	return false
}
