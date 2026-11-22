package cloudcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// orgtokens.go backs `putnami cloud token create|list|revoke --organization
// <id>`: the organization's ingest tokens, the pkt_* key a repository's own CI
// uploads its evidence with. identity-api serves them at
// /v1/organizations/{organization}/tokens on the control plane, for a person
// who holds platform.organization.manage there. The token belongs to the
// organization, carries no workspace and the delivery.ingest scope only, so
// these commands need no workspace link and run outside any workspace; inside
// one, they read only the link's control-plane URL.
//
// The calls are hand-written: the published Cloud CLI schema this repository
// generates identity-api's client from does not carry the organization token
// routes yet (clientgen.framework.json lists them as pending). Their wire
// contract is identity-api's: create takes {name, expires_at?} and answers
// 201 with the one-time raw_token; list answers {organization_id, tokens};
// delete answers {revoked, id, organization_id}.
//
// Credential custody: the raw token leaves this file only through the create
// reveal. No error built here quotes an answer body, so a malformed answer that
// carries the token never prints it.

// organizationIngestScope is the one scope identity-api gives an organization
// token.
const organizationIngestScope = "delivery.ingest"

// organizationParam is the --organization flag, "" when the command targets
// the linked workspace.
func organizationParam(params map[string]any) string {
	return strings.TrimSpace(stringParam(params, "organization"))
}

// organizationTokenContext resolves the caller's session and the control-plane
// base URL. It reads the workspace link only for its control-plane URL, when
// there is one: an organization token needs no workspace.
func organizationTokenContext(params map[string]any, workspaceRoot string, env map[string]string, ioctx IO) (accessToken, controlURL string, err error) {
	auth, err := activeAuth(params, env, ioctx)
	if err != nil {
		return "", "", err
	}
	linked := ""
	if workspaceRoot != "" {
		if link, err := readCloudLink(workspaceRoot); err == nil {
			linked = valueString(link, "control_plane_url")
		}
	}
	return auth.AccessToken, controlPlaneBaseURL(params, env, linked), nil
}

func organizationTokensURL(controlURL, organizationID string) string {
	return controlURL + "/v1/organizations/" + urlPathEscape(organizationID) + "/tokens"
}

// organizationTokenCall sends one call to the organization token routes and
// decodes its JSON object answer. A refusal names the route and the server's
// reason; a 403 also names the permission the route needs. Neither a refusal
// nor an undecodable answer quotes the body.
func organizationTokenCall(client *http.Client, method, target, accessToken string, body any, success int) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, newError("marshal request body: "+err.Error(), ExitAPI)
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, target, reader)
	if err != nil {
		return nil, clicore.RequestBuildError(target, err)
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	clicore.SetUserAgent(request)
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, newError(fmt.Sprintf("request failed for %s: %s", target, err.Error()), ExitAPI)
	}
	defer response.Body.Close()
	text, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, newError(fmt.Sprintf("request failed for %s: %s", target, err.Error()), ExitAPI)
	}
	if response.StatusCode != success && !(success == http.StatusCreated && response.StatusCode == http.StatusOK) {
		return nil, organizationTokenRefusal(target, response.StatusCode, response.Status, text)
	}
	answer := map[string]any{}
	if len(bytes.TrimSpace(text)) > 0 {
		if err := json.Unmarshal(text, &answer); err != nil {
			return nil, newError(fmt.Sprintf("invalid JSON response from %s (status %d)", target, response.StatusCode), ExitAPI)
		}
	}
	return answer, nil
}

// refuseWorkspaceOnlyFlags refuses, before any call, the create flags an
// organization token cannot take: another scope than delivery.ingest and the
// client bindings of a workspace token.
func refuseWorkspaceOnlyFlags(params map[string]any) error {
	if scopes := normalizeScopes(stringParam(params, "scopes")); scopes != "" && scopes != organizationIngestScope {
		return newError(fmt.Sprintf("cloud token create --organization: an organization token carries the %s scope only, not %q; drop --scopes", organizationIngestScope, scopes), ExitUsage)
	}
	for _, flag := range []string{"allowed-clients", "allowed-client-ids"} {
		if strings.TrimSpace(stringParam(params, flag)) != "" {
			return newError("cloud token create --organization: an organization token takes no --"+flag, ExitUsage)
		}
	}
	return nil
}

func organizationTokensCreate(params map[string]any, organizationID, workspaceRoot string, env map[string]string, ioctx IO) error {
	name := strings.TrimSpace(stringParam(params, "name"))
	if name == "" {
		return newError("cloud token create requires --name <name>", ExitUsage)
	}
	if err := refuseWorkspaceOnlyFlags(params); err != nil {
		return err
	}
	body := map[string]any{"name": name}
	if raw := strings.TrimSpace(stringParam(params, "expires")); raw != "" {
		expiresAt, err := machineTokenExpiresAt(raw, ioctx.Now())
		if err != nil {
			return err
		}
		body["expires_at"] = expiresAt
	}
	accessToken, controlURL, err := organizationTokenContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	resp, err := organizationTokenCall(ioctx.Client, http.MethodPost, organizationTokensURL(controlURL, organizationID), accessToken, body, http.StatusCreated)
	if err != nil {
		return err
	}
	rawToken := valueString(resp, "raw_token")
	if rawToken == "" {
		return newError("control plane returned an empty organization token", ExitAPI)
	}
	organizationID = firstString(valueString(resp, "organization_id"), organizationID)
	out := map[string]any{
		"id":              valueString(resp, "id"),
		"name":            firstString(valueString(resp, "name"), name),
		"prefix":          valueString(resp, "prefix"),
		"organization_id": organizationID,
		"tenant_id":       valueString(resp, "tenant_id"),
		"allowed_scopes":  firstString(valueString(resp, "allowed_scopes"), organizationIngestScope),
		"expires_at":      valueString(resp, "expires_at"),
		"created_at":      valueString(resp, "created_at"),
		"raw_token":       rawToken,
	}
	if clicore.StructuredOutput(params) {
		writeResult(out, params, ioctx, "")
		return nil
	}
	for _, line := range organizationTokenCreateLines(name, organizationID, rawToken) {
		ioctx.Stdout(line)
	}
	return nil
}

