package telemetry

import (
	"context"
	"math"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"go.putnami.dev/logger"
	otlp "go.putnami.dev/protocol/telemetry"
)

// initLogRecordsBridge registers log.records, an observable counter that
// reports logger.RecordCounts: every record the process's loggers received,
// by level and outcome. It returns the function that unregisters the
// callback; it is always non-nil.
func initLogRecordsBridge(meter metric.Meter) func() error {
	convention := otlp.LogRecordMetrics()[0]
	counter, err := meter.Int64ObservableCounter(convention.Name,
		metric.WithDescription(convention.Description),
	)
	if err != nil {
		return func() error { return nil }
	}
	registration, err := meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		for _, count := range logger.RecordCounts() {
			observer.ObserveInt64(counter, clampInt64(count.Count), metric.WithAttributes(
				attribute.String(otlp.AttrLogLevel, count.Level),
				attribute.String(otlp.AttrLogOutcome, count.Outcome),
			))
		}
		return nil
	}, counter)
	if err != nil {
		return func() error { return nil }
	}
	return registration.Unregister
}

// clampInt64 converts a cumulative count to the int64 OTel observes, holding
// it at math.MaxInt64 instead of wrapping negative.
func clampInt64(count uint64) int64 {
	if count > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(count)
}
