// Package codexappserver is a small JSONL client for the local Codex app-server.
// It uses the same account methods that back Codex CLI's /status view.
package codexappserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Client starts a short-lived Codex app-server against one CODEX_HOME.
type Client struct {
	Bin  string
	Home string

	// Command is a test seam. Production callers leave it nil.
	Command func(context.Context) *exec.Cmd
}

// Snapshot is the account and rate-limit state returned by one app-server.
type Snapshot struct {
	Account    Account
	RateLimits GetAccountRateLimitsResponse
}

// Account is the non-secret identity returned by account/read.
type Account struct {
	Type     string `json:"type"`
	Email    string `json:"email"`
	PlanType string `json:"planType"`
}

type getAccountResponse struct {
	Account            *Account `json:"account"`
	RequiresOpenAIAuth bool     `json:"requiresOpenaiAuth"`
}

// GetAccountRateLimitsResponse mirrors account/rateLimits/read.
type GetAccountRateLimitsResponse struct {
	AccountID             string                        `json:"accountId,omitempty"`
	OrdinaryUsageAllowed  *bool                         `json:"ordinaryUsageAllowed,omitempty"`
	RateLimits            RateLimitSnapshot             `json:"rateLimits"`
	RateLimitsByLimitID   map[string]RateLimitSnapshot  `json:"rateLimitsByLimitId,omitempty"`
	RateLimitResetCredits *RateLimitResetCreditsSummary `json:"rateLimitResetCredits,omitempty"`
}

// RateLimitSnapshot is one metered bucket such as the general Codex quota.
type RateLimitSnapshot struct {
	LimitID              string                     `json:"limitId,omitempty"`
	LimitName            string                     `json:"limitName,omitempty"`
	NormalModelSlug      string                     `json:"normalModelSlug,omitempty"`
	PlanType             string                     `json:"planType,omitempty"`
	Primary              *RateLimitWindow           `json:"primary,omitempty"`
	Secondary            *RateLimitWindow           `json:"secondary,omitempty"`
	Credits              *CreditsSnapshot           `json:"credits,omitempty"`
	IndividualLimit      *SpendControlLimitSnapshot `json:"individualLimit,omitempty"`
	SpendControlReached  *bool                      `json:"spendControlReached,omitempty"`
	RateLimitReachedType string                     `json:"rateLimitReachedType,omitempty"`
}

// RateLimitWindow contains utilization and reset information.
type RateLimitWindow struct {
	UsedPercent        int    `json:"usedPercent"`
	WindowDurationMins *int64 `json:"windowDurationMins,omitempty"`
	ResetsAt           *int64 `json:"resetsAt,omitempty"`
}

// CreditsSnapshot is the optional purchased-credit balance.
type CreditsSnapshot struct {
	HasCredits bool    `json:"hasCredits"`
	Unlimited  bool    `json:"unlimited"`
	Balance    *string `json:"balance,omitempty"`
}

// SpendControlLimitSnapshot is an optional per-user spend cap.
type SpendControlLimitSnapshot struct {
	Limit            string `json:"limit"`
	Used             string `json:"used"`
	RemainingPercent int    `json:"remainingPercent"`
	ResetsAt         int64  `json:"resetsAt"`
}

// RateLimitResetCreditsSummary reports available banked resets.
type RateLimitResetCreditsSummary struct {
	AvailableCount int `json:"availableCount"`
}

// Snapshot negotiates the app-server protocol and reads the active account and
// its current rate limits.
func (c Client) Snapshot(ctx context.Context) (Snapshot, error) {
	return c.read(ctx, true, false)
}

// ReadAccount starts a fresh app-server and reports which account it sees.
func (c Client) ReadAccount(ctx context.Context) (Account, error) {
	snapshot, err := c.read(ctx, false, false)
	return snapshot.Account, err
}

// Refresh proactively refreshes the active ChatGPT token before returning the
// account, matching account/read's refreshToken contract.
func (c Client) Refresh(ctx context.Context) (Account, error) {
	snapshot, err := c.read(ctx, false, true)
	return snapshot.Account, err
}

func (c Client) read(ctx context.Context, withRateLimits, refreshToken bool) (Snapshot, error) {
	cmd, err := c.command(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Snapshot{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Snapshot{}, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return Snapshot{}, fmt.Errorf("start Codex app-server: %w", err)
	}
	waited := false
	defer func() {
		if waited {
			return
		}
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	writer := bufio.NewWriter(stdin)
	call := func(id int, method string, params any, out any) error {
		request := map[string]any{"id": id, "method": method}
		if params != nil {
			request["params"] = params
		}
		if err := json.NewEncoder(writer).Encode(request); err != nil {
			return err
		}
		if err := writer.Flush(); err != nil {
			return err
		}
		for scanner.Scan() {
			var response rpcResponse
			if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
				return fmt.Errorf("parse Codex app-server response: %w", err)
			}
			if response.ID == nil || !response.ID.matches(id) {
				continue
			}
			if response.Error != nil {
				return fmt.Errorf("Codex app-server %s: %s", method, response.Error.Message)
			}
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(response.Result, out); err != nil {
				return fmt.Errorf("parse Codex app-server %s result: %w", method, err)
			}
			return nil
		}
		if err := scanner.Err(); err != nil {
			return err
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = "process ended before replying"
		}
		return errors.New(message)
	}

	var initialized map[string]any
	if err := call(1, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "cas", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": true},
	}, &initialized); err != nil {
		return Snapshot{}, err
	}
	if err := json.NewEncoder(writer).Encode(map[string]any{"method": "initialized"}); err != nil {
		return Snapshot{}, err
	}
	if err := writer.Flush(); err != nil {
		return Snapshot{}, err
	}

	var accountResponse getAccountResponse
	if err := call(2, "account/read", map[string]any{"refreshToken": refreshToken}, &accountResponse); err != nil {
		return Snapshot{}, err
	}
	if accountResponse.Account == nil {
		return Snapshot{}, errors.New("Codex app-server has no signed-in account")
	}
	var limits GetAccountRateLimitsResponse
	if withRateLimits {
		if err := call(3, "account/rateLimits/read", map[string]any{
			"excludeResetCreditDetails": true,
			"supportsLunaReserve":       false,
		}, &limits); err != nil {
			return Snapshot{}, err
		}
	}

	_ = stdin.Close()
	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		return Snapshot{}, fmt.Errorf("Codex app-server exited: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	waited = true
	return Snapshot{Account: *accountResponse.Account, RateLimits: limits}, nil
}

func (c Client) command(ctx context.Context) (*exec.Cmd, error) {
	if c.Command != nil {
		return c.Command(ctx), nil
	}
	bin := c.Bin
	if bin == "" {
		var err error
		bin, err = exec.LookPath("codex")
		if err != nil {
			return nil, errors.New("could not find `codex` on PATH")
		}
	}
	cmd := exec.CommandContext(ctx, bin, "app-server", "--stdio")
	cmd.Env = replaceEnv(os.Environ(), "CODEX_HOME", c.Home)
	return cmd, nil
}

type rpcResponse struct {
	ID     *rpcID          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Message string `json:"message"`
}

type rpcID string

func (id *rpcID) UnmarshalJSON(raw []byte) error {
	*id = rpcID(strings.Trim(string(raw), `"`))
	return nil
}

func (id rpcID) matches(want int) bool { return string(id) == strconv.Itoa(want) }

func replaceEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+value)
}
