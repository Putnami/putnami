package events

import "context"

// Transport is the pluggable messaging backend for event delivery.
type Transport interface {
	// Publish sends an envelope to the transport.
	Publish(ctx context.Context, env Envelope) error

	// Subscribe registers a handler definition with the transport.
	Subscribe(def *HandlerDefinition) error

	// Start initializes the transport and begins delivering messages.
	Start(ctx context.Context) error

	// Stop gracefully shuts down the transport, draining in-flight messages.
	Stop(ctx context.Context) error
}
