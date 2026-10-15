package codexappserver

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestSnapshotNegotiatesProtocolAndReadsAccountLimits(t *testing.T) {
	client := Client{Command: helperCommand(t)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Account.Type != "chatgpt" || snapshot.Account.Email != "person@example.com" {
		t.Fatalf("account = %+v", snapshot.Account)
	}
	if snapshot.RateLimits.AccountID != "account-1" {
		t.Fatalf("account id = %q", snapshot.RateLimits.AccountID)
	}
	window := snapshot.RateLimits.RateLimits.Primary
	if window == nil || window.UsedPercent != 42 || window.WindowDurationMins == nil || *window.WindowDurationMins != 300 {
		t.Fatalf("primary window = %+v", window)
	}
}

func TestReadAccountDoesNotNeedTheRateLimitRequest(t *testing.T) {
	client := Client{Command: helperCommand(t)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	account, err := client.ReadAccount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if account.Email != "person@example.com" || account.PlanType != "pro" {
		t.Fatalf("account = %+v", account)
	}
}

func TestRefreshUsesTheAccountProtocol(t *testing.T) {
	client := Client{Command: helperCommand(t)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	account, err := client.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if account.Email != "person@example.com" {
		t.Fatalf("account = %+v", account)
	}
}

func helperCommand(t *testing.T) func(context.Context) *exec.Cmd {
	t.Helper()
	return func(ctx context.Context) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestAppServerHelperProcess")
		cmd.Env = append(os.Environ(), "GO_WANT_CODEX_APP_SERVER_HELPER=1")
		return cmd
	}
}

func TestAppServerHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_CODEX_APP_SERVER_HELPER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			os.Exit(2)
		}
		switch request.Method {
		case "initialize":
			_ = encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{
				"userAgent": "helper", "codexHome": "/tmp/helper", "platformFamily": "unix", "platformOs": "macos",
			}})
		case "initialized":
		case "account/read":
			_ = encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{
				"account":            map[string]any{"type": "chatgpt", "email": "person@example.com", "planType": "pro"},
				"requiresOpenaiAuth": true,
			}})
		case "account/rateLimits/read":
			_ = encoder.Encode(map[string]any{"method": "account/rateLimits/updated", "params": map[string]any{}})
			windowMinutes := int64(300)
			reset := time.Now().Add(time.Hour).Unix()
			_ = encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{
				"accountId": "account-1",
				"rateLimits": map[string]any{
					"limitId": "codex", "planType": "pro",
					"primary": map[string]any{"usedPercent": 42, "windowDurationMins": windowMinutes, "resetsAt": reset},
				},
			}})
		}
	}
	os.Exit(0)
}
