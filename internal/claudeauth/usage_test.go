package claudeauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sampleUsage is a real /api/oauth/usage response, trimmed of the experiment
// codenames cas ignores but otherwise verbatim — including the fractional
// timestamps and the null-heavy scoped entries.
const sampleUsage = `{
 "five_hour": {"utilization": 100.0, "resets_at": "2026-08-31T02:00:00.014845+00:00",
   "limit_dollars": null, "used_dollars": null, "remaining_dollars": null, "locked_reason": null},
 "seven_day": {"utilization": 57.0, "resets_at": "2026-09-02T16:00:00.014868+00:00",
   "limit_dollars": null, "used_dollars": null, "remaining_dollars": null, "locked_reason": null},
 "seven_day_opus": null,
 "nimbus_quill": {"utilization": 0.0, "resets_at": null},
 "extra_usage": null,
 "limits": [
  {"kind": "session", "group": "session", "percent": 100, "severity": "critical",
   "resets_at": "2026-08-31T02:00:00.014845+00:00", "scope": null, "is_active": true},
  {"kind": "weekly_all", "group": "weekly", "percent": 57, "severity": "normal",
   "resets_at": "2026-09-02T16:00:00.014868+00:00", "scope": null, "is_active": false},
  {"kind": "weekly_scoped", "group": "weekly", "percent": 34, "severity": "normal",
   "resets_at": "2026-09-02T16:00:00.015058+00:00",
   "scope": {"model": {"id": null, "display_name": "Fable"}, "surface": null}, "is_active": false}
 ],
 "spend": null,
 "member_dashboard_available": false
}`

func TestUsageParsesTheThreeReportedBuckets(t *testing.T) {
	var u Usage
	if err := json.Unmarshal([]byte(sampleUsage), &u); err != nil {
		t.Fatal(err)
	}

	if got := u.Labels(); strings.Join(got, ",") != "5h,wk,Fable" {
		t.Errorf("Labels() = %v, want [5h wk Fable]", got)
	}

	five := u.Find("5h")
	if five == nil {
		t.Fatal("no 5h limit")
	}
	if five.Percent != 100 || five.Severity != "critical" || !five.IsActive {
		t.Errorf("5h = %+v", five)
	}
	if five.ResetsAt == nil || five.ResetsAt.UTC().Format("2006-01-02T15:04:05") != "2026-08-31T02:00:00" {
		t.Errorf("5h resets_at = %v", five.ResetsAt)
	}

	if wk := u.Find("wk"); wk == nil || wk.Percent != 57 {
		t.Errorf("wk = %+v", wk)
	}

	fable := u.Find("Fable")
	if fable == nil {
		t.Fatal("no Fable limit")
	}
	if fable.Percent != 34 || fable.Kind != "weekly_scoped" {
		t.Errorf("Fable = %+v", fable)
	}

	if u.FiveHour == nil || u.FiveHour.Utilization != 100 {
		t.Errorf("five_hour = %+v", u.FiveHour)
	}
	if u.SevenDay == nil || u.SevenDay.Utilization != 57 {
		t.Errorf("seven_day = %+v", u.SevenDay)
	}
}

func TestLimitLabelFallsBackWhenAScopeHasNoModel(t *testing.T) {
	surface := "cowork"
	l := Limit{Kind: "weekly_scoped", Scope: &LimitScope{Surface: &surface}}
	if got := l.Label(); got != "cowork" {
		t.Errorf("Label() = %q, want cowork", got)
	}

	bare := Limit{Kind: "some_future_kind"}
	if got := bare.Label(); got != "some_future_kind" {
		t.Errorf("Label() = %q, want the kind itself", got)
	}
}

func TestFindReturnsNilForAnAbsentBucket(t *testing.T) {
	var u Usage
	if err := json.Unmarshal([]byte(sampleUsage), &u); err != nil {
		t.Fatal(err)
	}
	if got := u.Find("Opus"); got != nil {
		t.Errorf("Find(Opus) = %+v, want nil", got)
	}
}

func TestFetchUsageSendsTheRightRequest(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAuth = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmtWrite(w, sampleUsage)
	}))
	defer srv.Close()

	cfg := NewConfig(func(string) string { return "" })
	cfg.APIBase = srv.URL

	u, err := cfg.FetchUsage(context.Background(), "tok123")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/oauth/usage" {
		t.Errorf("path = %s", gotPath)
	}
	if !strings.Contains(gotQuery, "at_wall=1") || !strings.Contains(gotQuery, "skip_spend=1") {
		t.Errorf("query = %s", gotQuery)
	}
	if gotAuth != "Bearer tok123" {
		t.Errorf("Authorization = %s", gotAuth)
	}
	if len(u.Limits) != 3 {
		t.Errorf("got %d limits, want 3", len(u.Limits))
	}
}

func TestFetchUsageReportsARejectedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	cfg := NewConfig(func(string) string { return "" })
	cfg.APIBase = srv.URL

	if _, err := cfg.FetchUsage(context.Background(), "bad"); err == nil {
		t.Error("expected an error for a 401")
	}
}

func fmtWrite(w http.ResponseWriter, s string) { _, _ = w.Write([]byte(s)) }
