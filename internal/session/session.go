// Package session inspects the Claude Code processes running on this machine.
//
// A running Claude Code session reads the credential once, at startup, and
// then keeps it alive on its own schedule: when its access token nears expiry
// it spends the refresh token and writes the rotated pair back over the live
// keychain item. That is harmless while the session is the one the user is
// actually working in. It is not harmless for a session that has been sitting
// untouched in a forgotten terminal tab since before the last `cas switch` —
// that session still holds the *previous* account, and its next refresh
// silently reinstalls that account, invalidating the refresh token cas has
// stored for it along the way.
//
// This package finds those processes so cas can show them and shut them down.
package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// procName is the argv[0] basename Claude Code runs under.
const procName = "claude"

// Session is one running Claude Code process.
type Session struct {
	PID     int
	PPID    int
	TTY     string // "ttys004"; empty when the process has no controlling terminal
	Started time.Time
	Uptime  time.Duration
	CPU     time.Duration
	Args    string
	CWD     string // best effort; empty when it could not be determined

	// Idle is how long the session has gone without activity. IdleKnown is
	// false when no signal was available, which is the usual case for a
	// detached session started by another program.
	Idle      time.Duration
	IdleKnown bool

	// Ours marks a session cas is running inside, or one that shares this
	// terminal. Those are never candidates for reaping.
	Ours bool
}

// Detached reports whether the session has no controlling terminal — an SDK or
// subagent run rather than something a person is typing into.
func (s *Session) Detached() bool { return s.TTY == "" }

// TTYName renders the terminal for display.
func (s *Session) TTYName() string {
	if s.TTY == "" {
		return "—"
	}
	return s.TTY
}

