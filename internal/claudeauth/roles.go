package claudeauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Roles is the caller's role within the organization and workspace.
type Roles struct {
	OrganizationUUID string  `json:"organization_uuid"`
	OrganizationName string  `json:"organization_name"`
	OrganizationRole string  `json:"organization_role"`
	WorkspaceUUID    *string `json:"workspace_uuid"`
	WorkspaceName    *string `json:"workspace_name"`
	WorkspaceRole    *string `json:"workspace_role"`
}

// FetchRoles reads the organization/workspace roles for an access token.
// It is best-effort: callers treat an error as "unknown roles", not a failure.
func (c Config) FetchRoles(ctx context.Context, accessToken string) (*Roles, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.APIBase+"/api/oauth/claude_cli/roles", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := c.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch roles: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch roles: HTTP %d: %s", resp.StatusCode, snippet(raw))
	}
	var r Roles
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("parse roles: %w", err)
	}
	return &r, nil
}
