package telemetry

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"go.putnami.dev/logger"
	otlp "go.putnami.dev/protocol/telemetry"
)

// NewOTLPLogSink returns a logger.Sink that batches log entries and POSTs them
// to the collector as OTLP/JSON at /v1/logs. It is opt-in: attach it when
// constructing the application logger, exactly like the console and JSON sinks.
//
//	sink := telemetry.NewOTLPLogSink(cfg)
//	log := logger.New("checkout", logger.LevelInfo, logger.NewJSONSink(), sink)
//	defer sink.Close() // final flush on shutdown
//
// Entries are buffered and flushed on a background ticker (the protocol's 10s
// default) and on Close, so the logging path never blocks on the network. A
// bounded queue drops the oldest entries if the collector wedges.
func NewOTLPLogSink(cfg OTLPConfig) *OTLPLogSink {
	s := &OTLPLogSink{
		client:   newOTLPClient(cfg),
		resource: otlpResourceFromFields(cfg.ServiceName, cfg.ServiceVersion),
		scope:    otlp.Scope{Name: "putnami"},
		log:      logger.Default().Named("telemetry.otlp.logs"),
		maxQueue: otlp.DefaultContract().Export.MaxQueueRecords,
		done:     make(chan struct{}),
	}
	s.wg.Add(1)
	go s.flushLoop(cfg.flushInterval())
	return s
}

// OTLPLogSink is a logger.Sink that exports log records as OTLP/JSON.
type OTLPLogSink struct {
	client   *otlpClient
	resource otlp.Resource
	scope    otlp.Scope
	log      *logger.Logger
	maxQueue int

	mu  sync.Mutex
	buf []otlp.LogRecord

	done      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// Write buffers one log entry. It never touches the network, so it is safe on
// the hot logging path.
func (s *OTLPLogSink) Write(entry logger.LogEntry) {
	rec := convLogRecord(entry)
	s.mu.Lock()
	s.buf = append(s.buf, rec)
	// Bounded queue: drop the oldest records so a wedged collector cannot grow
	// memory without limit.
	if len(s.buf) > s.maxQueue {
		s.buf = s.buf[len(s.buf)-s.maxQueue:]
	}
	s.mu.Unlock()
}

// Flush drains the buffer and POSTs it. Errors are logged and swallowed
// (best-effort): telemetry must never affect the workload.
func (s *OTLPLogSink) Flush() error {
	s.mu.Lock()
	if len(s.buf) == 0 {
		s.mu.Unlock()
		return nil
	}
	records := s.buf
	s.buf = nil
	s.mu.Unlock()

	req := otlp.LogsRequest{ResourceLogs: []otlp.ResourceLogs{{
		Resource:  s.resource,
		ScopeLogs: []otlp.ScopeLogs{{Scope: s.scope, LogRecords: records}},
	}}}
	body, err := otlp.MarshalCanonical(req)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if s.client.http.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.client.http.Timeout)
		defer cancel()
	}
	if err := s.client.post(ctx, otlp.PathLogs, body); err != nil {
		s.log.Warn("dropped log batch: " + err.Error())
	}
	return nil
}

// Close stops the background flusher and performs a final flush.
func (s *OTLPLogSink) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		s.wg.Wait()
	})
	return s.Flush()
}

func (s *OTLPLogSink) flushLoop(interval time.Duration) {
	defer s.wg.Done()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.Flush() //nolint:errcheck // best-effort periodic flush
		case <-s.done:
			return
		}
	}
}

// ---------------------------------------------------------------------------
// LogEntry → OTLP translation
// ---------------------------------------------------------------------------

func convLogRecord(e logger.LogEntry) otlp.LogRecord {
	sevNum, sevText := otlpSeverity(e.Level)
	body := otlp.StringVal(e.Message)
	rec := otlp.LogRecord{
		TimeUnixNano:   nanos(e.Timestamp),
		SeverityNumber: sevNum,
		SeverityText:   sevText,
		Body:           &body,
		Attributes:     logAttrs(e),
	}
	// Trace correlation: the framework logger already extracts the trace ID as a
	// 32-hex string; forward it only when well-formed so we never emit an
	// envelope the protocol parser would reject.
	if len(e.TraceID) == 32 {
		rec.TraceID = e.TraceID
	}
	return rec
}

// logAttrs gathers a log entry's structured context, slog attributes, logger
// name, and error fields into a sorted OTLP attribute slice.
func logAttrs(e logger.LogEntry) []otlp.KeyValue {
	// Cap covers logger.name, the context/slog attrs, and up to three error fields.
	attrs := make([]otlp.KeyValue, 0, len(e.Context)+len(e.Attrs)+4)
	if e.Logger != "" {
		attrs = append(attrs, otlp.Attr("logger.name", otlp.StringVal(e.Logger)))
	}
	for k, v := range e.Context {
		attrs = appendFlatAttr(attrs, k, v, 0)
	}
	for _, a := range e.Attrs {
		attrs = appendSlogAttr(attrs, a)
	}
	if e.Error != nil {
		attrs = appendErrorAttrs(attrs, e.Error)
	}
	if len(attrs) == 0 {
		return nil
	}
	return otlp.SortAttrs(attrs)
}

