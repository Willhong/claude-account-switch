package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const psFixture = `  501 65968       21:19   0:15.50 ??       /Users/me/.local/bin/claude --output-format stream-json
  502 400         01-00:48:27   9:42.66 ttys004  claude --permission-mode bypassPermissions
  503 502         01-00:48:23   0:00.07 ttys004  /Applications/Some.app/Contents/mcp-server --app desktop
  504 1           02:34   0:06.21 ttys003  claude
  505 1           02:34   0:06.21 ttys003  node /usr/local/lib/claude/cli.js
`

func TestParsePS(t *testing.T) {
	rows := parsePS(psFixture)
	if len(rows) != 5 {
		t.Fatalf("got %d rows, want 5", len(rows))
	}
	if rows[0].tty != "" {
		t.Errorf("detached row kept tty %q", rows[0].tty)
	}
	if got, want := rows[1].elapsed, 24*time.Hour+48*time.Minute+27*time.Second; got != want {
		t.Errorf("elapsed = %v, want %v", got, want)
	}
	if got, want := rows[1].cpu, 9*time.Minute+42*time.Second+660*time.Millisecond; got != want {
		t.Errorf("cpu = %v, want %v", got, want)
	}
	if rows[3].args != "claude" {
		t.Errorf("args = %q", rows[3].args)
	}
}

func TestIsClaude(t *testing.T) {
	cases := map[string]bool{
		"claude --permission-mode bypassPermissions":  true,
		"/Users/me/.local/bin/claude --output-format": true,
		"claude": true,
		"/Applications/Some.app/mcp-server --app": false,
		"node /usr/local/lib/claude/cli.js":       false,
		"grep claude":                             false,
		"":                                        false,
	}
	for args, want := range cases {
		if got := isClaude(args); got != want {
			t.Errorf("isClaude(%q) = %v, want %v", args, got, want)
		}
	}
}

func TestSessionsFromMarksOwnAncestry(t *testing.T) {
	rows := parsePS(psFixture)
	// pid 503 stands in for the cas process: its parent 502 is a claude session.
	got := sessionsFrom(rows, 503, Options{Now: func() time.Time { return time.Unix(1_700_000_000, 0) }})
	if len(got) != 3 {
		t.Fatalf("got %d sessions, want 3", len(got))
	}
	byPID := map[int]*Session{}
	for _, s := range got {
		byPID[s.PID] = s
	}
	if !byPID[502].Ours {
		t.Error("the session cas runs inside was not marked as ours")
	}
	if byPID[501] == nil || byPID[501].Ours {
		t.Error("an unrelated detached session was marked as ours")
	}
	if !byPID[501].Detached() {
		t.Error("a session with no terminal should report as detached")
	}
	if byPID[504].Ours {
		t.Error("a session on another terminal was marked as ours")
	}
}

func TestSessionsFromMarksSharedTerminal(t *testing.T) {
	// pid 504 shares ttys003 with the process running cas.
	rows := append(parsePS(psFixture), procRow{pid: 900, ppid: 1, tty: "ttys003", args: "cas reap"})
	got := sessionsFrom(rows, 900, Options{})
	for _, s := range got {
		if s.PID == 504 && !s.Ours {
			t.Error("a session sharing this terminal was not marked as ours")
		}
	}
}

func TestParseElapsed(t *testing.T) {
	cases := map[string]time.Duration{
		"02:34":      2*time.Minute + 34*time.Second,
		"01:02:03":   time.Hour + 2*time.Minute + 3*time.Second,
		"3-04:05:06": 3*24*time.Hour + 4*time.Hour + 5*time.Minute + 6*time.Second,
		"nonsense":   0,
	}
	for in, want := range cases {
		if got := parseElapsed(in); got != want {
			t.Errorf("parseElapsed(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSlugMatchesClaudeCodeLayout(t *testing.T) {
	if got, want := Slug("/Users/me/workspace/claude-account-switch"), "-Users-me-workspace-claude-account-switch"; got != want {
		t.Errorf("Slug = %q, want %q", got, want)
	}
	if got, want := Slug("/Users/me/.config/app_1"), "-Users-me--config-app-1"; got != want {
		t.Errorf("Slug = %q, want %q", got, want)
	}
}

func TestTranscriptIdleTakesTheNewestTranscript(t *testing.T) {
	projects := t.TempDir()
	cwd := "/Users/me/project"
	dir := filepath.Join(projects, Slug(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	write := func(name string, age time.Duration) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	write("old.jsonl", 5*time.Hour)
	write("new.jsonl", 20*time.Minute)
	write("ignored.txt", time.Minute)

	got, ok := transcriptIdle(projects, cwd, now)
	if !ok {
		t.Fatal("no transcript idle reported")
	}
	if got < 19*time.Minute || got > 21*time.Minute {
		t.Errorf("idle = %v, want about 20m", got)
	}

	if _, ok := transcriptIdle(projects, "/Users/me/elsewhere", now); ok {
		t.Error("a directory with no transcripts should report no signal")
	}
}

func TestIdleOfFallsBackToTranscriptsAndCapsAtUptime(t *testing.T) {
	s := &Session{Uptime: 10 * time.Minute}
	if _, ok := idleOf(s, "", time.Now()); ok {
		t.Error("a session with no terminal and no transcript should report no idle signal")
	}

	projects := t.TempDir()
	cwd := "/Users/me/fresh"
	dir := filepath.Join(projects, Slug(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "a.jsonl")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(path, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	s = &Session{Uptime: 10 * time.Minute, CWD: cwd}
	got, ok := idleOf(s, projects, now)
	if !ok {
		t.Fatal("no idle reported")
	}
	if got != 10*time.Minute {
		t.Errorf("idle = %v, want it capped at the 10m uptime", got)
	}
}
