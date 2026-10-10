package cloudcli

import (
	stdcontext "context"
	"fmt"
	"strings"
	"time"

	"go.putnami.dev/client"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// tokenExpiringWithin is how close to its expiry a machine token counts as
// expiring.
const tokenExpiringWithin = 7 * 24 * time.Hour

// machineTokensStatus backs `putnami cloud token status`: one child
// per workspace machine token that is not revoked.
func machineTokensStatus(params map[string]any, workspaceRoot string, env map[string]string, ioctx IO) error {
	rows, err := readMachineTokens(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	return clicore.WriteStatus(params, ioctx, TokenStatusNodeFrom(rows, tokenStatusNow(ioctx)))
}

// TokenStatusNode is the token line of `putnami cloud status`: whether a
// workspace machine token has expired or expires within 7 days. A read that
// fails makes the node unknown.
func TokenStatusNode(params map[string]any, workspaceRoot string, env map[string]string, ioctx IO) clicore.StatusNode {
	rows, err := readMachineTokens(params, workspaceRoot, env, ioctx)
	if err != nil {
		return clicore.UserSessionStatus("token", "token", err, env)
	}
	return TokenStatusNodeFrom(rows, tokenStatusNow(ioctx))
}

// readMachineTokens lists the linked workspace's machine tokens, revoked rows
// included, bound to the command's context.
func readMachineTokens(params map[string]any, workspaceRoot string, env map[string]string, ioctx IO) ([]map[string]any, error) {
	workspaceID, accessToken, controlURL, err := machineTokenContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return nil, err
	}
	base := ioctx.Context
	if base == nil {
		base = stdcontext.Background()
	}
	return listMachineTokensIn(client.WithForwardedUserToken(base, accessToken), ioctx.Client, controlURL, workspaceID)
}

// TokenStatusNodeFrom folds the machine tokens into the token node. A live
// token is ok. One that expires within 7 days is degraded, with the command
// that creates its successor. One past its expiry that nobody revoked is
// degraded, with the command that revokes it. Revoked tokens are left out.
func TokenStatusNodeFrom(rows []map[string]any, now time.Time) clicore.StatusNode {
	node := clicore.StatusNode{ID: "token", Title: "token"}
	active, expiring, expired := 0, 0, 0
	for _, row := range rows {
		if strings.TrimSpace(valueString(row, "revoked_at")) != "" {
			continue
		}
		child, state := machineTokenNode(row, now)
		switch state {
		case "expired":
			expired++
		case "expiring":
			active++
			expiring++
		default:
			active++
		}
		node.Children = append(node.Children, child)
	}
	node.State = clicore.WorstStatus(node.ChildStates()...)
	switch {
	case len(node.Children) == 0:
		node.Detail = "no workspace machine token"
	default:
		parts := []string{fmt.Sprintf("%d active", active)}
		if expiring > 0 {
			parts = append(parts, fmt.Sprintf("%d expiring within 7 days", expiring))
		}
		if expired > 0 {
			parts = append(parts, fmt.Sprintf("%d expired, not revoked", expired))
		}
		node.Detail = strings.Join(parts, ", ")
	}
	for _, child := range node.Children {
		if child.Fix != "" {
			node.Fix = child.Fix
			break
		}
	}
	node.Metrics = []clicore.StatusMetric{
		clicore.CountMetric("tokens_active", "active machine tokens", active, clicore.MetricUsage),
		clicore.CountMetric("tokens_expiring", "machine tokens expiring within 7 days", expiring, clicore.MetricHealth),
	}
	return node
}

// machineTokenNode is one token: its scopes, its expiry and its last use.
// The second value is "expired", "expiring" or "active".
func machineTokenNode(row map[string]any, now time.Time) (clicore.StatusNode, string) {
	id := valueString(row, "id")
	name := clicore.FirstString(valueString(row, "name"), id)
	node := clicore.StatusNode{ID: "token." + id, Title: name, State: clicore.StatusOK}
	scopes := strings.Fields(valueString(row, "allowed_scopes"))
	parts := []string{}
	if len(scopes) > 0 {
		parts = append(parts, "scopes "+strings.Join(scopes, " "))
	}
	state := "active"
	expiresAt := strings.TrimSpace(valueString(row, "expires_at"))
	expiry, parsed := time.Time{}, false
	if expiresAt != "" {
		if at, err := time.Parse(time.RFC3339, expiresAt); err == nil {
			expiry, parsed = at, true
		}
	}
	switch {
	case expiresAt == "":
		parts = append(parts, "never expires")
	case !parsed:
		parts = append(parts, "expires "+expiresAt)
	case !expiry.After(now):
		state = "expired"
		node.State = clicore.StatusDegraded
		parts = append(parts, "expired "+shortTime(expiresAt)+", not revoked")
		node.Fix = "putnami cloud token revoke " + id
	case expiry.Sub(now) <= tokenExpiringWithin:
		state = "expiring"
		node.State = clicore.StatusDegraded
		parts = append(parts, "expires "+shortTime(expiresAt))
		node.Fix = "putnami cloud token create --name " + name + " --scopes " + strings.Join(scopes, ",")
	default:
		parts = append(parts, "expires "+shortTime(expiresAt))
	}
	if lastUsed := strings.TrimSpace(valueString(row, "last_used_at")); lastUsed != "" {
		parts = append(parts, "last used "+shortTime(lastUsed))
	} else {
		parts = append(parts, "never used")
	}
	node.Detail = strings.Join(parts, ", ")
	return node, state
}

// tokenStatusNow is the clock of the status read.
func tokenStatusNow(ioctx IO) time.Time {
	if ioctx.Now != nil {
		return ioctx.Now()
	}
	return time.Now()
}
