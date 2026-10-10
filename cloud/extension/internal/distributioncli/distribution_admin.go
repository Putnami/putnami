package distributioncli

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	distributionapiclient "go.putnami.dev/cloud/clients/distribution-api/go"
	putserverclient "go.putnami.dev/cloud/clients/put-server/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// Distribution backs the owner-facing binding and grant administration flow.
// The owner workspace always comes from the linked checkout; namespace,
// creator, and owner authority are never accepted by grant commands.
//
//	putnami cloud packages namespaces activate --protocol npm --namespace @owner --idempotency-key npm-owner
//	putnami cloud packages namespaces list
//	putnami cloud packages grants create --grantee-workspace-id <uuid> --channel latest --idempotency-key latest-reader
//	putnami cloud packages grants list
//	putnami cloud packages grants revoke <grant-id>
//	putnami cloud packages mirrors add --ecosystem npm --id npmjs --destination https://registry.npmjs.org < token.txt
//	putnami cloud packages mirrors add --ecosystem archive --id github-cli --destination github.com/Putnami/cli --package putnami/cli < token.txt
//	putnami cloud packages mirrors list
//	putnami cloud packages mirrors remove <id>
//	putnami cloud packages mirrors rotate-credential <id> < token.txt
func Distribution(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	pos := commandPositionals(args)
	if len(pos) == 0 || pos[0] == "help" {
		return distributionHelp(params, ioctx)
	}
	if len(pos) < 2 {
		return clicore.NewError("cloud packages requires namespaces <activate|list>, grants <create|list|revoke> or mirrors <add|list|remove|rotate-credential>", clicore.ExitUsage)
	}
	if pos[0] == "grants" || pos[0] == "mirrors" {
		// Mirror targets are owner-workspace state exactly like v2 grants: the
		// owner is the linked checkout, and the registry derives its namespace
		// and the caller's admin role from local authority.
		// --package is retired authority everywhere except mirrors add, where
		// it names the one package an archive target copies.
		var accepted []string
		if pos[0] == "mirrors" && pos[1] == "add" {
			accepted = []string{"package"}
		}
		if err := validateWorkspaceGrantFlags(params, accepted...); err != nil {
			return err
		}
	}
	switch pos[0] + " " + pos[1] {
	case "bindings activate":
		return distributionBindingActivate(params, workspaceRoot, env, ioctx)
	case "bindings list":
		return distributionBindingsList(params, workspaceRoot, env, ioctx)
	case "grants create":
		return distributionGrantCreate(params, workspaceRoot, env, ioctx)
	case "grants list":
		return distributionGrantsList(params, workspaceRoot, env, ioctx)
	case "grants revoke":
		if len(pos) < 3 {
			return clicore.NewError("cloud packages grants revoke requires <grant-id>", clicore.ExitUsage)
		}
		return distributionGrantRevoke(params, workspaceRoot, env, ioctx, pos[2])
	case "mirrors add":
		return distributionMirrorAdd(params, workspaceRoot, env, ioctx)
	case "mirrors list":
		return distributionMirrorsList(params, workspaceRoot, env, ioctx)
	case "mirrors remove":
		if len(pos) < 3 {
			return clicore.NewError("cloud packages mirrors remove requires <id>", clicore.ExitUsage)
		}
		return distributionMirrorRemove(params, workspaceRoot, env, ioctx, pos[2])
	case "mirrors rotate-credential":
		if len(pos) < 3 {
			return clicore.NewError("cloud packages mirrors rotate-credential requires <id>", clicore.ExitUsage)
		}
		return distributionMirrorRotate(params, workspaceRoot, env, ioctx, pos[2])
	default:
		return clicore.NewError("unknown cloud packages command; expected namespaces <activate|list>, grants <create|list|revoke> or mirrors <add|list|remove|rotate-credential>", clicore.ExitUsage)
	}
}

