package app

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Willhong/claude-account-switch/internal/codexstore"
	"github.com/Willhong/claude-account-switch/internal/ui"
)

const codexLaunchdLabel = "com.github.hong-kyungtack.cas.codex-refresh"

func cmdCodexDaemon(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: cas codex daemon <install|uninstall|status|run|log>")
	}
	switch args[0] {
	case "install":
		return codexDaemonInstall(args[1:])
	case "uninstall", "remove":
		return codexDaemonUninstall()
	case "status":
		return codexDaemonStatus()
	case "run":
		return codexDaemonRun(args[1:])
	case "log":
		return codexDaemonLog()
	default:
		return fmt.Errorf("unknown Codex daemon subcommand %q", args[0])
	}
}

func codexDaemonInstall(args []string) error {
	fs := flag.NewFlagSet("cas codex daemon install", flag.ContinueOnError)
	interval := fs.Int("interval", defaultInterval, "seconds between Codex refresh runs")
	reap := fs.String("reap", ReapOff, "also close idle standalone CLI sessions: off, stale or idle")
	reapIdle := fs.Duration("reap-idle", codexReapIdle(), "how long a CLI session must be idle before unattended reap")
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
		return fmt.Errorf("--reap-idle must be at least %s for unattended Codex reap", ui.Duration(minDaemonReapIdle))
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	codexBin := codexBinOverride()
	if codexBin == "" {
		codexBin, err = exec.LookPath("codex")
		if err != nil {
			return errors.New("could not find `codex` on PATH")
		}
	}

	store, err := codexstore.Open()
	if err != nil {
		return err
	}
	logPath := store.Path("codex-daemon.log")
	path, err := codexPlistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	environment := map[string]string{"CAS_CODEX_BIN": codexBin}
	for _, key := range []string{"CAS_HOME", "CODEX_HOME", "CAS_CODEX_REFRESH_THRESHOLD", "CAS_CODEX_REAP_IDLE"} {
		if value := os.Getenv(key); value != "" {
			environment[key] = value
		}
	}
	var envKeys []string
	for key := range environment {
		envKeys = append(envKeys, key)
	}
	sort.Strings(envKeys)
	var envEntries []string
	for _, key := range envKeys {
		envEntries = append(envEntries, fmt.Sprintf("      <key>%s</key><string>%s</string>", escapeXML(key), escapeXML(environment[key])))
	}
	envBlock := "    <key>EnvironmentVariables</key>\n    <dict>\n" + strings.Join(envEntries, "\n") + "\n    </dict>\n"

	programArgs := []string{exe, "codex", "daemon", "run"}
	if *reap != ReapOff {
		programArgs = append(programArgs, "-reap", *reap, "-reap-idle", reapIdle.String())
	}
	var argBlock strings.Builder
	for _, arg := range programArgs {
		fmt.Fprintf(&argBlock, "      <string>%s</string>\n", escapeXML(arg))
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
  <dict>
    <key>Label</key><string>%s</string>
    <key>ProgramArguments</key>
    <array>
%s    </array>
    <key>StartInterval</key><integer>%d</integer>
    <key>RunAtLoad</key><true/>
    <key>ProcessType</key><string>Background</string>
%s    <key>StandardOutPath</key><string>%s</string>
    <key>StandardErrorPath</key><string>%s</string>
  </dict>
</plist>
`, codexLaunchdLabel, argBlock.String(), *interval, envBlock, escapeXML(logPath), escapeXML(logPath))
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_ = exec.Command("/bin/launchctl", "bootout", domain+"/"+codexLaunchdLabel).Run()
	if output, err := exec.Command("/bin/launchctl", "bootstrap", domain, path).CombinedOutput(); err != nil {
		return fmt.Errorf("load the Codex refresh agent: %s", strings.TrimSpace(string(output)))
	}
	_ = exec.Command("/bin/launchctl", "enable", domain+"/"+codexLaunchdLabel).Run()
	ui.OKf("Installed the Codex refresh agent (every %s).", ui.Duration(secs(*interval)))
	fmt.Fprintf(os.Stderr, "  plist  %s\n  log    %s\n  codex  %s\n", path, logPath, codexBin)
	return nil
}

func codexDaemonRun(args []string) error {
	fs := flag.NewFlagSet("cas codex daemon run", flag.ContinueOnError)
	reap := fs.String("reap", ReapOff, "close idle standalone CLI sessions before refresh")
	reapIdle := fs.Duration("reap-idle", codexReapIdle(), "idle threshold for reap")
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}
	if err := validReapMode(*reap); err != nil {
		return err
	}
	ui.SetTimestamps(true)
	if err := codexDaemonReap(*reap, *reapIdle); err != nil {
		ui.Errorf("codex reap: %v", err)
	}
	return cmdCodexRefresh([]string{"-quiet"})
}

func codexDaemonUninstall() error {
	path, err := codexPlistPath()
	if err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_ = exec.Command("/bin/launchctl", "bootout", domain+"/"+codexLaunchdLabel).Run()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ui.OKf("Removed the Codex refresh agent.")
	return nil
}

func codexDaemonStatus() error {
	path, err := codexPlistPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		fmt.Println("not installed; run `cas codex daemon install`")
		return nil
	}
	output, err := exec.Command("/bin/launchctl", "print", fmt.Sprintf("gui/%d/%s", os.Getuid(), codexLaunchdLabel)).CombinedOutput()
	if err != nil {
		fmt.Printf("installed but not loaded (%s)\n", path)
		fmt.Printf("  reap = %s\n", codexInstalledReapMode(path))
		return nil
	}
	fmt.Printf("%s  %s\n", ui.Green("loaded"), path)
	fmt.Printf("  reap = %s\n", codexInstalledReapMode(path))
	seen := map[string]bool{}
	for _, line := range strings.Split(string(output), "\n") {
		trimmed := strings.TrimSpace(line)
		for _, key := range []string{"state = ", "last exit code = ", "runs = ", "pid = "} {
			if strings.HasPrefix(trimmed, key) && !seen[key] {
				seen[key] = true
				fmt.Printf("  %s\n", trimmed)
			}
		}
	}
	store, err := codexstore.Open()
	if err == nil {
		if info, err := os.Stat(store.Path("codex-daemon.log")); err == nil {
			fmt.Printf("  log = %s (%s, last written %s ago)\n", store.Path("codex-daemon.log"), byteSize(info.Size()), ui.Duration(sinceMod(info)))
		}
	}
	return nil
}

func codexDaemonLog() error {
	store, err := codexstore.Open()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(store.Path("codex-daemon.log"))
	if errors.Is(err, os.ErrNotExist) {
		fmt.Println("no Codex daemon log yet")
		return nil
	}
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > 50 {
		lines = lines[len(lines)-50:]
	}
	fmt.Println(strings.Join(lines, "\n"))
	return nil
}

func codexPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", codexLaunchdLabel+".plist"), nil
}

func codexInstalledReapMode(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "unknown"
	}
	args := plistProgramArguments(string(raw))
	mode, idle := ReapOff, ""
	for i, arg := range args {
		if i+1 >= len(args) {
			break
		}
		switch arg {
		case "-reap", "--reap":
			mode = args[i+1]
		case "-reap-idle", "--reap-idle":
			idle = args[i+1]
		}
	}
	if mode == ReapOff {
		return ui.Dim("off")
	}
	if duration, err := time.ParseDuration(idle); err == nil {
		return fmt.Sprintf("%s (idle %s)", mode, ui.Duration(duration))
	}
	return mode
}
