package app

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Willhong/claude-account-switch/internal/claudeauth"
	"github.com/Willhong/claude-account-switch/internal/store"
	"github.com/Willhong/claude-account-switch/internal/ui"
)

// CmdLimit reports how much of each account's rate limits is used up: the
// 5-hour session window, the weekly total, and any per-model weekly budget
// (Fable today).
func CmdLimit(args []string) error {
	fs := flag.NewFlagSet("cas limit", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the usage report as JSON")
	wide := fs.Bool("wide", false, "one block per account, with reset times and usage bars")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}

	a, err := New()
	if err != nil {
		return err
	}
	defer a.Close()

	a.SyncActive()

	slots := a.State.Slots
	if len(positional) > 0 {
		slots = nil
		for _, ref := range positional {
			s, err := a.ResolveSlot(ref)
			if err != nil {
				return err
			}
			slots = append(slots, s)
		}
	}
	if len(slots) == 0 {
		ui.Infof("No accounts registered. Run `cas adopt` or `cas login` first.")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The usage endpoint needs a live access token, so rotate anything stale
	// before asking. This is the same pass every other command runs.
	a.AutoRefresh(ctx, true)
	if err := a.Save(); err != nil {
		return err
	}

	reports := a.fetchUsage(ctx, slots)

	if *asJSON {
		return writeUsageJSON(reports)
	}
	if *wide {
		renderUsageBlocks(reports, a.State.ActiveSlot)
	} else {
		renderUsageTable(reports, a.State.ActiveSlot)
	}

	var failed int
	for _, r := range reports {
		if r.Err != nil {
			failed++
			ui.Errorf("slot %d (%s): %v", r.Slot.N, r.Slot.Name(), r.Err)
		}
	}
	if failed == len(reports) {
		return fmt.Errorf("no account's usage could be read")
	}
	return nil
}

// usageReport pairs a slot with its usage, or with why it could not be read.
type usageReport struct {
	Slot  *store.Slot
	Usage *claudeauth.Usage
	Err   error
}

// fetchUsage queries every slot concurrently; one slow account does not hold
// up the rest.
func (a *App) fetchUsage(ctx context.Context, slots []*store.Slot) []usageReport {
	reports := make([]usageReport, len(slots))
	var wg sync.WaitGroup

	for i, s := range slots {
		reports[i].Slot = s

		env, err := a.Store.ReadCred(s.N)
		if err != nil {
			reports[i].Err = err
			continue
		}
		if env.OAuth.AccessExpired(0) {
			reports[i].Err = fmt.Errorf("access token expired; run `cas refresh %d`", s.N)
			continue
		}

		wg.Add(1)
		go func(i int, token string) {
			defer wg.Done()
			u, err := a.OAuth.FetchUsage(ctx, token)
			reports[i].Usage, reports[i].Err = u, err
		}(i, env.OAuth.AccessToken)
	}
	wg.Wait()
	return reports
}

// usageColumns collects the limit labels across every account, so an account
// that has a bucket the others lack still gets a column.
func usageColumns(reports []usageReport) []string {
	seen := map[string]bool{}
	var scoped []string
	for _, r := range reports {
		if r.Usage == nil {
			continue
		}
		for _, label := range r.Usage.Labels() {
			if seen[label] {
				continue
			}
			seen[label] = true
			if label != "5h" && label != "wk" {
				scoped = append(scoped, label)
			}
		}
	}
	sort.Strings(scoped)

	var cols []string
	if seen["5h"] {
		cols = append(cols, "5h")
	}
	if seen["wk"] {
		cols = append(cols, "wk")
	}
	return append(cols, scoped...)
}

func renderUsageTable(reports []usageReport, activeSlot int) {
	cols := usageColumns(reports)
	if len(cols) == 0 {
		return
	}

	header := []string{"", "SLOT", "EMAIL"}
	for _, c := range cols {
		header = append(header, strings.ToUpper(c))
	}
	t := &ui.Table{Header: header}

	for _, r := range reports {
		marker := " "
		if r.Slot.N == activeSlot {
			marker = ui.Green("*")
		}
		row := []string{marker, fmt.Sprintf("%d", r.Slot.N), r.Slot.Email}

		for _, c := range cols {
			switch {
			case r.Usage == nil:
				row = append(row, ui.Dim("—"))
			default:
				row = append(row, usageCell(r.Usage.Find(c)))
			}
		}
		t.Rows = append(t.Rows, row)
	}
	t.Render(os.Stdout)
}

// usageCell renders "57% · 2d 5h": how much is spent, and how long until the
// window rolls over.
func usageCell(l *claudeauth.Limit) string {
	if l == nil {
		return ui.Dim("—")
	}
	text := fmt.Sprintf("%3.0f%%", l.Percent)
	if l.ResetsAt != nil {
		if d := time.Until(*l.ResetsAt); d > 0 {
			text += ui.Dim(" · " + ui.Duration(d))
		} else {
			text += ui.Dim(" · due")
		}
	}
	return colorForLimit(*l, text)
}

// colorForLimit trusts the API's own severity, falling back to the percentage
// when a future severity value is one cas does not know.
func colorForLimit(l claudeauth.Limit, text string) string {
	switch l.Severity {
	case "critical":
		return ui.Red(text)
	case "warning":
		return ui.Yellow(text)
	case "normal":
		if l.Percent >= 100 {
			return ui.Red(text)
		}
		return text
	}
	switch {
	case l.Percent >= 100:
		return ui.Red(text)
	case l.Percent >= 80:
		return ui.Yellow(text)
	default:
		return text
	}
}

func renderUsageBlocks(reports []usageReport, activeSlot int) {
	for i, r := range reports {
		if i > 0 {
			fmt.Println()
		}
		marker := ""
		if r.Slot.N == activeSlot {
			marker = ui.Green(" (active)")
		}
		fmt.Printf("slot %d  %s%s\n", r.Slot.N, ui.Bold(r.Slot.Email), marker)

		if r.Usage == nil {
			fmt.Printf("  %s\n", ui.Dim("usage unavailable"))
			continue
		}
		for _, label := range r.Usage.Labels() {
			l := r.Usage.Find(label)
			if l == nil {
				continue
			}
			reset := ui.Dim("—")
			if l.ResetsAt != nil {
				reset = ui.RelativeTime(*l.ResetsAt) + ui.Dim(" ("+l.ResetsAt.Local().Format("Mon 15:04")+")")
			}
			fmt.Printf("  %-8s %s %s  resets %s\n",
				label, bar(l.Percent), colorForLimit(*l, fmt.Sprintf("%3.0f%%", l.Percent)), reset)
		}
	}
}

// bar draws a 20-cell utilisation meter.
func bar(percent float64) string {
	const width = 20
	filled := int(percent/100*width + 0.5)
	filled = min(max(filled, 0), width)

	full := strings.Repeat("█", filled)
	switch {
	case percent >= 100:
		full = ui.Red(full)
	case percent >= 80:
		full = ui.Yellow(full)
	}
	return full + ui.Dim(strings.Repeat("░", width-filled))
}

// writeUsageJSON emits a stable, script-friendly shape rather than the raw API
// response, whose experiment codenames are noise.
func writeUsageJSON(reports []usageReport) error {
	type jsonLimit struct {
		Label    string     `json:"label"`
		Kind     string     `json:"kind"`
		Percent  float64    `json:"percent"`
		Severity string     `json:"severity,omitempty"`
		ResetsAt *time.Time `json:"resetsAt,omitempty"`
		IsActive bool       `json:"isActive"`
	}
	type jsonAccount struct {
		Slot   int         `json:"slot"`
		Label  string      `json:"label,omitempty"`
		Email  string      `json:"email"`
		Limits []jsonLimit `json:"limits,omitempty"`
		Error  string      `json:"error,omitempty"`
	}

	out := make([]jsonAccount, 0, len(reports))
	for _, r := range reports {
		acct := jsonAccount{Slot: r.Slot.N, Label: r.Slot.Label, Email: r.Slot.Email}
		if r.Err != nil {
			acct.Error = r.Err.Error()
		}
		if r.Usage != nil {
			for _, l := range r.Usage.Limits {
				acct.Limits = append(acct.Limits, jsonLimit{
					Label: l.Label(), Kind: l.Kind, Percent: l.Percent,
					Severity: l.Severity, ResetsAt: l.ResetsAt, IsActive: l.IsActive,
				})
			}
		}
		out = append(out, acct)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
