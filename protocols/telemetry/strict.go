package telemetry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Telemetry error codes for strict validation failures. Runtimes and
// conformance suites key off these — keep them in sync with ValidErrorCodes.
const (
	ErrorCodeInvalidMetrics     = "telemetry.invalid_metrics"
	ErrorCodeInvalidTraces      = "telemetry.invalid_traces"
	ErrorCodeInvalidLogs        = "telemetry.invalid_logs"
	ErrorCodeInvalidMetric      = "telemetry.invalid_metric"
	ErrorCodeInvalidDataPoint   = "telemetry.invalid_data_point"
	ErrorCodeInvalidAttribute   = "telemetry.invalid_attribute"
	ErrorCodeInvalidResource    = "telemetry.invalid_resource"
	ErrorCodeInvalidSpan        = "telemetry.invalid_span"
	ErrorCodeInvalidTraceID     = "telemetry.invalid_trace_id"
	ErrorCodeInvalidSpanID      = "telemetry.invalid_span_id"
	ErrorCodeInvalidTemporality = "telemetry.invalid_temporality"
	ErrorCodeInvalidLogRecord   = "telemetry.invalid_log_record"
	ErrorCodeInvalidSeverity    = "telemetry.invalid_severity"
)

// ValidErrorCodes enumerates the canonical telemetry error taxonomy.
var ValidErrorCodes = map[string]bool{
	ErrorCodeInvalidMetrics:     true,
	ErrorCodeInvalidTraces:      true,
	ErrorCodeInvalidLogs:        true,
	ErrorCodeInvalidMetric:      true,
	ErrorCodeInvalidDataPoint:   true,
	ErrorCodeInvalidAttribute:   true,
	ErrorCodeInvalidResource:    true,
	ErrorCodeInvalidSpan:        true,
	ErrorCodeInvalidTraceID:     true,
	ErrorCodeInvalidSpanID:      true,
	ErrorCodeInvalidTemporality: true,
	ErrorCodeInvalidLogRecord:   true,
	ErrorCodeInvalidSeverity:    true,
}

var (
	traceIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	spanIDPattern  = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// ---------------------------------------------------------------------------
// Shared validation
// ---------------------------------------------------------------------------

// valueKinds counts how many scalar kinds an AnyValue sets. Exactly one is
// required.
func valueKinds(v AnyValue) int {
	n := 0
	if v.StringValue != nil {
		n++
	}
	if v.BoolValue != nil {
		n++
	}
	if v.IntValue != nil {
		n++
	}
	if v.DoubleValue != nil {
		n++
	}
	return n
}

// validateAttrs checks that every attribute has a non-empty key and exactly one
// value kind.
func validateAttrs(attrs []KeyValue, field string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	for i, kv := range attrs {
		at := fmt.Sprintf("%s[%d]", field, i)
		if kv.Key == "" {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidAttribute, at+".key",
				"attribute key is required"))
		}
		if n := valueKinds(kv.Value); n != 1 {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidAttribute, at+".value",
				"attribute %q must set exactly one value kind, got %d", kv.Key, n))
		}
	}
	return diags
}

func validateResource(r Resource, field string) []diag.Diagnostic {
	return validateAttrs(r.Attributes, field+".attributes")
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

// ParseMetricsRequest strict-parses an OTLP metrics request (unknown fields
// rejected).
func ParseMetricsRequest(data []byte) (*MetricsRequest, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var req MetricsRequest
	if err := dec.Decode(&req); err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidMetrics, "",
			"invalid metrics request JSON: %v", err)}
	}
	return &req, nil
}

// ValidateMetricsRequest enforces the metrics envelope rules.
func ValidateMetricsRequest(req MetricsRequest) []diag.Diagnostic {
	var diags []diag.Diagnostic
	for ri, rm := range req.ResourceMetrics {
		rf := fmt.Sprintf("resourceMetrics[%d]", ri)
		diags = append(diags, validateResource(rm.Resource, rf+".resource")...)
		for si, sm := range rm.ScopeMetrics {
			sf := fmt.Sprintf("%s.scopeMetrics[%d]", rf, si)
			for mi, m := range sm.Metrics {
				diags = append(diags, validateMetric(m, fmt.Sprintf("%s.metrics[%d]", sf, mi))...)
			}
		}
	}
	return diags
}

