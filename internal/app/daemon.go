package app

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Willhong/claude-account-switch/internal/store"
	"github.com/Willhong/claude-account-switch/internal/ui"
)

// LaunchdLabel identifies the background refresh agent.
const LaunchdLabel = "com.github.hong-kyungtack.cas.refresh"

// defaultInterval is how often the agent rotates tokens. Access tokens last
// hours, so half-hourly keeps every slot warm without being chatty.
const defaultInterval = 1800

// Reap modes for the background agent.
const (
	// ReapOff is the default: the agent refreshes tokens and touches nothing else.
	ReapOff = "off"
	// ReapStale closes idle sessions that predate the last account switch —
	// the ones actually holding the wrong credential.
	ReapStale = "stale"
	// ReapIdleMode closes every idle session, whichever account it holds.
	ReapIdleMode = "idle"
)

// minDaemonReapIdle is the shortest idle window the agent will act on. Nothing
// asks before the agent closes a session, so a threshold low enough to catch
// someone mid-thought is a footgun rather than a setting.
const minDaemonReapIdle = 15 * time.Minute

func validReapMode(mode string) error {
	switch mode {
	case ReapOff, ReapStale, ReapIdleMode:
		return nil
	default:
		return fmt.Errorf("unknown --reap mode %q (want %s, %s or %s)", mode, ReapOff, ReapStale, ReapIdleMode)
	}
}

// CmdDaemon manages the launchd agent that keeps parked accounts refreshed.
func CmdDaemon(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: cas daemon <install|uninstall|status|run|log>")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "install":
		return daemonInstall(rest)
	case "uninstall", "remove":
		return daemonUninstall()
	case "status":
		return daemonStatus()
	case "run":
		return daemonRun(rest)
	case "log":
		return daemonLog()
	default:
		return fmt.Errorf("unknown daemon subcommand %q (want install, uninstall, status, run or log)", sub)
	}
}

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", LaunchdLabel+".plist"), nil
}

func daemonInstall(args []string) error {
	fs := flag.NewFlagSet("cas daemon install", flag.ContinueOnError)
	interval := fs.Int("interval", defaultInterval, "seconds between refresh runs")
	reap := fs.String("reap", ReapOff, "also close idle sessions each run: off, stale or idle")
	reapIdle := fs.Duration("reap-idle", ReapIdle(), "how long untouched a session must be for the agent to close it")
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}
	if *interval < 60 {
		return errors.New("--interval must be at least 60 seconds")
	}
	if err := validReapMode(*reap); err != nil {
		return err
	}
	if *reap != ReapOff && *reapIdle < minDaemonReapIdle {
		return fmt.Errorf("--reap-idle must be at least %s for the background agent, which closes sessions without asking", ui.Duration(minDaemonReapIdle))
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the cas binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	st, err := store.Open()
	if err != nil {
		return err
	}
	logPath := st.Path("daemon.log")

	path, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}

	// Carry over the endpoint/target overrides, otherwise the agent would run
	// against different settings than the interactive command.
	var envEntries []string
	for _, key := range []string{
		"CAS_HOME", "CAS_KEYCHAIN_SERVICE", "CAS_KEYCHAIN_ACCOUNT",
		"CAS_REFRESH_THRESHOLD", "CAS_OAUTH_CLIENT_ID", "CAS_OAUTH_TOKEN_URL",
		"CAS_OAUTH_AUTHORIZE_URL", "CAS_API_BASE", "CLAUDE_CONFIG_DIR",
	} {
		if v := os.Getenv(key); v != "" {
			envEntries = append(envEntries,
				fmt.Sprintf("      <key>%s</key><string>%s</string>", key, escapeXML(v)))
		}
	}
	envBlock := ""
	if len(envEntries) > 0 {
		envBlock = "    <key>EnvironmentVariables</key>\n    <dict>\n" +
			strings.Join(envEntries, "\n") + "\n    </dict>\n"
	}

	// The agent re-runs this binary, so the reap settings ride along as flags.
	progArgs := []string{exe, "daemon", "run"}
	if *reap != ReapOff {
		progArgs = append(progArgs, "-reap", *reap, "-reap-idle", reapIdle.String())
	}
	var argBlock strings.Builder
	for _, arg := range progArgs {
		fmt.Fprintf(&argBlock, "      <string>%s</string>\n", escapeXML(arg))
	}

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
  <dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
