package distributioncli

import (
	"context"
	"net/http"
	"strings"

	"go.putnami.dev/client"
	distributionapiclient "go.putnami.dev/cloud/clients/distribution-api/go"
	ociserverclient "go.putnami.dev/cloud/clients/oci-server/go"
	putserverclient "go.putnami.dev/cloud/clients/put-server/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// putServerReaderProfile is put-server's forwarded registry-bearer credential
// profile: the owner's own Put user token the CLI mints and forwards per call.
const putServerReaderProfile = "reader"

// putOwnerClient resolves put-server's generated client bound to the Put
// registry the CLI resolved, forwarding the owner's Put bearer per call. The
// owner-workspace grant and mirror commands call it.
func putOwnerClient(ioctx clicore.IO, registryURL string) (*putserverclient.PutClient, error) {
	return clicore.NewServiceClient[putserverclient.PutClient](putserverclient.RegisterPutClient,
		clicore.ForwardedServiceBinding(putMountBase(registryURL), ioctx.Client, putServerReaderProfile))
}

// putMountBase drops a trailing `/put` from a Put endpoint. put-server's
// generated routes already carry the `/put` mount, and an endpoint can name
// that mount itself: the CI publication broker hands the CLI `<broker>/put`.
// Joining both sent `/put/put/_/release-sets/resolve`, which the broker
// refused, and every publishing run failed at release-set resolve.
func putMountBase(registryURL string) string {
	base := strings.TrimRight(strings.TrimSpace(registryURL), "/")
	return strings.TrimSuffix(base, "/put")
}

// distributionAPIClient resolves distribution-api's generated client bound to
// the control-plane origin (the load balancer routes /v1/workspaces/*/
// distribution to it), forwarding the signed-in user's bearer per call.
func distributionAPIClient(ioctx clicore.IO, controlURL string) (*distributionapiclient.DistributionClient, error) {
	return clicore.NewServiceClient[distributionapiclient.DistributionClient](distributionapiclient.RegisterDistributionClient,
		clicore.ServiceBindingFor(controlURL, ioctx.Client))
}

// ociCapabilitiesClient resolves oci-server's generated client bound to the
// registry the caller resolved, carrying NO credential: the capability probe is
// unauthenticated by contract (the operation declares an anonymous
// alternative), and a binding that holds no credential cannot hand the registry
// bearer to a probe the fast-path authorization decision has not been made for
// yet. A plain-http base stays accepted, as it was for the hand-written probe:
// the operator chooses the registry URL.
func ociCapabilitiesClient(httpClient *http.Client, baseURL string) (*ociserverclient.OCIClient, error) {
	return clicore.NewServiceClient[ociserverclient.OCIClient](ociserverclient.RegisterOCIClient,
		client.ServiceBinding{
			URL:           strings.TrimRight(strings.TrimSpace(baseURL), "/"),
			ClientID:      clicore.CLIClientID,
			AllowInsecure: true,
			HTTPClient:    clicore.WithUserAgent(httpClient),
		})
}

// withBearer carries token as the forwarded credential of one generated call.
func withBearer(token string) context.Context {
	return client.WithForwardedUserToken(context.Background(), token)
}

// commandContext is the context of the running command, so an interrupt
// cancels its calls.
func commandContext(ioctx clicore.IO) context.Context {
	if ioctx.Context != nil {
		return ioctx.Context
	}
	return context.Background()
}

// putOwnerURL names a put-server owner route the way the generated client
// reaches it, for the error a refusal renders.
func putOwnerURL(registryURL, suffix string) string {
	return putMountBase(registryURL) + "/put" + suffix
}
