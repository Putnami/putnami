package events

import (
	"context"
	stderrors "errors"
)

// Durable publish routes and outcomes form the provider-neutral contract that a
// transactional outbox relay uses. A relay persists the route with each record
// and the outcome of each attempt, so these string values must never change.
const (
	// PublishRouteDirectPubSub names a publish sent straight to Google Cloud
	// Pub/Sub by DirectPubSubTransport.
	PublishRouteDirectPubSub = "direct-pubsub"
	// PublishRouteEventServer names a publish sent to a managed Event Server.
	PublishRouteEventServer = TransportKindEventServer

	// PublishOutcomePermanent means the provider rejected the publish and a
	// retry of the same bytes fails the same way.
	PublishOutcomePermanent = "permanent"
	// PublishOutcomeRetryable means the provider did not accept the publish and
	// a later retry may succeed.
	PublishOutcomeRetryable = "retryable"
	// PublishOutcomeAmbiguous means the provider may have accepted the publish.
	// A relay must not publish it again automatically or through another route.
	PublishOutcomeAmbiguous = "ambiguous"
)

// DurablePublishTransport is the transport contract a durable relay consumes. It
// uses only framework and primitive types, so a relay can declare a matching
// interface of its own without importing a provider package.
//
// ActivePublishRoute returns the route this transport publishes on and, when
// the route has one, its topology generation. ContextForPublishRoute checks a
// persisted route before any credential or network work, and fails with
// ErrStalePublishRoute when it does not match. ClassifyPublishError maps a
// publish error to PublishOutcomePermanent, PublishOutcomeRetryable or
// PublishOutcomeAmbiguous, and returns "" for a nil error. An unknown delivery
// state is always ambiguous.
type DurablePublishTransport interface {
	Transport
	ActivePublishRoute() (transport string, topologyGenerationID string)
	ContextForPublishRoute(ctx context.Context, transport, topologyGenerationID string) (context.Context, error)
	ClassifyPublishError(err error) string
}

// ErrStalePublishRoute reports a persisted publish route that does not match the
// transport's active route. ClassifyPublishError reports it as ambiguous.
var ErrStalePublishRoute = stderrors.New("events: persisted publish route is stale or invalid")

// CheckPublishRoute returns ctx when the persisted route (gotTransport,
// gotGeneration) equals the active route (wantTransport, wantGeneration), and
// ErrStalePublishRoute otherwise. A DurablePublishTransport uses it to implement
// ContextForPublishRoute.
func CheckPublishRoute(ctx context.Context, wantTransport, wantGeneration, gotTransport, gotGeneration string) (context.Context, error) {
	if gotTransport != wantTransport || gotGeneration != wantGeneration {
		return nil, ErrStalePublishRoute
	}
	return ctx, nil
}
