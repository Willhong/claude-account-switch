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

	"github.com/Willhong/claude-account-switch/internal/store"
	"github.com/Willhong/claude-account-switch/internal/ui"
)

// LaunchdLabel identifies the background refresh agent.
const LaunchdLabel = "com.github.hong-kyungtack.cas.refresh"

// defaultInterval is how often the agent rotates tokens. Access tokens last
// hours, so half-hourly keeps every slot warm without being chatty.
const defaultInterval = 1800

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
		ui.SetTimestamps(true)
		return CmdRefresh(append([]string{"-quiet"}, rest...))
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
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}
	if *interval < 60 {
		return errors.New("--interval must be at least 60 seconds")
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

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
  <dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
      <string>%s</string>
      <string>daemon</string>
      <string>run</string>
    </array>
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
`, LaunchdLabel, escapeXML(exe), *interval, envBlock, escapeXML(logPath), escapeXML(logPath))

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
		return nil
	}

	fmt.Printf("%s  %s\n", ui.Green("loaded"), path)
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
