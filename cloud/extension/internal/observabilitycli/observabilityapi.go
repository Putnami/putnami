package observabilitycli

import (
	observabilityapiclient "go.putnami.dev/cloud/clients/observability-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// newObservabilityAPIClient resolves the generated observability-api client
// through the shared Cloud service-client wiring (clicore.NewServiceClient):
// the control-plane binding, the CLI's shared HTTP client (it
// stamps the CLI User-Agent) and the forwarded user bearer are exactly what
// every other generated Cloud client uses.
func newObservabilityAPIClient(ctx *clicore.WorkspaceContext) (*observabilityapiclient.ObservabilityClient, error) {
	return clicore.NewServiceClient[observabilityapiclient.ObservabilityClient](observabilityapiclient.RegisterObservabilityClient, ctx.ServiceBinding())
}
