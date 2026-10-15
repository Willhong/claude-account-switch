package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Willhong/claude-account-switch/internal/codexappserver"
	"github.com/Willhong/claude-account-switch/internal/codexstore"
	"github.com/Willhong/claude-account-switch/internal/codextarget"
	"github.com/Willhong/claude-account-switch/internal/ui"
)

func cmdCodexLimit(args []string) error {
	fs := flag.NewFlagSet("cas codex limit", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the Codex usage report as JSON")
	wide := fs.Bool("wide", false, "one block per Codex account, with reset times and usage bars")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}

	a, err := newCodexApp()
	if err != nil {
		return err
	}
	defer a.Close()
	if a.SyncActive() {
		if err := a.Save(); err != nil {
			return err
		}
	}

	slots := a.State.Slots
	if len(positional) > 0 {
		slots = nil
		for _, ref := range positional {
			slot, err := a.State.Resolve(ref)
			if err != nil {
				return err
			}
			slots = append(slots, slot)
		}
	}
	if len(slots) == 0 {
		ui.Infof("No Codex accounts registered. Run `cas codex adopt` or `cas codex login` first.")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	reports := make([]codexLimitReport, len(slots))
	for i, slot := range slots {
		reports[i] = a.fetchCodexLimit(ctx, slot)
	}
	if err := a.Save(); err != nil {
		return err
	}

	if *asJSON {
		return writeCodexLimitJSON(reports)
	}
	if *wide {
		renderCodexLimitBlocks(reports, a.State.ActiveSlot)
	} else {
		renderCodexLimitTable(reports, a.State.ActiveSlot)
	}

	failed := 0
	for _, report := range reports {
		if report.Err != nil {
			failed++
			ui.Errorf("Codex slot %d (%s): %v", report.Slot.N, report.Slot.Name(), report.Err)
		}
	}
	if failed == len(reports) {
		return errors.New("no Codex account's usage could be read")
	}
	return nil
}

type codexLimitReport struct {
	Slot     *codexstore.Slot
	Snapshot *codexappserver.Snapshot
	Err      error
}

func (a *codexApp) fetchCodexLimit(ctx context.Context, slot *codexstore.Slot) codexLimitReport {
	report := codexLimitReport{Slot: slot}
	doc, err := a.Store.ReadCredential(slot.N)
	if err != nil {
		report.Err = err
		return report
	}
	home, err := os.MkdirTemp("", fmt.Sprintf("cas-codex-slot-%d-*", slot.N))
	if err != nil {
		report.Err = err
		return report
	}
	defer os.RemoveAll(home)
	target := &codextarget.Target{Home: home, AuthPath: home + string(os.PathSeparator) + "auth.json"}
	if err := target.Write(doc); err != nil {
		report.Err = err
		return report
	}

	snapshot, callErr := (codexappserver.Client{Bin: codexBinOverride(), Home: home}).Snapshot(ctx)
	if updated, readErr := target.Read(); readErr == nil && updated.Identity() == doc.Identity() && updated.Fingerprint() != doc.Fingerprint() {
		if writeErr := a.Store.WriteCredential(slot, updated); writeErr != nil {
			report.Err = writeErr
			return report
		}
		slot.Apply(updated)
		if slot.N == a.State.ActiveSlot {
			if writeErr := a.Target.Write(updated); writeErr != nil {
				report.Err = writeErr
				return report
			}
		}
	}
	if callErr != nil {
		report.Err = callErr
		return report
	}
	if snapshot.RateLimits.AccountID != "" && slot.AccountID != "" && snapshot.RateLimits.AccountID != slot.AccountID {
		report.Err = fmt.Errorf("usage response belongs to account %s, expected %s", snapshot.RateLimits.AccountID, slot.AccountID)
		return report
	}
	if snapshot.Account.PlanType != "" {
		slot.Plan = snapshot.Account.PlanType
	}
	report.Snapshot = &snapshot
	return report
}

func renderCodexLimitTable(reports []codexLimitReport, activeSlot int) {
	columns := codexLimitColumns(reports)
	header := []string{"", "SLOT", "ACCOUNT", "PLAN"}
	header = append(header, columns...)
	if codexReportsHaveCredits(reports) {
		header = append(header, "CREDITS")
	}
	table := &ui.Table{Header: header}
	for _, report := range reports {
		marker := " "
		if report.Slot.N == activeSlot {
			marker = ui.Green("*")
		}
		row := []string{marker, fmt.Sprintf("%d", report.Slot.N), report.Slot.Account, codexValueOrDash(report.Slot.Plan)}
		windows := codexReportWindows(report)
		for _, column := range columns {
			row = append(row, codexRateLimitCell(windows[column]))
		}
		if codexReportsHaveCredits(reports) {
			row = append(row, codexCreditsCell(report))
		}
		table.Rows = append(table.Rows, row)
	}
	table.Render(os.Stdout)
}

func renderCodexLimitBlocks(reports []codexLimitReport, activeSlot int) {
	for i, report := range reports {
		if i > 0 {
			fmt.Println()
		}
		marker := ""
		if report.Slot.N == activeSlot {
			marker = ui.Green(" (active)")
		}
		fmt.Printf("Codex slot %d  %s%s\n", report.Slot.N, ui.Bold(report.Slot.Account), marker)
		if report.Err != nil || report.Snapshot == nil {
			fmt.Printf("  %s\n", ui.Dim("usage unavailable"))
			continue
		}
		windows := codexReportWindows(report)
		columns := make([]string, 0, len(windows))
		for column := range windows {
			columns = append(columns, column)
		}
		sortCodexLimitColumns(columns)
		for _, column := range columns {
			window := windows[column]
			reset := ui.Dim("—")
			if window.ResetsAt != nil {
				at := time.Unix(*window.ResetsAt, 0)
				reset = ui.RelativeTime(at) + ui.Dim(" ("+at.Local().Format("Mon 15:04")+")")
			}
			fmt.Printf("  %-12s %s %s  resets %s\n", column, bar(float64(window.UsedPercent)), codexPercent(window.UsedPercent), reset)
		}
		fmt.Printf("  %-12s %s\n", "credits", codexCreditsCell(report))
		if allowed := report.Snapshot.RateLimits.OrdinaryUsageAllowed; allowed != nil && !*allowed {
			fmt.Printf("  %-12s %s\n", "status", ui.Red("ordinary usage blocked"))
		}
	}
}

func codexLimitColumns(reports []codexLimitReport) []string {
	seen := map[string]bool{}
	var columns []string
	for _, report := range reports {
		for column := range codexReportWindows(report) {
			if !seen[column] {
				seen[column] = true
				columns = append(columns, column)
			}
		}
	}
	sortCodexLimitColumns(columns)
	return columns
}

func sortCodexLimitColumns(columns []string) {
	priority := func(column string) int {
		switch column {
		case "5H":
			return 0
		case "WK":
			return 1
		default:
			return 2
		}
	}
	sort.Slice(columns, func(i, j int) bool {
		pi, pj := priority(columns[i]), priority(columns[j])
		if pi != pj {
			return pi < pj
		}
		return columns[i] < columns[j]
	})
}

func codexReportWindows(report codexLimitReport) map[string]*codexappserver.RateLimitWindow {
	out := map[string]*codexappserver.RateLimitWindow{}
	if report.Snapshot == nil {
		return out
	}
	buckets := report.Snapshot.RateLimits.RateLimitsByLimitID
	if len(buckets) == 0 {
		bucket := report.Snapshot.RateLimits.RateLimits
		key := bucket.LimitID
		if key == "" {
			key = "codex"
		}
		buckets = map[string]codexappserver.RateLimitSnapshot{key: bucket}
	}
	for key, bucket := range buckets {
		if bucket.Primary != nil {
			out[codexWindowLabel(key, bucket, bucket.Primary, "PRIMARY")] = bucket.Primary
		}
		if bucket.Secondary != nil {
			out[codexWindowLabel(key, bucket, bucket.Secondary, "SECONDARY")] = bucket.Secondary
		}
	}
	return out
}

func codexWindowLabel(key string, bucket codexappserver.RateLimitSnapshot, window *codexappserver.RateLimitWindow, fallback string) string {
	label := fallback
	if window.WindowDurationMins != nil {
		switch minutes := *window.WindowDurationMins; minutes {
		case 300:
			label = "5H"
		case 10080:
			label = "WK"
		case 1440:
			label = "24H"
		default:
			if minutes%1440 == 0 {
				label = fmt.Sprintf("%dD", minutes/1440)
			} else if minutes%60 == 0 {
				label = fmt.Sprintf("%dH", minutes/60)
			} else {
				label = fmt.Sprintf("%dM", minutes)
			}
		}
	}
	if key == "" || key == "codex" {
		return label
	}
	prefix := bucket.NormalModelSlug
	if prefix == "" {
		prefix = key
	}
	return strings.ToUpper(prefix) + " " + label
}

func codexRateLimitCell(window *codexappserver.RateLimitWindow) string {
	if window == nil {
		return ui.Dim("—")
	}
	text := codexPercent(window.UsedPercent)
	if window.ResetsAt != nil {
		until := time.Until(time.Unix(*window.ResetsAt, 0))
		if until > 0 {
			text += ui.Dim(" · " + ui.Duration(until))
		} else {
			text += ui.Dim(" · due")
		}
	}
	return text
}

func codexPercent(percent int) string {
	text := fmt.Sprintf("%3d%%", percent)
	switch {
	case percent >= 100:
		return ui.Red(text)
	case percent >= 80:
		return ui.Yellow(text)
	default:
		return text
	}
}

func codexReportsHaveCredits(reports []codexLimitReport) bool {
	for _, report := range reports {
		if report.Snapshot != nil && report.Snapshot.RateLimits.RateLimits.Credits != nil {
			return true
		}
	}
	return false
}

func codexCreditsCell(report codexLimitReport) string {
	if report.Snapshot == nil {
		return ui.Dim("—")
	}
	credits := report.Snapshot.RateLimits.RateLimits.Credits
	if credits == nil {
		return ui.Dim("—")
	}
	if credits.Unlimited {
		return "unlimited"
	}
	if credits.Balance != nil {
		return *credits.Balance
	}
	if credits.HasCredits {
		return "available"
	}
	return "0"
}

func writeCodexLimitJSON(reports []codexLimitReport) error {
	type account struct {
		Slot       int                                          `json:"slot"`
		Label      string                                       `json:"label,omitempty"`
		Account    string                                       `json:"account"`
		Plan       string                                       `json:"plan,omitempty"`
		RateLimits *codexappserver.GetAccountRateLimitsResponse `json:"rateLimits,omitempty"`
		Error      string                                       `json:"error,omitempty"`
	}
	out := make([]account, 0, len(reports))
	for _, report := range reports {
		item := account{Slot: report.Slot.N, Label: report.Slot.Label, Account: report.Slot.Account, Plan: report.Slot.Plan}
		if report.Snapshot != nil {
			item.RateLimits = &report.Snapshot.RateLimits
		}
		if report.Err != nil {
			item.Error = report.Err.Error()
		}
		out = append(out, item)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(out)
}
