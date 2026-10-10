package cloudcli

import (
	stdcontext "context"
	"net/http"

	"go.putnami.dev/client"
	identityapi "go.putnami.dev/cloud/clients/identity-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// identityClient resolves identity-api's generated client bound to the
// control-plane origin (the load balancer routes /v1/workspaces and the
// workspace machine tokens to it). The caller's bearer travels per call.
func identityClient(httpClient *http.Client, controlURL string) (*identityapi.Client, error) {
	return clicore.NewServiceClient[identityapi.Client](identityapi.RegisterClient, clicore.ServiceBindingFor(controlURL, httpClient))
}

// bearerContext carries token as the forwarded credential of one generated call.
func bearerContext(token string) stdcontext.Context {
	return client.WithForwardedUserToken(stdcontext.Background(), token)
}

// asWorkspace checks that identity-api answered a workspace body. Setup reads
// the generated workspace directly: it keeps only the id, name and repository
// for the link.
func asWorkspace(answered *identityapi.Workspace2, target string) (*identityapi.Workspace2, error) {
	if answered == nil {
		return nil, newError("invalid JSON response from "+target, ExitAPI)
	}
	return answered, nil
}

// readWorkspace reads one workspace the caller may link (GET
// /v1/workspaces/{workspace}).
func readWorkspace(httpClient *http.Client, controlURL, accessToken, workspaceID string) (*identityapi.Workspace2, error) {
	target := controlURL + "/v1/workspaces/" + urlPathEscape(workspaceID)
	identity, err := identityClient(httpClient, controlURL)
	if err != nil {
		return nil, err
	}
	answered, err := identity.GetV1Workspaces(bearerContext(accessToken), identityapi.GetV1WorkspacesInput{
		Path: identityapi.GetV1WorkspacesPath{Workspace: workspaceID},
	})
	if err != nil {
		return nil, clicore.RequestError(target, err)
	}
	return asWorkspace(answered, target)
}

// createWorkspace creates a workspace in the caller's personal scope, or in
// organization when set (POST /v1/workspaces answers 201).
func createWorkspace(httpClient *http.Client, controlURL, accessToken string, request identityapi.CreateWorkspaceRequest) (*identityapi.Workspace2, error) {
	target := controlURL + "/v1/workspaces"
	identity, err := identityClient(httpClient, controlURL)
	if err != nil {
		return nil, err
	}
	created, err := identity.CreateV1Workspaces(bearerContext(accessToken), identityapi.CreateV1WorkspacesInput{Body: request})
	if err != nil {
		return nil, clicore.RequestError(target, err)
	}
	if created.Status != http.StatusCreated {
		return nil, clicore.UnexpectedStatus(target, created.Status)
	}
	return asWorkspace(created.Body, target)
}
