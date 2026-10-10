package cloudcli

import (
	stdcontext "context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	identityapi "go.putnami.dev/cloud/clients/identity-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// machineTokens backs `putnami cloud token <create|list|revoke>`: the
// first-class flow for workspace-owned pkt_* machine tokens.
// It is the plural sibling of `cloud token` (singular), which mints an
// ephemeral bearer for scripts — the two must not be confused. Every call
// resolves the linked workspace and drives the CONTROL PLANE (CPA) at
// /v1/workspaces/{workspace}/tokens (POST/GET/DELETE) with the user's normal
// session. The CPA is the authoritative membership gate — it
// checks the caller belongs to the workspace, then proxies to the auth-server as
// a trusted backend — so no workspace-scoped-token dance is needed and a
// non-member simply gets the CPA's 403. The owner is pinned to the path
// workspace CPA-side, never the request body.
//
//	putnami cloud token status [--strict]
//	putnami cloud token create --name <n> --scopes <s,...> [--allowed-clients <name,...>] [--allowed-client-ids <uuid,...>] [--expires <dur>]
//	putnami cloud token list
//	putnami cloud token revoke <id|name>
func machineTokens(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx IO) error {
	switch sub := clicore.FirstPositional(args); sub {
	case "create":
		return machineTokensCreate(params, workspaceRoot, env, ioctx)
	case "list":
		return machineTokensList(params, workspaceRoot, env, ioctx)
	case "revoke":
		return machineTokensRevoke(params, args, workspaceRoot, env, ioctx)
	case "status":
		return machineTokensStatus(params, workspaceRoot, env, ioctx)
	case "", "help":
		return machineTokensHelp(params, ioctx)
	default:
		return newError("unknown tokens subcommand: "+sub+`; expected "status", "create", "list", or "revoke"`, ExitUsage)
	}
}

func machineTokensHelp(params map[string]any, ioctx IO) error {
	commands := []map[string]string{
		{"command": "cloud token status", "description": "check the workspace's machine tokens: expired but not revoked, or expiring within 7 days"},
		{"command": "cloud token create", "description": "create a workspace-owned machine token (pkt_*) with chosen scopes; the raw token is shown exactly once"},
		{"command": "cloud token list", "description": "list the workspace's machine tokens (never the secret)"},
		{"command": "cloud token revoke", "description": "revoke a workspace machine token by id or name"},
	}
	if clicore.StructuredOutput(params) {
		writeResult(map[string]any{"commands": commands}, params, ioctx, "")
		return nil
	}
	ioctx.Stdout("@putnami/cloud token commands:")
	for _, item := range commands {
		ioctx.Stdout(fmt.Sprintf("  putnami %-21s %s", item["command"], item["description"]))
	}
	return nil
}

// machineTokenContext resolves the linked workspace id, the caller's normal
// session access token, and the control-plane (CPA) base URL shared by every
// tokens subcommand. Management now flows through the CPA, which
// performs the membership check with the user's ordinary session — so this uses
// activeAuth, NOT the old workspace-scoped-token dance. A workspace-owned key
// REQUIRES the workspace id, so an unlinked checkout is a usage error.
func machineTokenContext(params map[string]any, workspaceRoot string, env map[string]string, ioctx IO) (workspaceID, accessToken, controlURL string, err error) {
	link, err := readCloudLink(workspaceRoot)
	if err != nil {
		return "", "", "", err
	}
	workspaceID = strings.TrimSpace(stringValue(link["workspace_id"]))
	if workspaceID == "" {
		return "", "", "", newError("Putnami Cloud workspace config missing workspace_id; run `putnami cloud setup --workspace <id>`", ExitUsage)
	}
	auth, err := activeAuth(params, env, ioctx)
	if err != nil {
		return "", "", "", err
	}
	return workspaceID, auth.AccessToken, controlPlaneBaseURL(params, env, valueString(link, "control_plane_url")), nil
}

// workspaceTokensURL is the CPA route base for a workspace's machine tokens.
func workspaceTokensURL(controlURL, workspaceID string) string {
	return controlURL + "/v1/workspaces/" + urlPathEscape(workspaceID) + "/tokens"
}

func machineTokensCreate(params map[string]any, workspaceRoot string, env map[string]string, ioctx IO) error {
	name := strings.TrimSpace(stringParam(params, "name"))
	if name == "" {
		return newError("cloud token create requires --name <name>", ExitUsage)
	}
	scopes := normalizeScopes(stringParam(params, "scopes"))
	if scopes == "" {
		return newError("cloud token create requires --scopes <scope,...>", ExitUsage)
	}
	workspaceID, accessToken, controlURL, err := machineTokenContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	// The CPA pins owner_kind/owner_principal_id/workspace_id to the authorized
	// path workspace, so the body carries only the member-chosen fields — sending
	// an owner selector would be ignored and could read as an attempt to mint for
	// another workspace.
	body := identityapi.CreateWorkspaceTokenRequest{Name: &name, AllowedScopes: &scopes}
	if raw := strings.TrimSpace(stringParam(params, "allowed-client-ids")); raw != "" {
		ids := strings.Fields(strings.ReplaceAll(raw, ",", " "))
		body.AllowedClientIds = &ids
	}
	// --allowed-clients names clients by their public client id (for example
	// review-worker). The auth server resolves each name to its row and unions
	// the result with --allowed-client-ids, so nobody has to look up a UUID.
	if raw := strings.TrimSpace(stringParam(params, "allowed-clients")); raw != "" {
		clients := strings.Fields(strings.ReplaceAll(raw, ",", " "))
		body.AllowedClients = &clients
	}
	if raw := strings.TrimSpace(stringParam(params, "expires")); raw != "" {
		expiresAt, err := machineTokenExpiresAt(raw, ioctx.Now())
		if err != nil {
			return err
		}
		body.ExpiresAt = &expiresAt
	}
	identity, err := identityClient(ioctx.Client, controlURL)
	if err != nil {
		return err
	}
	created, err := identity.CreateV1WorkspacesTokens(bearerContext(accessToken), identityapi.CreateV1WorkspacesTokensInput{
		Path: identityapi.CreateV1WorkspacesTokensPath{Workspace: workspaceID},
		Body: body,
	})
	if err != nil {
		return clicore.RequestError(workspaceTokensURL(controlURL, workspaceID), err)
	}
	resp, err := clicore.ResponseMap(created.Body)
	if err != nil {
		return err
	}
	rawToken := valueString(resp, "raw_token")
	if rawToken == "" {
		return newError("control plane returned an empty machine token", ExitAPI)
	}
	out := map[string]any{
		"id":             valueString(resp, "id"),
		"name":           firstString(valueString(resp, "name"), name),
		"prefix":         valueString(resp, "prefix"),
		"workspace_id":   workspaceID,
		"allowed_scopes": firstString(valueString(resp, "allowed_scopes"), scopes),
		"expires_at":     valueString(resp, "expires_at"),
		"created_at":     valueString(resp, "created_at"),
		"raw_token":      rawToken,
	}
	if clicore.StructuredOutput(params) {
		writeResult(out, params, ioctx, "")
		return nil
	}
	for _, line := range machineTokenCreateLines(name, workspaceID, rawToken) {
		ioctx.Stdout(line)
	}
	return nil
}

// machineTokenCreateLines renders the one-time human reveal: the raw token, the
// store-it-now warning, and paste-ready hints for the common consumers. This is
// the ONLY command that ever prints the secret.
func machineTokenCreateLines(name, workspaceID, rawToken string) []string {
	return []string{
		fmt.Sprintf("Created workspace machine token %q for workspace %s.", name, workspaceID),
		"",
		"  " + rawToken,
		"",
		"Store it now — this token is shown only once and cannot be retrieved again.",
		"",
		"Paste-ready:",
		"  # GitHub Actions repo/environment secret",
		fmt.Sprintf("  gh secret set PUTNAMI_CLOUD_TOKEN --body %q", rawToken),
		"  # Build cache token source",
		fmt.Sprintf("  export PUTNAMI_CACHE_TOKEN=%q", rawToken),
		"  # E2E / agent runner",
		fmt.Sprintf("  export PUTNAMI_E2E_TOKEN=%q", rawToken),
	}
}

func machineTokensList(params map[string]any, workspaceRoot string, env map[string]string, ioctx IO) error {
	workspaceID, accessToken, controlURL, err := machineTokenContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	rows, err := listMachineTokens(ioctx.Client, controlURL, accessToken, workspaceID)
	if err != nil {
		return err
	}
	rows = activeMachineTokens(rows)
	if clicore.StructuredOutput(params) {
		writeResult(map[string]any{"tokens": rows}, params, ioctx, "")
		return nil
	}
	if len(rows) == 0 {
		ioctx.Stdout("No workspace machine tokens.")
		return nil
	}
	ioctx.Stdout(fmt.Sprintf("%-24s %-28s %-12s %-12s %-12s %s", "NAME", "SCOPES", "CREATED", "EXPIRES", "LAST-USED", "ID"))
	for _, row := range rows {
		ioctx.Stdout(fmt.Sprintf("%-24s %-28s %-12s %-12s %-12s %s",
			truncate(valueString(row, "name"), 24),
			truncate(valueString(row, "allowed_scopes"), 28),
			shortTime(valueString(row, "created_at")),
			expiryLabel(valueString(row, "expires_at")),
			usageLabel(valueString(row, "last_used_at")),
			valueString(row, "id"),
		))
	}
	return nil
}

func machineTokensRevoke(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx IO) error {
	target := machineTokenTarget(args)
	if target == "" {
		return newError("cloud token revoke requires an <id> or <name>", ExitUsage)
	}
	workspaceID, accessToken, controlURL, err := machineTokenContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	rows, err := listMachineTokens(ioctx.Client, controlURL, accessToken, workspaceID)
	if err != nil {
		return err
	}
	id, err := resolveMachineTokenID(rows, target)
	if err != nil {
		return err
	}
	identity, err := identityClient(ioctx.Client, controlURL)
	if err != nil {
		return err
	}
	// An already-revoked token (404) converges: the user's intent holds.
	if _, err := identity.DeleteV1WorkspacesTokens(bearerContext(accessToken), identityapi.DeleteV1WorkspacesTokensInput{
		Path: identityapi.DeleteV1WorkspacesTokensPath{Workspace: workspaceID, Id: id},
	}); err != nil && clicore.ServiceStatus(err) != http.StatusNotFound {
		return clicore.RequestError(workspaceTokensURL(controlURL, workspaceID)+"/"+urlPathEscape(id), err)
	}
	writeResult(
		map[string]any{"revoked": true, "id": id, "workspace_id": workspaceID},
		params,
		ioctx,
		fmt.Sprintf("Revoked workspace machine token %s.", id),
	)
	return nil
}

// listMachineTokens fetches the linked workspace's machine tokens from the CPA,
// revoked rows included — `revoke` must resolve an already-revoked id so a
// re-run converges idempotently (the DELETE tolerates 404) instead of failing
// resolution. Display paths hide revoked rows via activeMachineTokens.
func listMachineTokens(client *http.Client, controlURL, accessToken, workspaceID string) ([]map[string]any, error) {
	return listMachineTokensIn(bearerContext(accessToken), client, controlURL, workspaceID)
}

// listMachineTokensIn is listMachineTokens under a caller's context, which
// carries the forwarded session.
func listMachineTokensIn(ctx stdcontext.Context, client *http.Client, controlURL, workspaceID string) ([]map[string]any, error) {
	identity, err := identityClient(client, controlURL)
	if err != nil {
		return nil, err
	}
	listed, err := identity.GetV1WorkspacesTokens(ctx, identityapi.GetV1WorkspacesTokensInput{
		Path: identityapi.GetV1WorkspacesTokensPath{Workspace: workspaceID},
	})
	if err != nil {
		return nil, clicore.RequestError(workspaceTokensURL(controlURL, workspaceID), err)
	}
	resp, err := clicore.ResponseMap(listed)
	if err != nil {
		return nil, err
	}
	raw, _ := resp["tokens"].([]any)
	rows := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// activeMachineTokens drops already-revoked rows: `list` never shows them, and
// name→id resolution never matches them (a live token may reuse the name).
func activeMachineTokens(rows []map[string]any) []map[string]any {
	active := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if strings.TrimSpace(valueString(row, "revoked_at")) == "" {
			active = append(active, row)
		}
	}
	return active
}

// resolveMachineTokenID maps a user-supplied <id|name> to a concrete key id.
// An exact id match wins — revoked rows included, so re-revoking an id is
// idempotent; otherwise a unique name match among live tokens is used.
// Ambiguous or missing names are usage errors so a revoke never guesses.
func resolveMachineTokenID(rows []map[string]any, target string) (string, error) {
	for _, row := range rows {
		if valueString(row, "id") == target {
			return target, nil
		}
	}
	matches := make([]string, 0, 1)
	for _, row := range activeMachineTokens(rows) {
		if valueString(row, "name") == target {
			matches = append(matches, valueString(row, "id"))
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", newError(fmt.Sprintf("no workspace machine token matches %q", target), ExitUsage)
	default:
		return "", newError(fmt.Sprintf("%q matches multiple machine tokens (%s); revoke by id", target, strings.Join(matches, ", ")), ExitUsage)
	}
}

// normalizeScopes accepts a comma- and/or space-separated scope list and
// returns the canonical space-joined string the auth server stores.
func normalizeScopes(raw string) string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return strings.Join(out, " ")
}

// machineTokenExpiresAt converts a --expires value into the RFC3339 timestamp
// the server expects. A duration (Go units plus `d` days / `w` weeks) is added
// to now; anything else is treated as an explicit timestamp and passed through.
func machineTokenExpiresAt(raw string, now time.Time) (string, error) {
	if d, ok := parseMachineTokenDuration(raw); ok {
		if d <= 0 {
			return "", newError("cloud token create --expires must be a positive duration", ExitUsage)
		}
		return now.Add(d).UTC().Format(time.RFC3339), nil
	}
	return raw, nil
}

func parseMachineTokenDuration(raw string) (time.Duration, bool) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return 0, false
	}
	switch {
	case strings.HasSuffix(raw, "d"):
		if n, err := strconv.Atoi(strings.TrimSuffix(raw, "d")); err == nil {
			return time.Duration(n) * 24 * time.Hour, true
		}
		return 0, false
	case strings.HasSuffix(raw, "w"):
		if n, err := strconv.Atoi(strings.TrimSuffix(raw, "w")); err == nil {
			return time.Duration(n) * 7 * 24 * time.Hour, true
		}
		return 0, false
	}
	if d, err := time.ParseDuration(raw); err == nil {
		return d, true
	}
	return 0, false
}

