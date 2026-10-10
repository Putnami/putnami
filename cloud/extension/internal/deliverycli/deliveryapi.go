package deliverycli

import (
	deliveryapiclient "go.putnami.dev/cloud/clients/delivery-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// newDeliveryAPIClient resolves the generated delivery-api client through the
// shared Cloud service-client wiring (clicore.NewServiceClient): the
// control-plane binding, the CLI's shared HTTP client (it stamps the CLI
// User-Agent) and the forwarded user bearer are exactly what every other
// generated Cloud client uses.
func newDeliveryAPIClient(ctx *clicore.WorkspaceContext) (*deliveryapiclient.DeliveryClient, error) {
	return clicore.NewServiceClient[deliveryapiclient.DeliveryClient](deliveryapiclient.RegisterDeliveryClient, ctx.ServiceBinding())
}
