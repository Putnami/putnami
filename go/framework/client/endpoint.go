package client

import (
	"context"
	"strings"

	"go.putnami.dev/errors"
)

type endpointKey struct{}
type endpointBarrierKey struct{}

type endpointSelections map[string]string

// WithEndpoint selects the endpoint of one generated call without changing its
// service binding. serviceID is the provider-published service identity, so a
// context shared by several generated clients can redirect only the named
// service. The endpoint must be trusted deployment data, not unchecked user
// input: the named binding's declared credentials will be sent to it. The call
// validates it using that binding's URL and AllowInsecure policy before sending
// credentials. An empty endpoint is invalid; omit WithEndpoint for the default.
//
// The selection belongs only to the generated call that consumes the context:
// credential providers, interceptors and other callbacks receive a child
// context that cannot redirect a nested generated call. The caller's context is
// immutable and can still be reused for another call to the named service.
func WithEndpoint(ctx context.Context, serviceID, endpoint string) context.Context {
	selections := endpointSelections(nil)
	if current, ok := ctx.Value(endpointKey{}).(endpointSelections); ok {
		selections = current
	}
	next := make(endpointSelections, len(selections)+1)
	for selectedService, selectedEndpoint := range selections {
		next[selectedService] = selectedEndpoint
	}
	next[serviceID] = endpoint
	return context.WithValue(ctx, endpointKey{}, next)
}

// clientForEndpoint creates an immutable transport view. Mutable resilience
// state is shared only by calls to the same canonical endpoint; registry-owned
// credentials, response caches and stream cleanup keep their existing lifetime.
// The returned context retains cancellation, deadlines and ordinary values but
// puts a barrier in front of endpoint selections, making the selection one call
// wide even when a credential provider or interceptor invokes another client.
func clientForEndpoint(ctx context.Context, client *Client) (*Client, context.Context, error) {
	// Every generated call passes here before it sends anything, which makes
	// this the one place a success body sink no call took is refused.
	if err := refuseUntakenSuccessBody(ctx); err != nil {
		return nil, ctx, err
	}
	if blocked, ok := ctx.Value(endpointBarrierKey{}).(bool); ok && blocked {
		return client, ctx, nil
	}
	callCtx := context.WithValue(ctx, endpointBarrierKey{}, true)
	selections, ok := ctx.Value(endpointKey{}).(endpointSelections)
	if !ok {
		selections = nil
	}
	if len(selections) == 0 {
		return client, callCtx, nil
	}
	if client == nil || client.service == nil {
		return nil, callCtx, errors.New(CodeClientConfig, "an endpoint override requires a registered service binding")
	}
	runtime := client.service
	raw, selected := selections[runtime.descriptor.Contract.Service.ID]
	if !selected {
		return client, callCtx, nil
	}
	if runtime.registry != nil && runtime.registry.isClosed() {
		return nil, callCtx, errClosedRegistry()
	}
	endpoint, err := parseBoundURL(raw, runtime.binding.AllowInsecure)
	if err != nil {
		return nil, callCtx, err
	}
	baseURL := strings.TrimRight(endpoint.String(), "/")
	if runtime.baseURL == baseURL {
		return client, callCtx, nil
	}
	owner := runtime.endpointOwner
	if owner == nil {
		owner = runtime
	}
	owner.mu.Lock()
	if owner.endpoints == nil {
		owner.endpoints = map[string]*serviceRuntime{owner.baseURL: owner}
	}
	target := owner.endpoints[baseURL]
	if target == nil {
		binding := owner.binding
		binding.URL = baseURL
		target = &serviceRuntime{
			descriptor: owner.descriptor, binding: binding, baseURL: baseURL,
			registry: owner.registry, credentials: owner.credentials,
			breakers: make(map[string]*CircuitBreaker), cachePlans: make(map[cachePlanKey]*cachePlan),
			endpointOwner: owner,
		}
		owner.endpoints[baseURL] = target
	}
	owner.mu.Unlock()
	endpointClient := *client
	endpointClient.service = target
	endpointClient.config.BaseURL = baseURL
	switch transport := client.transport.(type) {
	case *HTTPTransport:
		view := *transport
		view.baseURL = baseURL
		endpointClient.transport = &view
	case *connectRoundTripper:
		view := *transport
		inner := *transport.inner
		inner.baseURL = baseURL
		view.inner = &inner
		endpointClient.transport = &view
	default:
		return nil, callCtx, errors.New(CodeClientConfig, "an endpoint override requires the framework HTTP transport")
	}
	return &endpointClient, callCtx, nil
}
