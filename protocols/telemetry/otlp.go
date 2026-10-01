package telemetry

// This file defines the OTLP/JSON envelope types Putnami runtimes emit — the
// subset of the OpenTelemetry protocol we actually produce, not the full proto.
//
// Encoding rules (proto3 JSON mapping, as the OTLP/HTTP spec fixes them):
//
//   - 64-bit integers (timestamps, counts, int data-point values) are encoded
//     as decimal STRINGS, because JSON numbers cannot safely carry uint64.
//   - trace_id and span_id are encoded as lowercase HEX strings (OTLP/JSON's
//     documented deviation from proto3's default base64 for bytes).
//   - enums (aggregation temporality, span kind, status code, severity number)
//     are encoded as INTEGERS — unambiguous and trivially identical across
//     languages, and accepted by every OTLP collector.
//   - field names are lowerCamelCase.
//
// Field DECLARATION ORDER is canonical: the TypeScript renderer builds its
// object literals in the same order, so encoding/json (Go) and JSON.stringify
// (TS) emit byte-identical bytes for equivalent input. Do not reorder fields
// without updating the TS renderer and the equivalence golden fixtures in
// lockstep.

// ---------------------------------------------------------------------------
// Shared types (common.v1)
// ---------------------------------------------------------------------------

// AnyValue is the OTLP common.v1.AnyValue, restricted to the scalar kinds
// Putnami emits. Exactly one field is set; the rest are nil and omitted.
type AnyValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"` // int64 as decimal string
	DoubleValue *float64 `json:"doubleValue,omitempty"`
}

// KeyValue is one attribute: a key and its AnyValue.
type KeyValue struct {
	Key   string   `json:"key"`
	Value AnyValue `json:"value"`
}

// Resource carries the attributes describing the emitting service.
type Resource struct {
	Attributes []KeyValue `json:"attributes,omitempty"`
}

// Scope is the OTLP common.v1.InstrumentationScope.
type Scope struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

// ---------------------------------------------------------------------------
// Metrics (metrics.v1)
// ---------------------------------------------------------------------------

// AggregationTemporality enumerates how a sum/histogram aggregates over time.
type AggregationTemporality = int

// Aggregation temporality values (OTLP metrics.v1.AggregationTemporality).
const (
	TemporalityUnspecified AggregationTemporality = 0
	TemporalityDelta       AggregationTemporality = 1
	TemporalityCumulative  AggregationTemporality = 2
)

// MetricsRequest is the OTLP ExportMetricsServiceRequest POSTed to /v1/metrics.
type MetricsRequest struct {
	ResourceMetrics []ResourceMetrics `json:"resourceMetrics,omitempty"`
}

// ResourceMetrics groups metrics for one resource.
type ResourceMetrics struct {
	Resource     Resource       `json:"resource"`
	ScopeMetrics []ScopeMetrics `json:"scopeMetrics,omitempty"`
}

// ScopeMetrics groups metrics for one instrumentation scope.
type ScopeMetrics struct {
	Scope   Scope    `json:"scope"`
	Metrics []Metric `json:"metrics,omitempty"`
}

// Metric is one metric stream. Exactly one of Sum/Gauge/Histogram is set.
type Metric struct {
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Unit        string     `json:"unit,omitempty"`
	Sum         *Sum       `json:"sum,omitempty"`
	Gauge       *Gauge     `json:"gauge,omitempty"`
	Histogram   *Histogram `json:"histogram,omitempty"`
}

// Sum is a monotonic or non-monotonic running total.
type Sum struct {
	DataPoints             []NumberDataPoint      `json:"dataPoints,omitempty"`
	AggregationTemporality AggregationTemporality `json:"aggregationTemporality,omitempty"`
	IsMonotonic            bool                   `json:"isMonotonic,omitempty"`
}

// Gauge is a point-in-time value.
type Gauge struct {
	DataPoints []NumberDataPoint `json:"dataPoints,omitempty"`
}

// Histogram is a distribution of observations.
type Histogram struct {
	DataPoints             []HistogramDataPoint   `json:"dataPoints,omitempty"`
	AggregationTemporality AggregationTemporality `json:"aggregationTemporality,omitempty"`
}

// NumberDataPoint is one observation of a Sum or Gauge. Exactly one of
// AsInt/AsDouble is set.
type NumberDataPoint struct {
	Attributes        []KeyValue `json:"attributes,omitempty"`
	StartTimeUnixNano string     `json:"startTimeUnixNano,omitempty"` // uint64 as string
	TimeUnixNano      string     `json:"timeUnixNano,omitempty"`      // uint64 as string
	AsInt             *string    `json:"asInt,omitempty"`             // int64 as string
	AsDouble          *float64   `json:"asDouble,omitempty"`
}

