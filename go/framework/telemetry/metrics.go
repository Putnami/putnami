package telemetry

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// Meter returns a named meter from the global MeterProvider.
// Use this to create counters, histograms, and gauges.
//
//	meter := telemetry.Meter("my-service")
//	counter, _ := meter.Int64Counter("requests_total")
//	counter.Add(ctx, 1)
func Meter(name string, opts ...metric.MeterOption) metric.Meter {
	return otel.Meter(name, opts...)
}

// Counter creates a named Int64Counter from the global MeterProvider.
func Counter(meter metric.Meter, name string, opts ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	return meter.Int64Counter(name, opts...)
}

// Histogram creates a named Float64Histogram from the global MeterProvider.
func Histogram(meter metric.Meter, name string, opts ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	return meter.Float64Histogram(name, opts...)
}

// Gauge creates a named Float64Gauge from the global MeterProvider.
func Gauge(meter metric.Meter, name string, opts ...metric.Float64GaugeOption) (metric.Float64Gauge, error) {
	return meter.Float64Gauge(name, opts...)
}

// UpDownCounter creates a named Int64UpDownCounter from the global MeterProvider.
func UpDownCounter(meter metric.Meter, name string, opts ...metric.Int64UpDownCounterOption) (metric.Int64UpDownCounter, error) {
	return meter.Int64UpDownCounter(name, opts...)
}
