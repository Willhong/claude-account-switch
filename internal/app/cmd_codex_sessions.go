package app

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Willhong/claude-account-switch/internal/session"
	"github.com/Willhong/claude-account-switch/internal/ui"
)

type codexLiveSession struct {
	*session.Session
	Stale      bool
	StaleKnown bool
	AppServer  bool
	BBPool     bool
}

func (s *codexLiveSession) Protected() bool { return s.Ours || s.AppServer }

func (a *codexApp) sessions() ([]*codexLiveSession, error) {
	found, err := session.List(session.Options{Match: session.IsCodex})
	if err != nil {
		return nil, fmt.Errorf("list Codex processes: %w", err)
	}
	out := make([]*codexLiveSession, 0, len(found))
	for _, process := range found {
		lower := strings.ToLower(process.Args)
		appServer := strings.Contains(lower, " app-server")
		out = append(out, &codexLiveSession{
			Session:    process,
			Stale:      !a.State.SwitchedAt.IsZero() && process.Started.Before(a.State.SwitchedAt),
			StaleKnown: !a.State.SwitchedAt.IsZero(),
			AppServer:  appServer,
			BBPool:     appServer && (strings.Contains(lower, "bb-account-pool") || strings.Contains(lower, "codex_pool_auth_token")),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Uptime > out[j].Uptime })
	return out, nil
}

func cmdCodexSessions(args []string) error {
	fs := flag.NewFlagSet("cas codex sessions", flag.ContinueOnError)
	idle := fs.Duration("idle", codexReapIdle(), "how long untouched counts as idle")
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}
	a, err := newCodexApp()
	if err != nil {
		return err
	}
	defer a.Close()
	found, err := a.sessions()
	if err != nil {
		return err
	}
	if len(found) == 0 {
		ui.Infof("No Codex processes are running.")
		return nil
	}
	table := &ui.Table{Header: []string{"PID", "TTY", "IDLE", "UPTIME", "CREDENTIAL", "KIND", "DIRECTORY", ""}}
	for _, process := range found {
		table.Rows = append(table.Rows, []string{
			strconv.Itoa(process.PID), process.TTYName(), codexIdleCell(process, *idle), ui.Duration(process.Uptime),
			codexCredentialCell(process), codexProcessKind(process), shortenHome(process.CWD), codexSessionNote(process),
		})
	}
	table.Render(os.Stdout)
	fmt.Println()
	if active := a.State.Active(); active != nil && !a.State.SwitchedAt.IsZero() {
		ui.Infof("cas switched the file-backed login to Codex slot %d (%s) %s ago.", active.N, active.Name(), ui.Duration(time.Since(a.State.SwitchedAt)))
	}
	summariseCodexSessions(found, *idle)
	return nil
}

func cmdCodexReap(args []string) error {
	fs := flag.NewFlagSet("cas codex reap", flag.ContinueOnError)
	idle := fs.Duration("idle", codexReapIdle(), "close CLI sessions untouched for at least this long")
	stale := fs.Bool("stale", false, "narrow selection to CLI sessions started before the last switch")
	all := fs.Bool("all", false, "ignore the idle threshold for non-managed CLI sessions")
	pidList := fs.String("pid", "", "close these Codex CLI pids only (comma separated)")
	force := fs.Bool("force", false, "send SIGKILL instead of SIGTERM")
	dryRun := fs.Bool("dry-run", false, "report what would be closed")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}
	a, err := newCodexApp()
	if err != nil {
		return err
	}
	defer a.Close()
	found, err := a.sessions()
	if err != nil {
		return err
	}
	if len(found) == 0 {
		ui.Infof("No Codex processes are running.")
		return nil
	}
	explicit, err := parsePIDs(*pidList)
	if err != nil {
		return err
	}
	for _, process := range found {
		if !explicit[process.PID] {
			continue
		}
		if process.Ours {
			return fmt.Errorf("pid %d is the Codex process hosting this cas command; refusing to close it", process.PID)
		}
		if process.AppServer {
			return fmt.Errorf("pid %d is a managed Codex app-server (%s); refusing to reap it", process.PID, codexProcessKind(process))
		}
	}
	doomed := selectCodexForReap(found, codexReapCriteria{Idle: *idle, IgnoreIdle: *all, StaleOnly: *stale, PIDs: explicit})
	if len(explicit) > 0 {
		for pid := range explicit {
			if !containsCodexPID(doomed, pid) {
				return fmt.Errorf("pid %d is not a reapable Codex CLI session", pid)
			}
		}
	}
	if len(doomed) == 0 {
		summariseCodexSessions(found, *idle)
		ui.OKf("Nothing to reap.")
		return nil
	}

	fmt.Println(ui.Bold("Codex CLI sessions to close"))
	plan := &ui.Table{}
	for _, process := range doomed {
		plan.Rows = append(plan.Rows, []string{
			"  " + ui.Red("✗") + fmt.Sprintf(" pid %d", process.PID), process.TTYName(),
			"idle " + codexIdleCell(process, *idle), "up " + ui.Duration(process.Uptime),
			codexCredentialCell(process), ui.Dim(shortenHome(process.CWD)),
		})
	}
	plan.Render(os.Stdout)
	if *dryRun {
		ui.Infof("%d Codex CLI session(s) would be closed.", len(doomed))
		return nil
	}
	if !*yes {
		fmt.Fprintf(os.Stderr, "\nClose %d Codex CLI session(s)? Unsaved work in them is lost. [y/N] ", len(doomed))
		if !confirmed() {
			ui.Infof("Nothing closed.")
			return nil
		}
	}
	signal := syscall.SIGTERM
	if *force {
		signal = syscall.SIGKILL
	}
	closed, survivors := closeCodexSessions(doomed, signal)
	for _, process := range closed {
		ui.OKf("Closed Codex pid %d.", process.PID)
	}
	for _, process := range survivors {
		ui.Warnf("Codex pid %d is still running; re-run with --force to send SIGKILL.", process.PID)
	}
	return nil
}