func distributionHelp(params map[string]any, ioctx clicore.IO) error {
	commands := []map[string]string{
		{"command": "cloud packages namespaces activate", "description": "activate one immutable protocol namespace for the linked owner workspace"},
		{"command": "cloud packages namespaces list", "description": "list active namespace bindings for the linked owner workspace"},
		{"command": "cloud packages grants create", "description": "grant one workspace read access to release history, optionally through one channel"},
		{"command": "cloud packages grants list", "description": "list the linked owner workspace's grants"},
		{"command": "cloud packages grants revoke", "description": "revoke one linked-workspace grant by immutable id"},
		{"command": "cloud packages mirrors add", "description": "onboard one npm, oci or archive (GitHub Release assets) public mirror target; the token or password is read from stdin"},
		{"command": "cloud packages mirrors list", "description": "list the linked owner workspace's public mirror targets"},
		{"command": "cloud packages mirrors remove", "description": "revoke one public mirror target by id; pending copies stop"},
		{"command": "cloud packages mirrors rotate-credential", "description": "replace one target's credential from stdin; accepted copies keep running"},
	}
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(map[string]any{"commands": commands}, params, ioctx, "")
		return nil
	}
	ioctx.Stdout("@putnami/cloud packages commands:")
	for _, command := range commands {
		ioctx.Stdout(fmt.Sprintf("  putnami %-42s %s", command["command"], command["description"]))
	}
	return nil
}

func distributionBindingActivate(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	for _, forbidden := range []string{"owner-workspace", "owner-workspace-id", "workspace"} {
		// The framework contributes the linked workspace as a structured task
		// option. That ambient object is not a caller override; explicit CLI flags
		// arrive as scalar values and remain forbidden.
		if clicore.ScalarStringValue(clicore.Param(params, forbidden)) != "" {
			return clicore.NewError("cloud packages namespaces activate does not accept --"+forbidden+"; the owner workspace is derived server-side", clicore.ExitUsage)
		}
	}
	protocol := canonicalDistributionProtocol(clicore.ScalarStringValue(clicore.Param(params, "protocol")))
	namespace := clicore.ScalarStringValue(clicore.Param(params, "namespace"))
	key := distributionIdempotencyKey(params)
	if protocol == "" || namespace == "" || key == "" {
		return clicore.NewError("cloud packages namespaces activate requires --protocol gomod|npm|put|oci, --namespace <exact>, and --idempotency-key <key>", clicore.ExitUsage)
	}
	workspaceID, token, controlURL, err := distributionAdminContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	target := distributionBindingsURL(controlURL, workspaceID)
	api, err := distributionAPIClient(ioctx, controlURL)
	if err != nil {
		return err
	}
	result, err := api.CreateV1WorkspacesDistributionBindings(withBearer(token), distributionapiclient.CreateV1WorkspacesDistributionBindingsInput{
		Path: distributionapiclient.CreateV1WorkspacesDistributionBindingsPath{Workspace: workspaceID},
		Body: distributionapiclient.ActivateBindingRequest{Protocol: &protocol, Namespace: &namespace, IdempotencyKey: &key},
	})
	if err != nil {
		return clicore.RequestError(target, err)
	}
	if result.Status != http.StatusCreated || result.Body == nil {
		return clicore.UnexpectedStatus(target, result.Status)
	}
	response, err := clicore.ResponseMap(result.Body)
	if err != nil {
		return err
	}
	clicore.WriteResult(response, params, ioctx, fmt.Sprintf("Activated immutable %s Distribution namespace %s for workspace %s.", protocol, namespace, workspaceID))
	return nil
}

func distributionBindingsList(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	workspaceID, token, controlURL, err := distributionAdminContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	api, err := distributionAPIClient(ioctx, controlURL)
	if err != nil {
		return err
	}
	listed, err := api.GetV1WorkspacesDistributionBindings(withBearer(token), distributionapiclient.GetV1WorkspacesDistributionBindingsInput{
		Path: distributionapiclient.GetV1WorkspacesDistributionBindingsPath{Workspace: workspaceID},
	})
	if err != nil {
		return clicore.RequestError(distributionBindingsURL(controlURL, workspaceID), err)
	}
	response, err := clicore.ResponseMap(listed)
	if err != nil {
		return err
	}
	return renderDistributionRows(params, ioctx, response, "bindings", "No active Distribution namespace bindings.", func(row map[string]any) string {
		return fmt.Sprintf("%-7s %-36s %s", clicore.ValueString(row, "protocol"), clicore.ValueString(row, "namespace"), clicore.ValueString(row, "id"))
	})
}