// Options tunes a listing.
type Options struct {
	// ProjectsDir is Claude Code's transcript directory (~/.claude/projects).
	// It supplies the idle signal for sessions with no terminal.
	ProjectsDir string
	// Now overrides the clock in tests.
	Now func() time.Time
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// List returns every Claude Code process owned by this user, newest last.
func List(opts Options) ([]*Session, error) {
	out, err := exec.Command("/bin/ps", "-x", "-o", "pid=,ppid=,etime=,time=,tty=,args=").Output()
	if err != nil {
		return nil, err
	}
	return sessionsFrom(parsePS(string(out)), os.Getpid(), opts), nil
}

// sessionsFrom turns parsed ps rows into sessions. It is separated from List so
// tests can feed it fixed process tables.
func sessionsFrom(rows []procRow, self int, opts Options) []*Session {
	now := opts.now()
	parents := make(map[int]int, len(rows))
	byPID := make(map[int]procRow, len(rows))
	for _, r := range rows {
		parents[r.pid] = r.ppid
		byPID[r.pid] = r
	}

	mine := ancestors(parents, self)
	mine[self] = true
	selfTTY := byPID[self].tty

	var sessions []*Session
	var pids []int
	for _, r := range rows {
		if !isClaude(r.args) {
			continue
		}
		s := &Session{
			PID:     r.pid,
			PPID:    r.ppid,
			TTY:     r.tty,
			Started: now.Add(-r.elapsed),
			Uptime:  r.elapsed,
			CPU:     r.cpu,
			Args:    r.args,
			Ours:    mine[r.pid] || (r.tty != "" && r.tty == selfTTY),
		}
		sessions = append(sessions, s)
		pids = append(pids, r.pid)
	}

	cwds := workingDirs(pids)
	for _, s := range sessions {
		s.CWD = cwds[s.PID]
		s.Idle, s.IdleKnown = idleOf(s, opts.ProjectsDir, now)
	}
	return sessions
}

// idleOf measures how long a session has gone without activity.
//
// The terminal's own mtime — the same signal `w` reports as idle time — is the
// one to trust: it is per-process and it moves on both input and output. Only
// a session with no terminal falls back to its transcripts, which are shared
// by every session in the same directory and so can only ever make a session
// look busier than it is.
func idleOf(s *Session, projectsDir string, now time.Time) (time.Duration, bool) {
	idle, ok := ttyIdle(s.TTY, now)
	if !ok {
		idle, ok = transcriptIdle(projectsDir, s.CWD, now)
	}
	if !ok {
		return 0, false
	}
	// A session cannot have been idle for longer than it has existed.
	if idle > s.Uptime {
		idle = s.Uptime
	}
	return idle, true
}

// ttyIdle is how long ago a byte last moved over the session's terminal.
func ttyIdle(tty string, now time.Time) (time.Duration, bool) {
	if tty == "" {
		return 0, false
	}
	info, err := os.Stat(filepath.Join("/dev", tty))
	if err != nil {
		return 0, false
	}
	return nonNegative(now.Sub(info.ModTime())), true
}

// transcriptIdle is how long ago Claude Code last appended to a transcript for
// this working directory. Several sessions can share a directory, so this
// reads as "at least one session here was active then" — which is why it is
// only ever used to make a session look *more* recently active, never less.
func transcriptIdle(projectsDir, cwd string, now time.Time) (time.Duration, bool) {
	if projectsDir == "" || cwd == "" {
		return 0, false
	}
	entries, err := os.ReadDir(filepath.Join(projectsDir, Slug(cwd)))
	if err != nil {
		return 0, false
	}
	var newest time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	if newest.IsZero() {
		return 0, false
	}
	return nonNegative(now.Sub(newest)), true
}

// Slug is how Claude Code names the transcript directory for a project: every
// character that is not a letter or a digit becomes a dash.
func Slug(dir string) string {
	var b strings.Builder
	b.Grow(len(dir))
	for _, r := range dir {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// ancestors collects every pid above one, stopping at the first unknown parent.
func ancestors(parents map[int]int, pid int) map[int]bool {
	seen := map[int]bool{}
	for {
		parent, ok := parents[pid]
		if !ok || parent <= 1 || seen[parent] {
			return seen
		}
		seen[parent] = true
		pid = parent
	}
}

// isClaude reports whether a command line belongs to Claude Code itself rather
// than to one of the MCP servers or tools it spawns.
func isClaude(args string) bool {
	argv0 := args
	if i := strings.IndexByte(args, ' '); i >= 0 {
		argv0 = args[:i]
	}
	return filepath.Base(argv0) == procName
}

// workingDirs asks lsof for each process's current directory. It is best
// effort: lsof may be missing or refuse a process, and an empty answer only
// costs the listing a column.
func workingDirs(pids []int) map[int]string {
	if len(pids) == 0 {
		return nil
	}
	list := make([]string, len(pids))
	for i, p := range pids {
		list[i] = strconv.Itoa(p)
	}
	out, err := exec.Command("/usr/sbin/lsof", "-a", "-d", "cwd", "-Fpn", "-p", strings.Join(list, ",")).Output()
	if err != nil && len(out) == 0 {
		return nil
	}
	dirs := map[int]string{}
	var pid int
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(line[1:])
		case 'n':
			if pid != 0 {
				dirs[pid] = line[1:]
			}
		}
	}
	return dirs
}

// Signal sends sig to the session.
func (s *Session) Signal(sig syscall.Signal) error { return syscall.Kill(s.PID, sig) }

// Alive reports whether the process still exists.
func (s *Session) Alive() bool {
	err := syscall.Kill(s.PID, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// procRow is one line of ps output.
type procRow struct {
	pid     int
	ppid    int
	tty     string
	elapsed time.Duration
	cpu     time.Duration
	args    string
}

// parsePS reads `ps -o pid=,ppid=,etime=,time=,tty=,args=`. Every column but
// the last is a single whitespace-free field, so a plain split is enough.
func parsePS(out string) []procRow {
	var rows []procRow
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		tty := fields[4]
		if tty == "??" || tty == "?" || tty == "-" {
			tty = ""
		}
		rows = append(rows, procRow{
			pid:     pid,
			ppid:    ppid,
			tty:     tty,
			elapsed: parseElapsed(fields[2]),
			cpu:     parseCPU(fields[3]),
			args:    strings.Join(fields[5:], " "),
		})
	}
	return rows
}

// parseElapsed reads ps's etime: [[dd-]hh:]mm:ss.
func parseElapsed(s string) time.Duration {
	var days int
	if i := strings.IndexByte(s, '-'); i >= 0 {
		days, _ = strconv.Atoi(s[:i])
		s = s[i+1:]
	}
	d := parseClock(s)
	return time.Duration(days)*24*time.Hour + d
}

// parseCPU reads ps's time: [hh:]mm:ss.ff.
func parseCPU(s string) time.Duration { return parseClock(s) }

// parseClock reads a colon-separated clock, seconds last, with an optional
// fractional part on the seconds.
func parseClock(s string) time.Duration {
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0
	}
	var total time.Duration
	for i, p := range parts {
		unit := time.Second
		switch len(parts) - i {
		case 2:
			unit = time.Minute
		case 3:
			unit = time.Hour
		}
		f, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return 0
		}
		total += time.Duration(f * float64(unit))
	}
	return total
}

func nonNegative(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}