func validateMetric(m Metric, field string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if m.Name == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidMetric, field+".name",
			"metric name is required"))
	}
	set := 0
	if m.Sum != nil {
		set++
	}
	if m.Gauge != nil {
		set++
	}
	if m.Histogram != nil {
		set++
	}
	if set != 1 {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidMetric, field,
			"metric %q must set exactly one of sum/gauge/histogram, got %d", m.Name, set))
		return diags
	}

	switch {
	case m.Sum != nil:
		if !validTemporality(m.Sum.AggregationTemporality) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidTemporality, field+".sum.aggregationTemporality",
				"sum aggregationTemporality %d is not a valid OTLP temporality", m.Sum.AggregationTemporality))
		}
		for di, dp := range m.Sum.DataPoints {
			diags = append(diags, validateNumberDataPoint(dp, fmt.Sprintf("%s.sum.dataPoints[%d]", field, di))...)
		}
	case m.Gauge != nil:
		for di, dp := range m.Gauge.DataPoints {
			diags = append(diags, validateNumberDataPoint(dp, fmt.Sprintf("%s.gauge.dataPoints[%d]", field, di))...)
		}
	case m.Histogram != nil:
		if !validTemporality(m.Histogram.AggregationTemporality) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidTemporality, field+".histogram.aggregationTemporality",
				"histogram aggregationTemporality %d is not a valid OTLP temporality", m.Histogram.AggregationTemporality))
		}
		for di, dp := range m.Histogram.DataPoints {
			diags = append(diags, validateHistogramDataPoint(dp, fmt.Sprintf("%s.histogram.dataPoints[%d]", field, di))...)
		}
	}
	return diags
}

func validTemporality(t AggregationTemporality) bool {
	return t == TemporalityUnspecified || t == TemporalityDelta || t == TemporalityCumulative
}

func validateNumberDataPoint(dp NumberDataPoint, field string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	diags = append(diags, validateAttrs(dp.Attributes, field+".attributes")...)
	set := 0
	if dp.AsInt != nil {
		set++
	}
	if dp.AsDouble != nil {
		set++
	}
	if set != 1 {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidDataPoint, field,
			"number data point must set exactly one of asInt/asDouble, got %d", set))
	}
	if dp.TimeUnixNano == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidDataPoint, field+".timeUnixNano",
			"number data point timeUnixNano is required"))
	}
	return diags
}

func validateHistogramDataPoint(dp HistogramDataPoint, field string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	diags = append(diags, validateAttrs(dp.Attributes, field+".attributes")...)
	if dp.Count == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidDataPoint, field+".count",
			"histogram data point count is required"))
	}
	if dp.TimeUnixNano == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidDataPoint, field+".timeUnixNano",
			"histogram data point timeUnixNano is required"))
	}
	// bucketCounts must have exactly len(explicitBounds)+1 entries when present.
	if len(dp.BucketCounts) > 0 && len(dp.BucketCounts) != len(dp.ExplicitBounds)+1 {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidDataPoint, field+".bucketCounts",
			"bucketCounts has %d entries, want explicitBounds+1 (%d)", len(dp.BucketCounts), len(dp.ExplicitBounds)+1))
	}
	return diags
}

// ParseAndValidateMetrics strict-parses then validates. Cross-language suites
// feed the shared fixture corpus through this entry point.
func ParseAndValidateMetrics(data []byte) (*MetricsRequest, []diag.Diagnostic) {
	req, diags := ParseMetricsRequest(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return req, append(diags, ValidateMetricsRequest(*req)...)
}

// ---------------------------------------------------------------------------
// Traces
// ---------------------------------------------------------------------------

// ParseTracesRequest strict-parses an OTLP traces request.
func ParseTracesRequest(data []byte) (*TracesRequest, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var req TracesRequest
	if err := dec.Decode(&req); err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidTraces, "",
			"invalid traces request JSON: %v", err)}
	}
	return &req, nil
}