func distributionGrantCreate(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	channel := clicore.ScalarStringValue(clicore.Param(params, "channel"))
	granteeID := clicore.ScalarStringValue(clicore.Param(params, "grantee-workspace-id", "granteeWorkspaceId"))
	key := distributionIdempotencyKey(params)
	if granteeID == "" || granteeID != strings.TrimSpace(granteeID) || key == "" {
		return clicore.NewError("cloud packages grants create requires --grantee-workspace-id <workspace UUID> and --idempotency-key <key>", clicore.ExitUsage)
	}
	workspaceID, token, registryURL, err := distributionGrantContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	body := putserverclient.AccessGrantCreateRequest{GranteeWorkspaceId: &granteeID, IdempotencyKey: &key}
	if channel != "" {
		body.Channel = &channel
	}
	put, err := putOwnerClient(ioctx, registryURL)
	if err != nil {
		return err
	}
	created, err := put.CreatePutReleaseSetsWorkspacesGrants(withBearer(token), putserverclient.CreatePutReleaseSetsWorkspacesGrantsInput{
		Path: putserverclient.CreatePutReleaseSetsWorkspacesGrantsPath{Workspace: workspaceID},
		Body: body,
	})
	if err != nil {
		return clicore.RequestError(distributionGrantsURL(registryURL, workspaceID), err)
	}
	response, err := clicore.ResponseMap(created)
	if err != nil {
		return err
	}
	clicore.WriteResult(response, params, ioctx, "Granted workspace "+granteeID+" read access to release history.")
	return nil
}

func distributionGrantsList(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	workspaceID, token, registryURL, err := distributionGrantContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	endpoint := distributionGrantsURL(registryURL, workspaceID)
	query := putserverclient.GetPutReleaseSetsWorkspacesGrantsQuery{}
	if clicore.Truthy(clicore.Param(params, "include-revoked")) {
		includeRevoked := true
		query.IncludeRevoked = &includeRevoked
		endpoint += "?include_revoked=true"
	}
	put, err := putOwnerClient(ioctx, registryURL)
	if err != nil {
		return err
	}
	listed, err := put.GetPutReleaseSetsWorkspacesGrants(withBearer(token), putserverclient.GetPutReleaseSetsWorkspacesGrantsInput{
		Path:  putserverclient.GetPutReleaseSetsWorkspacesGrantsPath{Workspace: workspaceID},
		Query: query,
	})
	if err != nil {
		return clicore.RequestError(endpoint, err)
	}
	response, err := clicore.ResponseMap(listed)
	if err != nil {
		return err
	}
	return renderDistributionRows(params, ioctx, response, "grants", "No Distribution workspace grants.", func(row map[string]any) string {
		channel := clicore.ValueString(row, "channel")
		if channel == "" {
			channel = "all channels"
		}
		return fmt.Sprintf("%-36s %-36s %s", clicore.ValueString(row, "id"), clicore.ValueString(row, "grantee_workspace_id"), channel)
	})
}

func distributionGrantRevoke(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO, grantID string) error {
	grantID = strings.TrimSpace(grantID)
	if grantID == "" {
		return clicore.NewError("cloud packages grants revoke requires a canonical grant id", clicore.ExitUsage)
	}
	workspaceID, token, registryURL, err := distributionGrantContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	put, err := putOwnerClient(ioctx, registryURL)
	if err != nil {
		return err
	}
	revoked, err := put.DeletePutReleaseSetsWorkspacesGrants(withBearer(token), putserverclient.DeletePutReleaseSetsWorkspacesGrantsInput{
		Path: putserverclient.DeletePutReleaseSetsWorkspacesGrantsPath{Workspace: workspaceID, Id: grantID},
	})
	if err != nil {
		return clicore.RequestError(distributionGrantsURL(registryURL, workspaceID)+"/"+clicore.URLPathEscape(grantID), err)
	}
	response, err := clicore.ResponseMap(revoked)
	if err != nil {
		return err
	}
	clicore.WriteResult(response, params, ioctx, "Revoked Distribution grant "+grantID+".")
	return nil
}