// machineTokenTarget returns the positional after the `revoke` subcommand,
// skipping the parent CLI's --putnamiContext token and string-flag values (the
// same walk clicore.FirstPositional performs, kept local because revoke takes a
// second positional).
func machineTokenTarget(args []string) string {
	seen := 0
	skipNext := false
	for i, raw := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if raw == "--putnamiContext" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(raw, "--") {
			name := strings.TrimPrefix(raw, "--")
			if strings.Contains(name, "=") || strings.HasPrefix(name, "no-") || clicore.IsBooleanFlag(name) {
				continue
			}
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				skipNext = true
			}
			continue
		}
		seen++
		if seen == 2 {
			return raw
		}
	}
	return ""
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	if max <= 1 {
		return value[:max]
	}
	return value[:max-1] + "…"
}

// shortTime renders an ISO timestamp as its date part for compact table output.
func shortTime(iso string) string {
	iso = strings.TrimSpace(iso)
	if iso == "" {
		return "-"
	}
	if len(iso) >= 10 {
		return iso[:10]
	}
	return iso
}

func expiryLabel(iso string) string {
	if strings.TrimSpace(iso) == "" {
		return "never"
	}
	return shortTime(iso)
}

func usageLabel(iso string) string {
	if strings.TrimSpace(iso) == "" {
		return "never"
	}
	return shortTime(iso)
}