// organizationTokenCreateLines renders the one-time reveal of an organization
// token: the raw token, the store-it-now warning and the CI settings that
// upload with it.
func organizationTokenCreateLines(name, organizationID, rawToken string) []string {
	return []string{
		fmt.Sprintf("Created organization ingest token %q for organization %s, scope %s.", name, organizationID, organizationIngestScope),
		"",
		"  " + rawToken,
		"",
		"Store it now — this token is shown only once and cannot be retrieved again.",
		"",
		"Paste-ready:",
		"  # GitHub Actions repository or environment secret",
		fmt.Sprintf("  gh secret set PUTNAMI_CLOUD_TOKEN --body %q", rawToken),
		"  # Any other CI: a masked secret variable named PUTNAMI_CLOUD_TOKEN",
		fmt.Sprintf("  export PUTNAMI_CLOUD_TOKEN=%q", rawToken),
	}
}

// listOrganizationTokens fetches the organization's ingest tokens, revoked rows
// included: revoke resolves an already-revoked id so a re-run converges.
func listOrganizationTokens(client *http.Client, controlURL, accessToken, organizationID string) ([]map[string]any, error) {
	resp, err := organizationTokenCall(client, http.MethodGet, organizationTokensURL(controlURL, organizationID), accessToken, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	raw, _ := resp["tokens"].([]any)
	rows := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if row, ok := item.(map[string]any); ok {
			// The list never carries a secret; drop one a server would send.
			delete(row, "raw_token")
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func organizationTokensList(params map[string]any, organizationID, workspaceRoot string, env map[string]string, ioctx IO) error {
	accessToken, controlURL, err := organizationTokenContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	rows, err := listOrganizationTokens(ioctx.Client, controlURL, accessToken, organizationID)
	if err != nil {
		return err
	}
	rows = activeMachineTokens(rows)
	if clicore.StructuredOutput(params) {
		writeResult(map[string]any{"organization_id": organizationID, "tokens": rows}, params, ioctx, "")
		return nil
	}
	if len(rows) == 0 {
		ioctx.Stdout("No ingest tokens in organization " + organizationID + ".")
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

func organizationTokensRevoke(params map[string]any, args []string, organizationID, workspaceRoot string, env map[string]string, ioctx IO) error {
	target := machineTokenTarget(args)
	if target == "" {
		return newError("cloud token revoke requires an <id> or <name>", ExitUsage)
	}
	accessToken, controlURL, err := organizationTokenContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	rows, err := listOrganizationTokens(ioctx.Client, controlURL, accessToken, organizationID)
	if err != nil {
		return err
	}
	id, err := resolveTokenID(rows, target, "organization ingest token")
	if err != nil {
		return err
	}
	route := organizationTokensURL(controlURL, organizationID) + "/" + urlPathEscape(id)
	// An already-revoked token (404) converges: the person's intent holds.
	if _, err := organizationTokenCall(ioctx.Client, http.MethodDelete, route, accessToken, nil, http.StatusOK); err != nil && !refusedWith(err, http.StatusNotFound) {
		return err
	}
	writeResult(
		map[string]any{"revoked": true, "id": id, "organization_id": organizationID},
		params,
		ioctx,
		fmt.Sprintf("Revoked organization ingest token %s.", id),
	)
	return nil
}

// orgTokenRefusal is a refused organization token call: the route, the status
// and the server's reason, never its body.
type orgTokenRefusal struct {
	status int
	err    error
}

func (r *orgTokenRefusal) Error() string { return r.err.Error() }
func (r *orgTokenRefusal) Unwrap() error { return r.err }

// organizationTokenRefusal names the route and identity-api's reason, read
// from its envelope's message, then error. A 401 exits as an auth failure; a
// 403 also names the permission the routes need.
func organizationTokenRefusal(target string, statusCode int, status string, body []byte) error {
	reason := clicore.ServerErrorMessageFromBody(body, status, "message", "error_description", "error")
	message := fmt.Sprintf("request failed for %s: %s (%s)", target, reason, status)
	code := ExitAPI
	switch statusCode {
	case http.StatusUnauthorized:
		code = ExitAuth
	case http.StatusForbidden:
		message += "; an organization token needs platform.organization.manage on that organization"
	}
	return &orgTokenRefusal{status: statusCode, err: newError(message, code)}
}

// refusedWith reports whether err is a refusal the server answered with status.
func refusedWith(err error, status int) bool {
	var refusal *orgTokenRefusal
	return errors.As(err, &refusal) && refusal.status == status
}
