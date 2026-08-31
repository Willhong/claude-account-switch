package claudeauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Window is one rate-limit bucket as the usage endpoint reports it.
type Window struct {
	Utilization      float64    `json:"utilization"`
	ResetsAt         *time.Time `json:"resets_at"`
	LimitDollars     *float64   `json:"limit_dollars"`
	UsedDollars      *float64   `json:"used_dollars"`
	RemainingDollars *float64   `json:"remaining_dollars"`
	LockedReason     *string    `json:"locked_reason"`
}

// LimitScope narrows a limit to a model or a surface.
type LimitScope struct {
	Model *struct {
		ID          *string `json:"id"`
		DisplayName string  `json:"display_name"`
	} `json:"model"`
	Surface *string `json:"surface"`
}

// Limit is one entry of the usage endpoint's `limits` array. This is the view
// Claude Code's own /usage screen is built from.
type Limit struct {
	Kind     string      `json:"kind"`  // session, weekly_all, weekly_scoped
	Group    string      `json:"group"` // session, weekly
	Percent  float64     `json:"percent"`
	Severity string      `json:"severity"` // normal, warning, critical
	ResetsAt *time.Time  `json:"resets_at"`
	Scope    *LimitScope `json:"scope"`
	IsActive bool        `json:"is_active"`
}

// Label names the column a limit belongs under: "5h" and "wk" for the two
// account-wide buckets, and the model name for a scoped one (e.g. "Fable").
func (l Limit) Label() string {
	switch l.Kind {
	case "session":
		return "5h"
	case "weekly_all":
		return "wk"
	}
	if l.Scope != nil {
		if l.Scope.Model != nil && l.Scope.Model.DisplayName != "" {
			return l.Scope.Model.DisplayName
		}
		if l.Scope.Surface != nil && *l.Scope.Surface != "" {
			return *l.Scope.Surface
		}
	}
	return l.Kind
}

// Usage is the subset of /api/oauth/usage that cas reports. The endpoint also
// returns a batch of experiment codenames, which are deliberately ignored.
type Usage struct {
	FiveHour *Window `json:"five_hour"`
	SevenDay *Window `json:"seven_day"`
	Limits   []Limit `json:"limits"`
}

// Find returns the limit with the given label, or nil.
func (u *Usage) Find(label string) *Limit {
	for i := range u.Limits {
		if u.Limits[i].Label() == label {
			return &u.Limits[i]
		}
	}
	return nil
}

// Labels lists the limit labels this response carries, in report order:
// the 5-hour bucket, the weekly total, then any scoped buckets.
func (u *Usage) Labels() []string {
	var scoped []string
	var has5h, hasWk bool
	for _, l := range u.Limits {
		switch l.Label() {
		case "5h":
			has5h = true
		case "wk":
			hasWk = true
		default:
			scoped = append(scoped, l.Label())
		}
	}
	var out []string
	if has5h {
		out = append(out, "5h")
	}
	if hasWk {
		out = append(out, "wk")
	}
	return append(out, scoped...)
}

// FetchUsage reads the rate-limit utilisation for an access token.
//
// at_wall=1 asks for utilisation as of now rather than as of the last request;
// skip_spend=1 drops the spend breakdown, which cas does not report.
func (c Config) FetchUsage(ctx context.Context, accessToken string) (*Usage, error) {
	url := c.APIBase + "/api/oauth/usage?at_wall=1&skip_spend=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := c.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch usage: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("fetch usage: the access token was rejected")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch usage: HTTP %d: %s", resp.StatusCode, snippet(raw))
	}
	var u Usage
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, fmt.Errorf("parse usage: %w", err)
	}
	return &u, nil
}
