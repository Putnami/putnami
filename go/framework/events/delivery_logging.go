package events

import (
	"cmp"
	"context"
	"log/slog"
	"time"

	"go.putnami.dev/logger"
)

// Outcome vocabulary of the logging contract (protocols/logging/conformance).
// An event delivery reports success, failure, or skipped when its handler asks
// for a redelivery (see Redeliver).
const (
	outcomeSuccess = "success"
	outcomeFailure = "failure"
	outcomeSkipped = "skipped"
)

// Drop reasons reported as event.reason on a "message dropped" record. They are
// the same strings the Observer.Dropped bridge already used, defined once here so
// the log records and the observer bridge can never drift apart.
const (
	dropQueueFull          = "queue_full"
	dropRetryEnqueueFailed = "retry_enqueue_failed"
	dropDLQEnqueueFailed   = "dlq_enqueue_failed"
	dropDLQNoSubscriber    = "dlq_no_subscriber"
	// dropBrokerStopped reports a retry that Stop cancels and a queued delivery
	// that Stop discards at the drain deadline.
	dropBrokerStopped = "broker_stopped"
	// dropRetriesExhausted is log-only: the retries-exhausted branch reports the
	// drop through the record, it has no Observer.Dropped callback (it never had).
	dropRetriesExhausted = "retries_exhausted"
)

// eventLoggers holds the pinned logger names of the event boundary contract
// (protocols/logging/conformance, "Pinned logger names"): events.handler owns the
// single terminal record per delivery, events.broker owns the retry /
// dead-letter / drop records. Every dispatching transport holds one so the names
// are identical no matter which transport delivered the message.
type eventLoggers struct {
	handler *logger.Logger
	broker  *logger.Logger
}

// newEventLoggers derives the pinned event logger names from the process default
// logger. Transports resolve it once at construction, never per delivery.
func newEventLoggers() eventLoggers {
	return eventLoggersFrom(logger.Default())
}

// eventLoggersFrom derives the pinned names from root. Tests use it with a
// memory-sink root logger to capture the records one delivery emits.
func eventLoggersFrom(root *logger.Logger) eventLoggers {
	return eventLoggers{
		handler: root.Named("events.handler"),
		broker:  root.Named("events.broker"),
	}
}

// eventGroup returns the base "event" group shared by every record of one
// delivery: the topic, the message id, and the 1-based attempt of THIS delivery.
func eventGroup(env Envelope) map[string]any {
	return map[string]any{
		"topic":     env.Topic,
		"messageId": env.ID,
		"attempt":   env.Attempt,
	}
}

// eventAttr renders a delivery's event group as one closed attr. An attr replaces
// a same-named context key at the sink, so a broker record carries exactly the
// fields it declares — never leftovers of the terminal record's group (such as
// durationMs) that the delivery's field bag still holds.
func eventAttr(env Envelope, extra map[string]any) slog.Attr {
	group := eventGroup(env)
	for k, v := range extra {
		group[k] = v
	}
	return slog.Any("event", group)
}

// retryEventAttr is the event group of a "message retry scheduled" record: the
// failed delivery's attempt plus the scheduled nextAttempt (attempt + 1), the
// backoff delay, and maxRetries in the same unit as attempt (the maximum total
// number of delivery attempts).
func retryEventAttr(env Envelope, delay time.Duration, opts HandlerOptions) slog.Attr {
	return eventAttr(env, map[string]any{
		"nextAttempt": env.Attempt + 1,
		"delayMs":     delay.Milliseconds(),
		"maxRetries":  opts.MaxRetries,
		"outcome":     outcomeFailure,
	})
}

// newDeliveryContext installs the per-delivery logging boundary on ctx: a fresh
// field bag (shadowing any caller bag — each delivery owns its own terminal
// record), a seeded trace id, and the base event group every record of the
// delivery shares. The trace id is the envelope's trace id falling back to the
// message id, matching the TypeScript dispatch pipeline so a message stays
// correlatable across runtimes even with no active span. It returns the bag too,
// so the delivery can hand its fields to a failureReporter.
func newDeliveryContext(ctx context.Context, env Envelope) (context.Context, *logger.FieldBag) {
	if ctx == nil {
		ctx = context.Background()
	}
	if traceID := cmp.Or(env.TraceID, env.ID); traceID != "" {
		ctx = logger.ContextWithTraceID(ctx, traceID)
	}
	bag := logger.NewFieldBag()
	ctx = logger.ContextWithFieldBag(ctx, bag)
	logger.Set(ctx, "event", eventGroup(env))
	return ctx, bag
}

// failureReporter offers the terminal record of a failed delivery to an
// enclosing boundary: it receives the fields the delivery accumulated, its event
// group included, and the handler error. It returns true when a record of that
// boundary will carry them; the delivery then logs no record of its own. The
// push receiver uses it to report the failure on the request that carried the
// message.
type failureReporter func(fields map[string]any, err error) bool

// dispatchDelivery runs one delivery of env through invoke inside the canonical
// per-delivery boundary and emits exactly ONE terminal record for it: INFO
// "message handled" on success, WARN "message redelivery requested" when the
// handler returns a Redeliver error, and ERROR "message handling failed" with
// the structured error on failure. A non-nil reportFailure that accepts the
// failure replaces the ERROR record. Every transport dispatches through it (directly
// or through invokeTransportHandler) so the terminal record is identical
// everywhere.
//
// It returns the boundary context — so a transport's retry / dead-letter / drop
// records correlate with the terminal record through the same trace id and event
// group — plus the delivery duration for the Observer bridge and the handler
// error. env is a pointer so a handler that mutates the envelope still mutates
// the caller's copy, as it did before the boundary existed.
func dispatchDelivery(
	ctx context.Context,
	logs eventLoggers,
	env *Envelope,
	invoke func(ctx context.Context, env *Envelope) error,
	reportFailure failureReporter,
) (context.Context, time.Duration, error) {
	deliveryCtx, bag := newDeliveryContext(ctx, *env)

	start := time.Now()
	err := invoke(deliveryCtx, env)
	duration := time.Since(start)

	redelivery := IsRedeliver(err)
	outcome := outcomeSuccess
	switch {
	case redelivery:
		outcome = outcomeSkipped
	case err != nil:
		outcome = outcomeFailure
	}
	terminal := map[string]any{
		"outcome":    outcome,
		"durationMs": duration.Milliseconds(),
	}
	// Merge the terminal fields into the bag's event group instead of emitting a
	// closed attr: the terminal record must also carry what the delivery
	// accumulated (e.g. events the handler published), exactly like the
	// TypeScript dispatch pipeline's logger.with('event', …).
	logger.Set(deliveryCtx, "event", terminal)
	switch {
	case err == nil:
		logs.handler.InfoCtx(deliveryCtx, "message handled")
	case redelivery:
		logs.handler.WarnCtx(deliveryCtx, "message redelivery requested", logger.ErrorAttr(err))
	case reportFailure != nil && reportFailure(bag.Snapshot(), err):
		// A record of the enclosing boundary carries the failure.
	default:
		logs.handler.ErrorCtx(deliveryCtx, "message handling failed", err)
	}
	return deliveryCtx, duration, err
}
