// Package telemetry defines the shared push-telemetry protocol every Putnami
// runtime uses to ship observability data to an OpenTelemetry collector.
//
// Putnami targets serverless first: instances scale to zero, are not externally
// addressable, and live too briefly to register with a scraping collector. So
// /metrics scrape is an explicit non-goal (see the platform protocol) and every
// runtime pushes instead. This protocol pins the one push transport we support —
// OTLP/JSON over HTTP — so a workload running on the Go and TypeScript runtimes
// can point both at the same standard collector and get identical envelopes.
//
// The protocol fixes the contract that is observable from outside any single
// runtime:
//
//   - the signal endpoints (/v1/metrics, /v1/traces, /v1/logs) a collector
//     exposes and a runtime POSTs to,
//   - the OTLP/JSON envelope shape per signal (the subset Putnami emits),
//   - the canonical serialization rules that make two runtimes produce
//     byte-identical bytes for equivalent input (sorted attributes, fixed field
//     order, OTLP number/ID encodings),
//   - the resource attributes every runtime populates,
//   - and the exporter behavior contract (flush cadence, final flush on
//     shutdown, drop-on-error, bounded in-memory queue).
//
// What stays out of scope: gRPC OTLP, statsd, the Prometheus push gateway, and
// any Putnami-native push format. OTLP/JSON over HTTP is the one wire we support.
//
// The Go types here are the source of truth for the envelope shape and the
// canonical serializer. TypeScript reimplements the renderer and tests it
// against the same fixture corpus and golden digests, so neither side can drift.
package telemetry

// ProtocolVersion is the current telemetry protocol version. Bumped whenever a
// backwards-incompatible change to the signal endpoints, envelope shapes,
// canonical serialization rules, resource conventions, or exporter contract
// lands.
const ProtocolVersion = 1

// Signal identifies one OTLP signal. A runtime POSTs each signal's envelope to
// the collector base URL joined with the signal's Path.
type Signal string

// Signal values. These mirror the OTLP/HTTP spec's signal endpoints.
const (
	SignalMetrics Signal = "metrics"
	SignalTraces  Signal = "traces"
	SignalLogs    Signal = "logs"
)

// Canonical signal paths, relative to the configured collector base URL. Every
// compliant runtime POSTs "<endpoint><path>" for the corresponding signal. These
// are fixed by the OTLP/HTTP specification.
const (
	PathMetrics = "/v1/metrics"
	PathTraces  = "/v1/traces"
	PathLogs    = "/v1/logs"
)

// ContentType is the canonical request content type for OTLP/JSON over HTTP.
const ContentType = "application/json"

// PathFor returns the canonical collector path for a signal.
func PathFor(s Signal) (string, bool) {
	switch s {
	case SignalMetrics:
		return PathMetrics, true
	case SignalTraces:
		return PathTraces, true
	case SignalLogs:
		return PathLogs, true
	default:
		return "", false
	}
}

// CanonicalSignals is the canonical ordered set of signals every runtime that
// adopts the protocol may export. Adding or removing a signal is a protocol
// change.
var CanonicalSignals = []Signal{SignalMetrics, SignalTraces, SignalLogs}

// Resource attribute keys every Putnami runtime populates on the OTLP Resource.
// service.name and service.version follow OpenTelemetry semantic conventions;
// putnami.framework is a Putnami marker identifying the emitting runtime
// ("go" or "typescript") so a collector can attribute by runtime.
const (
	AttrServiceName      = "service.name"
	AttrServiceVersion   = "service.version"
	AttrPutnamiFramework = "putnami.framework"
	// AttrServiceInstanceID names the process that pushes: two processes of
	// one service carry two values.
	AttrServiceInstanceID = "service.instance.id"
)

// CanonicalResourceAttrs is the canonical ordered set of resource attribute keys
// the protocol reserves. service.name is required; the rest are populated when
// known.
var CanonicalResourceAttrs = []string{
	AttrServiceName,
	AttrServiceVersion,
	AttrPutnamiFramework,
	AttrServiceInstanceID,
}

// Framework values for the putnami.framework resource attribute.
const (
	FrameworkGo         = "go"
	FrameworkTypeScript = "typescript"
)

// ExportContract pins the behavior every conformant exporter implements,
// regardless of language. The values are deliberate: serverless workloads live
// briefly, so a final flush on shutdown is mandatory, which lets the periodic
// cadence stay long and an idle service send few requests; telemetry must never
// block or fail the workload, so collector errors are dropped; and the
// in-memory queue is bounded so a wedged collector cannot grow memory without
// limit.
type ExportContract struct {
	// DefaultFlushIntervalMS is the default periodic flush cadence of log
	// records.
	DefaultFlushIntervalMS int `json:"defaultFlushIntervalMs"`

	// DefaultMetricIntervalMS is the default periodic metric export cadence.
	// OTEL_METRIC_EXPORT_INTERVAL overrides it in every runtime.
	DefaultMetricIntervalMS int `json:"defaultMetricIntervalMs"`

	// DefaultSpanBatchDelayMS is the longest a completed span waits before its
	// batch is exported. OTEL_BSP_SCHEDULE_DELAY overrides it in every runtime.
	DefaultSpanBatchDelayMS int `json:"defaultSpanBatchDelayMs"`

	// MaxSpanBatchSize is the largest span batch. A batch that reaches it is
	// exported without waiting for the delay.
	MaxSpanBatchSize int `json:"maxSpanBatchSize"`

	// DefaultTimeoutMS is the per-request HTTP timeout. A slow collector must
	// not stall the flush loop or the shutdown path.
	DefaultTimeoutMS int `json:"defaultTimeoutMs"`

	// FinalFlushOnShutdown is true when the exporter must flush remaining
	// buffered data during graceful shutdown, so short-lived containers do not
	// silently drop their last batch.
	FinalFlushOnShutdown bool `json:"finalFlushOnShutdown"`

	// DropOnCollectorError is true when the exporter discards a batch (rather
	// than retrying or surfacing an error to the workload) when the collector
	// is unreachable or rejects the request. Telemetry is best-effort: it must
	// never affect application behavior.
	DropOnCollectorError bool `json:"dropOnCollectorError"`

	// MaxQueueRecords is the upper bound on records buffered in memory between
	// flushes. Beyond it the exporter drops the oldest records so a wedged
	// collector cannot exhaust memory.
	MaxQueueRecords int `json:"maxQueueRecords"`
}

// Contract is the top-level telemetry protocol contract every compliant runtime
// publishes via DefaultContract.
type Contract struct {
	Version            int            `json:"version"`
	Signals            []Signal       `json:"signals"`
	ContentType        string         `json:"contentType"`
	ResourceAttributes []string       `json:"resourceAttributes"`
	Export             ExportContract `json:"export"`
}

// DefaultContract returns the canonical telemetry contract Putnami runtimes
// implement unless a later protocol version explicitly changes it.
func DefaultContract() Contract {
	return Contract{
		Version:            ProtocolVersion,
		Signals:            CanonicalSignals,
		ContentType:        ContentType,
		ResourceAttributes: CanonicalResourceAttrs,
		Export: ExportContract{
			DefaultFlushIntervalMS:  10000,
			DefaultMetricIntervalMS: 60000,
			DefaultSpanBatchDelayMS: 30000,
			MaxSpanBatchSize:        512,
			DefaultTimeoutMS:        5000,
			FinalFlushOnShutdown:    true,
			DropOnCollectorError:    true,
			MaxQueueRecords:         10000,
		},
	}
}