type codexReapCriteria struct {
	Idle       time.Duration
	IgnoreIdle bool
	StaleOnly  bool
	PIDs       map[int]bool
}

func selectCodexForReap(found []*codexLiveSession, criteria codexReapCriteria) []*codexLiveSession {
	var selected []*codexLiveSession
	for _, process := range found {
		if process.Protected() {
			continue
		}
		if len(criteria.PIDs) > 0 {
			if criteria.PIDs[process.PID] {
				selected = append(selected, process)
			}
			continue
		}
		if !criteria.IgnoreIdle && !(process.IdleKnown && process.Idle >= criteria.Idle) {
			continue
		}
		if criteria.StaleOnly && !process.Stale {
			continue
		}
		selected = append(selected, process)
	}
	return selected
}

func closeCodexSessions(selected []*codexLiveSession, signal syscall.Signal) (closed, survivors []*codexLiveSession) {
	for _, process := range selected {
		if err := process.Signal(signal); err != nil {
			ui.Warnf("could not signal Codex pid %d: %v", process.PID, err)
			continue
		}
		survivors = append(survivors, process)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(survivors) > 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		var remaining []*codexLiveSession
		for _, process := range survivors {
			if process.Alive() {
				remaining = append(remaining, process)
			} else {
				closed = append(closed, process)
			}
		}
		survivors = remaining
	}
	return closed, survivors
}

func codexReapIdle() time.Duration {
	value := strings.TrimSpace(os.Getenv("CAS_CODEX_REAP_IDLE"))
	if value == "" {
		return DefaultReapIdle
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < 0 {
		ui.Warnf("ignoring invalid CAS_CODEX_REAP_IDLE=%q", value)
		return DefaultReapIdle
	}
	return duration
}

func codexProcessKind(process *codexLiveSession) string {
	switch {
	case process.BBPool:
		return "bb-pool"
	case process.AppServer:
		return "app-server"
	default:
		return "cli"
	}
}

func codexSessionNote(process *codexLiveSession) string {
	switch {
	case process.Ours:
		return ui.Dim("this session")
	case process.BBPool:
		return ui.Dim("protected BB provider")
	case process.AppServer:
		return ui.Dim("protected managed process")
	case process.Detached():
		return ui.Dim("no terminal")
	default:
		return ""
	}
}

func codexIdleCell(process *codexLiveSession, threshold time.Duration) string {
	if !process.IdleKnown {
		return ui.Dim("?")
	}
	text := ui.Duration(process.Idle)
	if process.Idle >= threshold {
		return ui.Yellow(text)
	}
	return text
}

func codexCredentialCell(process *codexLiveSession) string {
	if process.BBPool {
		return ui.Cyan("pool")
	}
	switch {
	case !process.StaleKnown:
		return ui.Dim("?")
	case process.Stale:
		return ui.Red("stale")
	default:
		return ui.Green("current")
	}
}

func summariseCodexSessions(found []*codexLiveSession, idle time.Duration) {
	var stale, idleCount, managed int
	for _, process := range found {
		if process.AppServer {
			managed++
			continue
		}
		if process.Ours {
			continue
		}
		if process.Stale {
			stale++
		}
		if process.IdleKnown && process.Idle >= idle {
			idleCount++
		}
	}
	if stale > 0 {
		ui.Warnf("%d Codex CLI session(s) started before the last file-backed account switch.", stale)
	}
	if idleCount > 0 {
		ui.Infof("%d Codex CLI session(s) idle for %s or more; run `cas codex reap` to close them.", idleCount, ui.Duration(idle))
	}
	if managed > 0 {
		ui.Infof("%d managed app-server process(es) are shown but always protected from reap.", managed)
	}
}

func containsCodexPID(processes []*codexLiveSession, pid int) bool {
	for _, process := range processes {
		if process.PID == pid {
			return true
		}
	}
	return false
}

func codexDaemonReap(mode string, idle time.Duration) error {
	if mode == ReapOff {
		return nil
	}
	if idle < minDaemonReapIdle {
		return fmt.Errorf("refusing to reap Codex sessions on a %s idle window; %s is the minimum", ui.Duration(idle), ui.Duration(minDaemonReapIdle))
	}
	a, err := newCodexApp()
	if err != nil {
		return err
	}
	defer a.Close()
	found, err := a.sessions()
	if err != nil {
		return err
	}
	if mode == ReapStale && a.State.SwitchedAt.IsZero() {
		return nil
	}
	selected := selectCodexForReap(found, codexReapCriteria{Idle: idle, StaleOnly: mode == ReapStale})
	closed, survivors := closeCodexSessions(selected, syscall.SIGTERM)
	for _, process := range closed {
		ui.OKf("codex reap: closed pid %d (idle %s, up %s)", process.PID, ui.Duration(process.Idle), ui.Duration(process.Uptime))
	}
	for _, process := range survivors {
		ui.Warnf("codex reap: pid %d ignored SIGTERM; leaving it running", process.PID)
	}
	return nil
}