func distributionAdminContext(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) (workspaceID, token, controlURL string, err error) {
	link, err := clicore.ReadCloudLink(workspaceRoot)
	if err != nil {
		return "", "", "", err
	}
	workspaceID = strings.TrimSpace(clicore.StringValue(link["workspace_id"]))
	if workspaceID == "" {
		return "", "", "", clicore.NewError("linked workspace is missing workspace_id; run `putnami cloud setup --workspace <id>`", clicore.ExitUsage)
	}
	auth, err := clicore.ActiveAuth(params, env, ioctx)
	if err != nil {
		return "", "", "", err
	}
	return workspaceID, auth.AccessToken, clicore.ControlPlaneBaseURL(params, env, clicore.StringValue(link["control_plane_url"])), nil
}

func distributionBindingsURL(controlURL, workspaceID string) string {
	return controlURL + "/v1/workspaces/" + clicore.URLPathEscape(workspaceID) + "/distribution/bindings"
}

// distributionGrantContext uses the native Put user credential. The owner
// workspace remains the linked checkout and the registry resolves its current
// namespace and the caller's grant-admin role from local authority.
func distributionGrantContext(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) (workspaceID, token, registryURL string, err error) {
	link, err := clicore.ReadCloudLink(workspaceRoot)
	if err != nil {
		return "", "", "", err
	}
	workspaceID = strings.TrimSpace(clicore.StringValue(link["workspace_id"]))
	if workspaceID == "" {
		return "", "", "", clicore.NewError("linked workspace is missing workspace_id; run `putnami cloud setup --workspace <id>`", clicore.ExitUsage)
	}
	state, err := readRegistriesState(env)
	if err != nil {
		return "", "", "", err
	}
	endpoint, ok := registryEndpointForRegistry(params, env, state, RegistryPut)
	if !ok {
		return "", "", "", clicore.NewError("Put registry endpoint is unavailable; run `putnami cloud login`", clicore.ExitAuth)
	}
	token, err = mintUserRegistryToken(params, workspaceRoot, env, ioctx, endpoint)
	return workspaceID, token, strings.TrimRight(endpoint.URL, "/"), err
}

func distributionGrantsURL(registryURL, workspaceID string) string {
	return putOwnerURL(registryURL, "/_/release-sets/workspaces/"+clicore.URLPathEscape(workspaceID)+"/grants")
}

func validateWorkspaceGrantFlags(params map[string]any, accepted ...string) error {
	for _, flag := range []string{
		"owner-workspace", "owner-workspace-id", "namespace", "protocol", "package", "resource-kind",
		"purpose", "action", "grantee-kind", "grantee-id", "creator", "creator-id",
		"principal", "principal-kind", "principal-id", "scope", "scopes", "audience", "ttl", "expires-in",
	} {
		if slices.Contains(accepted, flag) {
			continue
		}
		if clicore.Param(params, flag, registryTokenFlagCamel(flag)) != nil {
			return clicore.NewError("cloud packages grants does not accept --"+flag+"; v2 grants name one workspace and optional channel", clicore.ExitUsage)
		}
	}
	if clicore.ScalarStringValue(clicore.Param(params, "workspace")) != "" {
		return clicore.NewError("cloud packages grants does not accept --workspace; the owner is the linked workspace", clicore.ExitUsage)
	}
	return nil
}

func canonicalDistributionProtocol(protocol string) string {
	if validDistributionProtocol(protocol) {
		return protocol
	}
	return ""
}

func distributionIdempotencyKey(params map[string]any) string {
	key := clicore.ScalarStringValue(clicore.Param(params, "idempotency-key"))
	if key == "" || key != strings.TrimSpace(key) {
		return ""
	}
	return key
}

func renderDistributionRows(params map[string]any, ioctx clicore.IO, response map[string]any, field, empty string, line func(map[string]any) string) error {
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(response, params, ioctx, "")
		return nil
	}
	raw, _ := response[field].([]any)
	if len(raw) == 0 {
		ioctx.Stdout(empty)
		return nil
	}
	for _, item := range raw {
		if row, ok := item.(map[string]any); ok {
			ioctx.Stdout(line(row))
		}
	}
	return nil
}
