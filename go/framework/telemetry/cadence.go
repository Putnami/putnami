package telemetry

import (
	"os"
	"strconv"
	"strings"
	"time"

	otlp "go.putnami.dev/protocol/telemetry"
)

// The standard OpenTelemetry environment variables that tune the export
// cadence. The TypeScript runtime reads the same three.
const (
	// EnvMetricExportInterval overrides the metric export interval, in
	// milliseconds, when OTLPConfig.MetricInterval is zero.
	EnvMetricExportInterval = "OTEL_METRIC_EXPORT_INTERVAL"
	// EnvSpanBatchDelay overrides the span batch delay, in milliseconds, when
	// Config.SpanBatchDelay is zero.
	EnvSpanBatchDelay = "OTEL_BSP_SCHEDULE_DELAY"
	// EnvSpanBatchSize overrides the number of spans that make a full batch.
	EnvSpanBatchSize = "OTEL_BSP_MAX_EXPORT_BATCH_SIZE"
)

// envPositiveInt reads the environment variable name as a positive integer
// written in digits only, so both runtimes accept and refuse the same values.
func envPositiveInt(name string) (int, bool) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" || strings.Trim(raw, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// cadence resolves an export period: the configured value when positive, then
// the environment variable env in milliseconds, then the protocol default.
func cadence(configured time.Duration, env string, defaultMS int) time.Duration {
	if configured > 0 {
		return configured
	}
	if ms, ok := envPositiveInt(env); ok {
		return time.Duration(ms) * time.Millisecond
	}
	return time.Duration(defaultMS) * time.Millisecond
}

// spanBatchDelay is the longest a completed span waits before its batch is
// exported.
func (c Config) spanBatchDelay() time.Duration {
	return cadence(c.SpanBatchDelay, EnvSpanBatchDelay, otlp.DefaultContract().Export.DefaultSpanBatchDelayMS)
}

// spanBatchSize is the number of spans that make a full batch, exported
// without waiting for the delay.
func spanBatchSize() int {
	if n, ok := envPositiveInt(EnvSpanBatchSize); ok {
		return n
	}
	return otlp.DefaultContract().Export.MaxSpanBatchSize
}

// metricInterval is the periodic metric export cadence.
func (c OTLPConfig) metricInterval() time.Duration {
	return cadence(c.MetricInterval, EnvMetricExportInterval, otlp.DefaultContract().Export.DefaultMetricIntervalMS)
}
