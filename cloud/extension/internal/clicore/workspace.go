package clicore

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"go.putnami.dev/client"
	identityapi "go.putnami.dev/cloud/clients/identity-api/go"
)

// WorkspaceContext is the resolved per-invocation seam every workspace command
// shares — deploy, status, and the observability query commands
// (logs/traces/metrics). It carries the linked workspace id, the control-plane
// base URL, the minted workspace-scoped bearer, the IO surface, and a one-shot
// re-mint closure. CallWithSession and OpenStreamWithSession own the
// single-401 re-mint, giving every generated call ONE auth-fetch
// implementation instead of drifting copies.
//
// Commands that carry extra per-invocation fields (deploy's app/environment/
// workspaceRoot, the observability app/environment) embed a *WorkspaceContext
// and add their own fields; status uses it directly.
type WorkspaceContext struct {
	WorkspaceID  string
	ControlPlane string
	AuthToken    Bearer
	IO           IO
	// RefreshAuth re-mints the workspace session (WorkspaceAuth refreshes via the
	// stored refresh token). A long-running command (deploy --wait, logs --all,
	// the SSE tail) can outlive the access token minted at start; without this the
	// request dies mid-run with "Authentication required" while the control plane
	// keeps working. CallWithSession invokes it at most once per call.
	RefreshAuth func() (Bearer, error)
}

// ResolveWorkspaceContext reads the cloud link, resolves the workspace id
// (honoring the --workspace / workspaceId param with link fallback), mints the
// workspace-scoped bearer, resolves the control-plane URL, and builds the
// one-shot re-mint closure. It is the single resolver behind every workspace
// command so the --workspace override, the token mint, and the control-plane
// resolution behave identically everywhere.
//
// The --workspace override changes the token scope and workspace path; the
// control-plane base still resolves from --control-plane-url, env overrides, then
// the linked workspace. If the target workspace lives on a different control
// plane than the link, pass --control-plane-url (or PUTNAMI_CLOUD_API_URL /
// PUTNAMI_CONTROL_PLANE_URL) with --workspace.
//
// App and environment are deliberately NOT resolved here: they are
// command-specific (status has neither; deploy is lenient about an unresolved
// app; the observability commands treat an unresolved app as a hard error), so
// each command resolves them alongside this call.
func ResolveWorkspaceContext(params map[string]any, workspaceRoot string, env map[string]string, ioctx IO) (*WorkspaceContext, error) {
	link, err := ReadCloudLink(workspaceRoot)
	if err != nil {
		return nil, err
	}
	return ResolveWorkspaceContextFromLink(params, link, env, ioctx)
}

// ResolveWorkspaceContextFromLink is ResolveWorkspaceContext for callers that
// already read the Cloud link to resolve command-local defaults (for example
// environment). It avoids a second file read/parse while keeping workspace id,
// auth minting, control-plane override precedence, and refresh behavior shared.
func ResolveWorkspaceContextFromLink(params map[string]any, link map[string]any, env map[string]string, ioctx IO) (*WorkspaceContext, error) {
	workspaceID, err := ResolveWorkspaceRef(params, env, ioctx, link)
	if err != nil {
		return nil, err
	}
	auth, err := WorkspaceAuth(params, env, ioctx, workspaceID)
	if err != nil {
		return nil, err
	}
	controlPlane := resolveWorkspaceControlPlaneURL(params, env, link)
	if controlPlane == "" {
		return nil, NewError("no control-plane URL — run `putnami cloud setup --workspace <id>` or pass --control-plane-url", ExitUsage)
	}
	return &WorkspaceContext{
		WorkspaceID:  workspaceID,
		ControlPlane: controlPlane,
		AuthToken:    NewBearer(auth.AccessToken),
		IO:           ioctx,
		RefreshAuth: func() (Bearer, error) {
			fresh, err := WorkspaceAuth(params, env, ioctx, workspaceID)
			if err != nil {
				return Bearer{}, err
			}
			return NewBearer(fresh.AccessToken), nil
		},
	}, nil
}

func resolveWorkspaceControlPlaneURL(params map[string]any, env map[string]string, link map[string]any) string {
	fallback := FirstString(workspaceParamField(Param(params, "workspace", "workspaceId"), "control_plane_url"), StringValue(link["control_plane_url"]))
	if FirstString(
		StringParam(params, "control-plane-url", "controlPlaneUrl"),
		EnvGet(env, "PUTNAMI_CLOUD_API_URL"),
		EnvGet(env, "PUTNAMI_CONTROL_PLANE_URL"),
		fallback,
	) == "" {
		return ""
	}
	return ControlPlaneBaseURL(params, env, fallback)
}

// ResolveWorkspaceID resolves the workspace to target: the explicit --workspace
// flag (with its camelCase workspaceId alias) wins, falling back to the linked
// workspace. This honors the flag's advertised "Target workspace id. Defaults to
// the linked workspace" contract; without it --workspace was silently ignored
// and a command always queried — and minted auth for — the linked workspace, not
// the one the caller asked for.
func ResolveWorkspaceID(params map[string]any, link map[string]any) string {
	value := Param(params, "workspace", "workspaceId")
	return FirstString(
		workspaceParamField(value, "workspace_id"),
		strings.TrimSpace(ScalarStringValue(value)),
		StringValue(link["workspace_id"]),
	)
}

var workspaceIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// IsWorkspaceID reports whether ref is a canonical workspace id (lower-case
// UUID). Anything else passed to --workspace is a slug or a display name that
// LookupWorkspaceID must resolve before a workspace route can be built.
func IsWorkspaceID(ref string) bool {
	return workspaceIDPattern.MatchString(strings.TrimSpace(ref))
}

// ResolveWorkspaceRef is ResolveWorkspaceID for humans: --workspace accepts the
// workspace id, its slug, or its display name. An id (or an absent value) is
// returned as is without any network call; a slug or name is resolved to the
// id with the caller's session through LookupWorkspaceID. Every workspace
// route and every scope_ref check downstream keeps addressing the id, so the
// resolution happens exactly once, here.
//
// Only a scalar flag value is ever resolved: the Cloud link and the
// task-injected workspace object always carry the canonical id (setup resolves
// a slug before writing the link), so they pass through untouched.
func ResolveWorkspaceRef(params map[string]any, env map[string]string, ioctx IO, link map[string]any) (string, error) {
	value := Param(params, "workspace", "workspaceId")
	if _, isObject := value.(map[string]any); !isObject {
		if ref := strings.TrimSpace(ScalarStringValue(value)); ref != "" && !IsWorkspaceID(ref) {
			return LookupWorkspaceID(params, env, ioctx, link, ref)
		}
	}
	return ResolveWorkspaceID(params, link), nil
}

// LookupWorkspaceID resolves a slug or display name to the workspace id. It
// lists the caller's workspaces (identity-api GET /v1/workspaces) and matches
// the slug first, then the name, case-insensitively; an ambiguous name is
// refused rather than guessed. A workspace the listing does not carry (for
// example one reached only through an organization grant) is then looked up
// by name in the caller's personal organization (GET /v1/workspaces/lookup),
// the only server-side lookup that accepts something other than the id. Both
// reads go through identity-api's generated client.
func LookupWorkspaceID(params map[string]any, env map[string]string, ioctx IO, link map[string]any, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	controlPlane := resolveWorkspaceControlPlaneURL(params, env, link)
	if controlPlane == "" {
		return "", NewError("resolving --workspace "+ref+" needs a control-plane URL — run `putnami cloud setup` or pass --control-plane-url", ExitUsage)
	}
	auth, err := ActiveAuth(params, env, ioctx)
	if err != nil {
		return "", err
	}
	identity, err := NewServiceClient[identityapi.Client](identityapi.RegisterClient, ServiceBindingFor(controlPlane, ioctx.Client))
	if err != nil {
		return "", err
	}
	callCtx := client.WithForwardedUserToken(context.Background(), auth.AccessToken)
	listing, err := identity.ListV1Workspaces(callCtx, identityapi.ListV1WorkspacesInput{})
	if err != nil {
		return "", RequestError(controlPlane+"/v1/workspaces", err)
	}
	var bySlug, byName []string
	for _, row := range Deref(listing.Workspaces) {
		id := strings.TrimSpace(Deref(row.Id))
		if id == "" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(Deref(row.Slug)), ref) {
			bySlug = append(bySlug, id)
		}
		if strings.EqualFold(strings.TrimSpace(Deref(row.Name)), ref) {
			byName = append(byName, id)
		}
	}
	for _, matches := range [][]string{bySlug, byName} {
		if len(matches) == 1 {
			return matches[0], nil
		}
		if len(matches) > 1 {
			sort.Strings(matches)
			return "", NewError(fmt.Sprintf("--workspace %s matches several workspaces (%s); pass the workspace id", ref, strings.Join(matches, ", ")), ExitUsage)
		}
	}
	personal, err := identity.ListV1WorkspacesLookup(callCtx, identityapi.ListV1WorkspacesLookupInput{
		Query: identityapi.ListV1WorkspacesLookupQuery{Name: &ref},
	})
	if err != nil && ServiceStatus(err) != http.StatusNotFound {
		return "", RequestError(controlPlane+"/v1/workspaces/lookup?name="+url.QueryEscape(ref), err)
	}
	if err == nil {
		if id := strings.TrimSpace(Deref(personal.Id)); IsWorkspaceID(id) {
			return id, nil
		}
	}
	return "", NewError(fmt.Sprintf("no workspace with slug or name %q among your workspaces; pass the workspace id", ref), ExitUsage)
}

// workspaceParamField extracts one trimmed field from an object-shaped workspace
// param — the task-injected options."@putnami/cloud".workspace link object. A
// scalar or absent param is not an object, so it yields "" (ValueString handles
// the nil map), letting callers fall back to a scalar id or the on-disk link.
func workspaceParamField(value any, key string) string {
	link, _ := value.(map[string]any)
	return strings.TrimSpace(ValueString(link, key))
}

// WorkspaceURL builds a control-plane URL under this workspace:
// <control-plane>/v1/workspaces/<workspace-id><suffix>. suffix is the trailing
// route segment (with its leading slash), e.g. "/deploy", "/deployments",
// "/logs". The workspace id is path-escaped; a caller embedding a second dynamic
// segment (a release id) escapes it itself.
func (ctx *WorkspaceContext) WorkspaceURL(suffix string) string {
	return strings.TrimRight(ctx.ControlPlane, "/") + "/v1/workspaces/" + url.PathEscape(ctx.WorkspaceID) + suffix
}
