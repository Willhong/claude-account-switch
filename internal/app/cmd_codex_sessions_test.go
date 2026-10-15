package app

import (
	"testing"
	"time"

	"github.com/Willhong/claude-account-switch/internal/session"
)

func TestSelectCodexForReapAlwaysProtectsAppServers(t *testing.T) {
	managed := &codexLiveSession{Session: &session.Session{PID: 1, Idle: 5 * time.Hour, IdleKnown: true}, AppServer: true}
	cli := &codexLiveSession{Session: &session.Session{PID: 2, Idle: 5 * time.Hour, IdleKnown: true}}
	selected := selectCodexForReap([]*codexLiveSession{managed, cli}, codexReapCriteria{IgnoreIdle: true})
	if len(selected) != 1 || selected[0].PID != 2 {
		t.Fatalf("selected = %+v, want only standalone CLI pid 2", selected)
	}
}

func TestSelectCodexForReapRequiresKnownIdle(t *testing.T) {
	unknown := &codexLiveSession{Session: &session.Session{PID: 3, Uptime: 24 * time.Hour}}
	if selected := selectCodexForReap([]*codexLiveSession{unknown}, codexReapCriteria{Idle: time.Hour}); len(selected) != 0 {
		t.Fatalf("unknown-idle session was selected: %+v", selected)
	}
}

func TestCodexProcessKindRecognizesBBPool(t *testing.T) {
	process := &codexLiveSession{Session: &session.Session{Args: `codex app-server -c model_provider="bb-account-pool"`}, AppServer: true, BBPool: true}
	if got := codexProcessKind(process); got != "bb-pool" {
		t.Fatalf("kind = %q", got)
	}
}