%s    </array>
    <key>StartInterval</key>
    <integer>%d</integer>
    <key>RunAtLoad</key>
    <true/>
    <key>ProcessType</key>
    <string>Background</string>
%s    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
  </dict>
</plist>
`, LaunchdLabel, argBlock.String(), *interval, envBlock, escapeXML(logPath), escapeXML(logPath))

	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	domain := fmt.Sprintf("gui/%d", os.Getuid())
	// Replace any previous registration before loading the new one.
	_ = exec.Command("/bin/launchctl", "bootout", domain+"/"+LaunchdLabel).Run()
	if out, err := exec.Command("/bin/launchctl", "bootstrap", domain, path).CombinedOutput(); err != nil {
		return fmt.Errorf("load the launchd agent: %s", strings.TrimSpace(string(out)))
	}
	_ = exec.Command("/bin/launchctl", "enable", domain+"/"+LaunchdLabel).Run()

	ui.OKf("Installed the refresh agent (every %s).", ui.Duration(secs(*interval)))
	fmt.Fprintf(os.Stderr, "  plist  %s\n  log    %s\n", path, logPath)
	switch *reap {
	case ReapStale:
		fmt.Fprintf(os.Stderr, "  reap   sessions idle for %s that predate the last switch\n", ui.Duration(*reapIdle))
	case ReapIdleMode:
		fmt.Fprintf(os.Stderr, "  reap   every session idle for %s\n", ui.Duration(*reapIdle))
	default:
		fmt.Fprintf(os.Stderr, "  reap   %s — run `cas daemon install --reap stale` to close idle sessions too\n", ui.Dim("off"))
	}
	return nil
}

// daemonRun is what launchd executes: refresh every slot that needs it, and,
// when the agent was installed with --reap, close the idle sessions that would
// otherwise rotate a credential behind cas's back.
func daemonRun(args []string) error {
	fs := flag.NewFlagSet("cas daemon run", flag.ContinueOnError)
	reap := fs.String("reap", ReapOff, "close idle sessions before refreshing: off, stale or idle")
	reapIdle := fs.Duration("reap-idle", ReapIdle(), "how long untouched a session must be to be closed")
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}
	if err := validReapMode(*reap); err != nil {
		return err
	}
	ui.SetTimestamps(true)

	// Reaping comes first, and in its own store lock: closing a session that
	// holds a superseded credential before rotating anything means it cannot
	// race the refresh and write its own token back afterwards.
	if err := daemonReap(*reap, *reapIdle); err != nil {
		// A reap failure must not cost the run its refresh.
		ui.Errorf("reap: %v", err)
	}
	return CmdRefresh([]string{"-quiet"})
}

// daemonReap closes idle sessions unattended. It never escalates to SIGKILL
// and never acts on a session whose idle time could not be measured.
func daemonReap(mode string, idle time.Duration) error {
	if mode == ReapOff {
		return nil
	}
	if idle < minDaemonReapIdle {
		return fmt.Errorf("refusing to reap on a %s idle window; %s is the minimum", ui.Duration(idle), ui.Duration(minDaemonReapIdle))
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
		return nil
	}
	if mode == ReapStale && a.State.SwitchedAt.IsZero() {
		ui.Infof("reap: no account switch recorded yet, so no session can be judged stale; nothing closed")
		return nil
	}

	doomed := selectForReap(found, reapCriteria{Idle: idle, StaleOnly: mode == ReapStale})
	if len(doomed) == 0 {
		return nil
	}

	closed, survivors := closeSessions(doomed, syscall.SIGTERM)
	for _, s := range closed {
		ui.OKf("reap: closed pid %d (idle %s, up %s, %s)", s.PID, ui.Duration(s.Idle), ui.Duration(s.Uptime), shortenHome(s.CWD))
	}
	// launchd runs again shortly; a session that ignored SIGTERM is left for a
	// person to look at rather than killed outright.
	for _, s := range survivors {
		ui.Warnf("reap: pid %d did not exit on SIGTERM; leaving it running", s.PID)
	}
	return nil
}

func daemonUninstall() error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_ = exec.Command("/bin/launchctl", "bootout", domain+"/"+LaunchdLabel).Run()

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	ui.OKf("Removed the refresh agent.")
	return nil
}

func daemonStatus() error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		fmt.Println("not installed — run `cas daemon install`")
		return nil
	}

	out, err := exec.Command("/bin/launchctl", "print",
		fmt.Sprintf("gui/%d/%s", os.Getuid(), LaunchdLabel)).CombinedOutput()
	if err != nil {
		fmt.Printf("installed but not loaded (%s)\n", path)
		fmt.Printf("  reap = %s\n", installedReapMode(path))
		return nil
	}

	fmt.Printf("%s  %s\n", ui.Green("loaded"), path)
	fmt.Printf("  reap = %s\n", installedReapMode(path))
	// launchctl print repeats some keys for nested sections; report the first.
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		for _, key := range []string{"state = ", "last exit code = ", "runs = ", "pid = "} {
			if strings.HasPrefix(trimmed, key) && !seen[key] {
				seen[key] = true
				fmt.Printf("  %s\n", trimmed)
			}
		}
	}

	st, err := store.Open()
	if err == nil {
		if info, err := os.Stat(st.Path("daemon.log")); err == nil {
			fmt.Printf("  log = %s (%s, last written %s ago)\n",
				st.Path("daemon.log"), byteSize(info.Size()), ui.Duration(sinceMod(info)))
		}
	}
	return nil
}

// installedReapMode reports the reap settings baked into an installed plist,
// so `cas daemon status` says what the agent will actually do rather than what
// the current defaults would do.
func installedReapMode(plistPath string) string {
	b, err := os.ReadFile(plistPath)
	if err != nil {
		return "unknown"
	}
	args := plistProgramArguments(string(b))
	mode, idle := ReapOff, ""
	for i, a := range args {
		if i+1 >= len(args) {
			break
		}
		switch a {
		case "-reap", "--reap":
			mode = args[i+1]
		case "-reap-idle", "--reap-idle":
			idle = args[i+1]
		}
	}
	if mode == ReapOff {
		return ui.Dim("off")
	}
	if d, err := time.ParseDuration(idle); err == nil {
		return fmt.Sprintf("%s (idle %s)", mode, ui.Duration(d))
	}
	return mode
}

// plistProgramArguments pulls the <string> values out of the ProgramArguments
// array. cas writes this file itself, so a tolerant scan beats a plist parser.
func plistProgramArguments(plist string) []string {
	start := strings.Index(plist, "<key>ProgramArguments</key>")
	if start < 0 {
		return nil
	}
	rest := plist[start:]
	end := strings.Index(rest, "</array>")
	if end < 0 {
		return nil
	}
	var args []string
	for _, chunk := range strings.Split(rest[:end], "<string>")[1:] {
		if i := strings.Index(chunk, "</string>"); i >= 0 {
			args = append(args, chunk[:i])
		}
	}
	return args
}

func daemonLog() error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	b, err := os.ReadFile(st.Path("daemon.log"))
	if errors.Is(err, os.ErrNotExist) {
		fmt.Println("no log yet")
		return nil
	}
	if err != nil {
		return err
	}
	// Show the tail; the agent appends forever.
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > 50 {
		lines = lines[len(lines)-50:]
	}
	fmt.Println(strings.Join(lines, "\n"))
	return nil
}

func escapeXML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func byteSize(n int64) string {
	switch {
	case n < 1024:
		return strconv.FormatInt(n, 10) + " B"
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}
