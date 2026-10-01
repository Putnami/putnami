package events

import "context"

// invokeTransportHandler runs one delivery for a transport that has no broker of
// its own, through the shared per-delivery boundary in dispatchDelivery — so the
// handler timeout, the trace seeding, and the single terminal record are
// identical on every transport. It returns the boundary context so the caller's
// retry / dead-letter / drop records correlate with that terminal record.
func invokeTransportHandler(ctx context.Context, logs eventLoggers, def *HandlerDefinition, env Envelope) (context.Context, error) {
	deliveryCtx, _, err := dispatchDelivery(ctx, logs, &env, func(deliveryCtx context.Context, msg *Envelope) error {
		handlerCtx, cancel := context.WithTimeout(deliveryCtx, handlerTimeout(def.Options))
		defer cancel()
		return def.Handler(handlerCtx, msg)
	})
	return deliveryCtx, err
}