// ValidateTracesRequest enforces the traces envelope rules.
func ValidateTracesRequest(req TracesRequest) []diag.Diagnostic {
	var diags []diag.Diagnostic
	for ri, rs := range req.ResourceSpans {
		rf := fmt.Sprintf("resourceSpans[%d]", ri)
		diags = append(diags, validateResource(rs.Resource, rf+".resource")...)
		for si, ss := range rs.ScopeSpans {
			sf := fmt.Sprintf("%s.scopeSpans[%d]", rf, si)
			for spi, sp := range ss.Spans {
				diags = append(diags, validateSpan(sp, fmt.Sprintf("%s.spans[%d]", sf, spi))...)
			}
		}
	}
	return diags
}

func validateSpan(s Span, field string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if !traceIDPattern.MatchString(s.TraceID) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidTraceID, field+".traceId",
			"traceId %q must be 32 lowercase hex chars", s.TraceID))
	}
	if !spanIDPattern.MatchString(s.SpanID) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSpanID, field+".spanId",
			"spanId %q must be 16 lowercase hex chars", s.SpanID))
	}
	if s.ParentSpanID != "" && !spanIDPattern.MatchString(s.ParentSpanID) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSpanID, field+".parentSpanId",
			"parentSpanId %q must be 16 lowercase hex chars", s.ParentSpanID))
	}
	if s.Name == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSpan, field+".name",
			"span name is required"))
	}
	if s.Kind < SpanKindUnspecified || s.Kind > SpanKindConsumer {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSpan, field+".kind",
			"span kind %d is out of range", s.Kind))
	}
	if s.Status != nil && (s.Status.Code < StatusUnset || s.Status.Code > StatusError) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSpan, field+".status.code",
			"span status code %d is out of range", s.Status.Code))
	}
	diags = append(diags, validateAttrs(s.Attributes, field+".attributes")...)
	return diags
}

// ParseAndValidateTraces strict-parses then validates.
func ParseAndValidateTraces(data []byte) (*TracesRequest, []diag.Diagnostic) {
	req, diags := ParseTracesRequest(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return req, append(diags, ValidateTracesRequest(*req)...)
}

// ---------------------------------------------------------------------------
// Logs
// ---------------------------------------------------------------------------

// ParseLogsRequest strict-parses an OTLP logs request.
func ParseLogsRequest(data []byte) (*LogsRequest, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var req LogsRequest
	if err := dec.Decode(&req); err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidLogs, "",
			"invalid logs request JSON: %v", err)}
	}
	return &req, nil
}

// ValidateLogsRequest enforces the logs envelope rules.
func ValidateLogsRequest(req LogsRequest) []diag.Diagnostic {
	var diags []diag.Diagnostic
	for ri, rl := range req.ResourceLogs {
		rf := fmt.Sprintf("resourceLogs[%d]", ri)
		diags = append(diags, validateResource(rl.Resource, rf+".resource")...)
		for si, sl := range rl.ScopeLogs {
			sf := fmt.Sprintf("%s.scopeLogs[%d]", rf, si)
			for li, lr := range sl.LogRecords {
				diags = append(diags, validateLogRecord(lr, fmt.Sprintf("%s.logRecords[%d]", sf, li))...)
			}
		}
	}
	return diags
}

func validateLogRecord(r LogRecord, field string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if r.SeverityNumber < 0 || r.SeverityNumber > 24 {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSeverity, field+".severityNumber",
			"severityNumber %d is out of OTLP range 1..24", r.SeverityNumber))
	}
	if r.Body != nil && valueKinds(*r.Body) != 1 {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidLogRecord, field+".body",
			"log body must set exactly one value kind"))
	}
	if r.TraceID != "" && !traceIDPattern.MatchString(r.TraceID) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidTraceID, field+".traceId",
			"traceId %q must be 32 lowercase hex chars", r.TraceID))
	}
	if r.SpanID != "" && !spanIDPattern.MatchString(r.SpanID) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSpanID, field+".spanId",
			"spanId %q must be 16 lowercase hex chars", r.SpanID))
	}
	diags = append(diags, validateAttrs(r.Attributes, field+".attributes")...)
	return diags
}

// ParseAndValidateLogs strict-parses then validates.
func ParseAndValidateLogs(data []byte) (*LogsRequest, []diag.Diagnostic) {
	req, diags := ParseLogsRequest(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return req, append(diags, ValidateLogsRequest(*req)...)
}
