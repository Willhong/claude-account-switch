package app

import (
	"strings"
	"testing"
	"time"

	"github.com/Willhong/claude-account-switch/internal/session"
)

func newTestSession(pid int) *session.Session { return &session.Session{PID: pid} }

// ourSession stands in for the session cas is running inside.
func ourSession(pid int) *session.Session { return &session.Session{PID: pid, Ours: true} }

func TestValidReapMode(t *testing.T) {
	for _, mode := range []string{ReapOff, ReapStale, ReapIdleMode} {
		if err := validReapMode(mode); err != nil {
			t.Errorf("validReapMode(%q) = %v, want nil", mode, err)
		}
	}
	if err := validReapMode("kill-everything"); err == nil {
		t.Error("an unknown mode was accepted")
	}
}

func TestDaemonReapRefusesATinyIdleWindow(t *testing.T) {
	// The agent closes sessions with nobody watching, so it must not act on a
	// window short enough to catch someone mid-thought.
	err := daemonReap(ReapStale, time.Minute)
	if err == nil {
		t.Fatal("a one-minute idle window was accepted")
	}
	if !strings.Contains(err.Error(), "minimum") {
		t.Errorf("unhelpful error: %v", err)
	}
	// Off stays off whatever the window.
	if err := daemonReap(ReapOff, time.Second); err != nil {
		t.Errorf("reap off should be a no-op, got %v", err)
	}
}

func TestPlistProgramArguments(t *testing.T) {
	plist := `<plist version="1.0">
  <dict>
    <key>Label</key>
    <string>com.example.thing</string>
    <key>ProgramArguments</key>
    <array>
      <string>/Users/me/.local/bin/cas</string>
      <string>daemon</string>
      <string>run</string>
      <string>-reap</string>
      <string>stale</string>
      <string>-reap-idle</string>
      <string>2h0m0s</string>
    </array>
    <key>StartInterval</key>
    <integer>1800</integer>
  </dict>
</plist>`
	got := plistProgramArguments(plist)
	if len(got) != 7 {
		t.Fatalf("got %d arguments (%v), want 7", len(got), got)
	}
	// The Label string above the array must not leak in.
	if got[0] != "/Users/me/.local/bin/cas" {
		t.Errorf("first argument = %q", got[0])
	}
	if got[4] != "stale" {
		t.Errorf("reap mode argument = %q", got[4])
	}
	if plistProgramArguments("<plist></plist>") != nil {
		t.Error("a plist with no ProgramArguments should yield nothing")
	}
}

func TestSelectForReapNeverTakesOurOwnSession(t *testing.T) {
	ours := &liveSession{Session: ourSession(1)}
	ours.Idle, ours.IdleKnown = 10*time.Hour, true
	idle := &liveSession{Session: newTestSession(2)}
	idle.Idle, idle.IdleKnown = 3*time.Hour, true
	fresh := &liveSession{Session: newTestSession(3)}
	fresh.Idle, fresh.IdleKnown = time.Minute, true
	unknown := &liveSession{Session: newTestSession(4)}
	found := []*liveSession{ours, idle, fresh, unknown}

	got := selectForReap(found, reapCriteria{Idle: 2 * time.Hour})
	if len(got) != 1 || got[0].PID != 2 {
		t.Fatalf("idle selection = %v, want just pid 2", pidsOf(got))
	}

	// --all still refuses our own session, and still skips nothing else.
	got = selectForReap(found, reapCriteria{IgnoreIdle: true})
	if pids := pidsOf(got); len(pids) != 3 || contains(pids, 1) {
		t.Errorf("--all selection = %v, want the three sessions that are not ours", pids)
	}

	// An explicit pid cannot reach our own session either.
	if got := selectForReap(found, reapCriteria{PIDs: map[int]bool{1: true}}); len(got) != 0 {
		t.Errorf("selecting our own session by pid returned %v", pidsOf(got))
	}
}

func TestSelectForReapStaleOnly(t *testing.T) {
	stale := &liveSession{Session: newTestSession(2), Stale: true, StaleKnown: true}
	stale.Idle, stale.IdleKnown = 3*time.Hour, true
	current := &liveSession{Session: newTestSession(3), StaleKnown: true}
	current.Idle, current.IdleKnown = 3*time.Hour, true

	got := selectForReap([]*liveSession{stale, current}, reapCriteria{Idle: 2 * time.Hour, StaleOnly: true})
	if len(got) != 1 || got[0].PID != 2 {
		t.Fatalf("stale-only selection = %v, want just pid 2", pidsOf(got))
	}
}

func TestSelectForReapIgnoresUnmeasuredIdle(t *testing.T) {
	// A detached session with no transcript has no idle signal. The threshold
	// path must never guess at it.
	unknown := &liveSession{Session: newTestSession(9)}
	unknown.Uptime = 100 * time.Hour
	if got := selectForReap([]*liveSession{unknown}, reapCriteria{Idle: time.Hour}); len(got) != 0 {
		t.Errorf("a session with no idle signal was selected: %v", pidsOf(got))
	}
}

func pidsOf(list []*liveSession) []int {
	out := make([]int, len(list))
	for i, s := range list {
		out[i] = s.PID
	}
	return out
}

func contains(list []int, n int) bool {
	for _, v := range list {
		if v == n {
			return true
		}
	}
	return false
}