// HistogramDataPoint is one bucketed distribution. Putnami's collectors track
// count/sum/min/max with no explicit bounds, so BucketCounts carries a single
// implicit (-inf,+inf) bucket and ExplicitBounds is empty.
type HistogramDataPoint struct {
	Attributes        []KeyValue `json:"attributes,omitempty"`
	StartTimeUnixNano string     `json:"startTimeUnixNano,omitempty"` // uint64 as string
	TimeUnixNano      string     `json:"timeUnixNano,omitempty"`      // uint64 as string
	Count             string     `json:"count"`                       // uint64 as string
	Sum               *float64   `json:"sum,omitempty"`
	BucketCounts      []string   `json:"bucketCounts,omitempty"` // []uint64 as strings
	ExplicitBounds    []float64  `json:"explicitBounds,omitempty"`
	Min               *float64   `json:"min,omitempty"`
	Max               *float64   `json:"max,omitempty"`
}

// ---------------------------------------------------------------------------
// Traces (trace.v1)
// ---------------------------------------------------------------------------

// SpanKind enumerates the role of a span in a trace.
type SpanKind = int

// Span kind values (OTLP trace.v1.Span.SpanKind).
const (
	SpanKindUnspecified SpanKind = 0
	SpanKindInternal    SpanKind = 1
	SpanKindServer      SpanKind = 2
	SpanKindClient      SpanKind = 3
	SpanKindProducer    SpanKind = 4
	SpanKindConsumer    SpanKind = 5
)

// StatusCode enumerates a span's terminal status.
type StatusCode = int

// Status code values (OTLP trace.v1.Status.StatusCode).
const (
	StatusUnset StatusCode = 0
	StatusOK    StatusCode = 1
	StatusError StatusCode = 2
)

// TracesRequest is the OTLP ExportTraceServiceRequest POSTed to /v1/traces.
type TracesRequest struct {
	ResourceSpans []ResourceSpans `json:"resourceSpans,omitempty"`
}

// ResourceSpans groups spans for one resource.
type ResourceSpans struct {
	Resource   Resource     `json:"resource"`
	ScopeSpans []ScopeSpans `json:"scopeSpans,omitempty"`
}

// ScopeSpans groups spans for one instrumentation scope.
type ScopeSpans struct {
	Scope Scope  `json:"scope"`
	Spans []Span `json:"spans,omitempty"`
}

// Span is one operation in a trace.
type Span struct {
	TraceID           string      `json:"traceId"` // 16 bytes as 32 hex chars
	SpanID            string      `json:"spanId"`  // 8 bytes as 16 hex chars
	ParentSpanID      string      `json:"parentSpanId,omitempty"`
	Name              string      `json:"name"`
	Kind              SpanKind    `json:"kind,omitempty"`
	StartTimeUnixNano string      `json:"startTimeUnixNano,omitempty"`
	EndTimeUnixNano   string      `json:"endTimeUnixNano,omitempty"`
	Attributes        []KeyValue  `json:"attributes,omitempty"`
	Status            *SpanStatus `json:"status,omitempty"`
}

// SpanStatus is a span's terminal status.
type SpanStatus struct {
	Code    StatusCode `json:"code,omitempty"`
	Message string     `json:"message,omitempty"`
}

// ---------------------------------------------------------------------------
// Logs (logs.v1)
// ---------------------------------------------------------------------------

// SeverityNumber enumerates a log record's severity (OTLP logs.v1).
type SeverityNumber = int

// Canonical severity numbers for the levels Putnami loggers emit.
const (
	SeverityTrace SeverityNumber = 1
	SeverityDebug SeverityNumber = 5
	SeverityInfo  SeverityNumber = 9
	SeverityWarn  SeverityNumber = 13
	SeverityError SeverityNumber = 17
	SeverityFatal SeverityNumber = 21
)

// LogsRequest is the OTLP ExportLogsServiceRequest POSTed to /v1/logs.
type LogsRequest struct {
	ResourceLogs []ResourceLogs `json:"resourceLogs,omitempty"`
}

// ResourceLogs groups log records for one resource.
type ResourceLogs struct {
	Resource  Resource    `json:"resource"`
	ScopeLogs []ScopeLogs `json:"scopeLogs,omitempty"`
}

// ScopeLogs groups log records for one instrumentation scope.
type ScopeLogs struct {
	Scope      Scope       `json:"scope"`
	LogRecords []LogRecord `json:"logRecords,omitempty"`
}

// LogRecord is one structured log entry.
type LogRecord struct {
	TimeUnixNano         string         `json:"timeUnixNano,omitempty"`
	ObservedTimeUnixNano string         `json:"observedTimeUnixNano,omitempty"`
	SeverityNumber       SeverityNumber `json:"severityNumber,omitempty"`
	SeverityText         string         `json:"severityText,omitempty"`
	Body                 *AnyValue      `json:"body,omitempty"`
	Attributes           []KeyValue     `json:"attributes,omitempty"`
	TraceID              string         `json:"traceId,omitempty"`
	SpanID               string         `json:"spanId,omitempty"`
}
