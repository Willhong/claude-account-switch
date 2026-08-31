// Package ui holds the small amount of terminal formatting cas needs:
// colours that switch themselves off when stdout is not a terminal, a plain
// column-aligned table, and relative-time rendering for token expiries.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

var color = detectColor()

func detectColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// SetColor forces colour output on or off.
func SetColor(on bool) { color = on }

const (
	reset  = "\x1b[0m"
	bold   = "\x1b[1m"
	dim    = "\x1b[2m"
	red    = "\x1b[31m"
	green  = "\x1b[32m"
	yellow = "\x1b[33m"
	cyan   = "\x1b[36m"
)

func wrap(code, s string) string {
	if !color || s == "" {
		return s
	}
	return code + s + reset
}

func Bold(s string) string   { return wrap(bold, s) }
func Dim(s string) string    { return wrap(dim, s) }
func Red(s string) string    { return wrap(red, s) }
func Green(s string) string  { return wrap(green, s) }
func Yellow(s string) string { return wrap(yellow, s) }
func Cyan(s string) string   { return wrap(cyan, s) }

var timestamps bool

// SetTimestamps prefixes every message with an RFC3339 stamp. The launchd
// agent turns this on so ~/.cas/daemon.log stays readable over time.
func SetTimestamps(on bool) { timestamps = on }

func stamp() string {
	if !timestamps {
		return ""
	}
	return time.Now().Format(time.RFC3339) + " "
}

// Infof writes a normal progress line to stderr, keeping stdout clean for data.
func Infof(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s%s\n", stamp(), fmt.Sprintf(format, args...))
}

// Warnf writes a warning to stderr.
func Warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s%s %s\n", stamp(), Yellow("warning:"), fmt.Sprintf(format, args...))
}

// Errorf writes an error to stderr.
func Errorf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s%s %s\n", stamp(), Red("error:"), fmt.Sprintf(format, args...))
}

// OKf writes a success line to stderr.
func OKf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s%s %s\n", stamp(), Green("✓"), fmt.Sprintf(format, args...))
}

// Table renders rows in aligned columns. Cells may contain colour escapes;
// widths are measured on the visible text only.
type Table struct {
	Header []string
	Rows   [][]string
}

// Render writes the table to w.
func (t *Table) Render(w io.Writer) {
	cols := len(t.Header)
	for _, r := range t.Rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	widths := make([]int, cols)
	measure := func(row []string) {
		for i, cell := range row {
			if n := visibleWidth(cell); n > widths[i] {
				widths[i] = n
			}
		}
	}
	if len(t.Header) > 0 {
		measure(t.Header)
	}
	for _, r := range t.Rows {
		measure(r)
	}

	write := func(row []string, style func(string) string) {
		var b strings.Builder
		for i, cell := range row {
			text := cell
			if style != nil {
				text = style(cell)
			}
			b.WriteString(text)
			if i < len(row)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-visibleWidth(cell)+2))
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}

	if len(t.Header) > 0 {
		write(t.Header, Dim)
	}
	for _, r := range t.Rows {
		write(r, nil)
	}
}

func visibleWidth(s string) int {
	var n int
	inEscape := false
	for _, r := range s {
		switch {
		case inEscape:
			if r == 'm' {
				inEscape = false
			}
		case r == '\x1b':
			inEscape = true
		default:
			n++
		}
	}
	_ = utf8.RuneCountInString
	return n
}

// RelativeTime renders how far t is from now, e.g. "in 7h 12m" or
// "expired 3d ago". A zero time renders as "unknown".
func RelativeTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Until(t)
	if d <= 0 {
		return "expired " + Duration(-d) + " ago"
	}
	return "in " + Duration(d)
}

// Duration renders a duration with at most two units.
func Duration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60

	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case days > 0:
		return fmt.Sprintf("%dd", days)
	case hours > 0 && mins > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	case hours > 0:
		return fmt.Sprintf("%dh", hours)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}