// appendSlogAttr translates an slog attr into OTLP attributes. Structured slog
// groups and slog.Any maps use the same dotted-key flattening as logger context
// so the boundary record fields are queryable in both sinks.
func appendSlogAttr(attrs []otlp.KeyValue, a slog.Attr) []otlp.KeyValue {
	v := a.Value.Resolve()
	if a.Key == "error" {
		if info, ok := v.Any().(*logger.ErrorInfo); ok {
			return appendErrorAttrs(attrs, info)
		}
	}
	switch v.Kind() {
	case slog.KindGroup:
		return appendFlatAttr(attrs, a.Key, slogValueToAny(v), 0)
	case slog.KindAny:
		return appendFlatAttr(attrs, a.Key, v.Any(), 0)
	default:
		return append(attrs, otlp.Attr(a.Key, slogToValue(v)))
	}
}

// appendErrorAttrs adds the OTLP semantic error fields shared by Error-level
// records and the logger.ErrorAttr helper used on warning records.
func appendErrorAttrs(attrs []otlp.KeyValue, info *logger.ErrorInfo) []otlp.KeyValue {
	if info.Name != "" {
		attrs = append(attrs, otlp.Attr("error.type", otlp.StringVal(info.Name)))
	}
	if info.Message != "" {
		attrs = append(attrs, otlp.Attr("error.message", otlp.StringVal(info.Message)))
	}
	if info.Code != "" {
		attrs = append(attrs, otlp.Attr("error.code", otlp.StringVal(info.Code)))
	}
	return attrs
}

// otlpSeverity maps an slog level to the OTLP severity number and text.
func otlpSeverity(l slog.Level) (otlp.SeverityNumber, string) {
	switch {
	case l >= slog.LevelError:
		return otlp.SeverityError, "error"
	case l >= slog.LevelWarn:
		return otlp.SeverityWarn, "warn"
	case l >= slog.LevelInfo:
		return otlp.SeverityInfo, "info"
	default:
		return otlp.SeverityDebug, "debug"
	}
}

// maxFlattenDepth bounds how deeply nested plain map values are flattened into
// dotted attribute keys before the remaining sub-tree is JSON-encoded.
const maxFlattenDepth = 2

// appendFlatAttr flattens a nested plain map value into dotted attribute keys
// (e.g. {"http": {"method": "GET"}} → http.method=GET), bounded to
// maxFlattenDepth. Non-map values — including arrays and maps at the depth limit
// — are emitted as a single attribute via anyToValue, which JSON-encodes
// composites so they survive as structured text instead of Go's %v map[…] form.
func appendFlatAttr(attrs []otlp.KeyValue, key string, v any, depth int) []otlp.KeyValue {
	if m, ok := v.(map[string]any); ok && depth < maxFlattenDepth {
		for sk, sv := range m {
			attrs = appendFlatAttr(attrs, key+"."+sk, sv, depth+1)
		}
		return attrs
	}
	return append(attrs, otlp.Attr(key, anyToValue(v)))
}

func anyToValue(v any) otlp.AnyValue {
	switch t := v.(type) {
	case string:
		return otlp.StringVal(t)
	case bool:
		return otlp.BoolVal(t)
	case int:
		return otlp.IntVal(int64(t))
	case int64:
		return otlp.IntVal(t)
	case float64:
		return otlp.DoubleVal(t)
	case interface{ String() string }:
		return otlp.StringVal(t.String())
	}
	// Composite values (arrays, and maps at/over the flatten depth) are
	// JSON-encoded so an array renders as ["a","b"] and an object as {"k":…}
	// rather than Go's fmt %v syntax, keeping the value machine-readable.
	if b, err := json.Marshal(v); err == nil {
		return otlp.StringVal(string(b))
	}
	return otlp.StringVal(stringify(v))
}

func slogToValue(v slog.Value) otlp.AnyValue {
	switch v.Kind() {
	case slog.KindString:
		return otlp.StringVal(v.String())
	case slog.KindBool:
		return otlp.BoolVal(v.Bool())
	case slog.KindInt64:
		return otlp.IntVal(v.Int64())
	case slog.KindUint64:
		return otlp.IntVal(int64(v.Uint64())) //nolint:gosec // log attribute value; a lossy wrap is acceptable and not a security boundary
	case slog.KindFloat64:
		return otlp.DoubleVal(v.Float64())
	default:
		return otlp.StringVal(v.String())
	}
}

// slogValueToAny resolves a structured slog value into the ordinary Go shape
// that appendFlatAttr understands. In particular, slog groups become maps
// rather than their opaque []slog.Attr representation.
func slogValueToAny(v slog.Value) any {
	v = v.Resolve()
	if v.Kind() != slog.KindGroup {
		return v.Any()
	}
	group := v.Group()
	m := make(map[string]any, len(group))
	for _, a := range group {
		m[a.Key] = slogValueToAny(a.Value)
	}
	return m
}

func stringify(v any) string {
	if s, ok := v.(interface{ String() string }); ok {
		return s.String()
	}
	if s, ok := v.(string); ok {
		return s
	}
	return slog.AnyValue(v).String()
}
