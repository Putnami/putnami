package observabilitycli

import (
	"time"

	observabilityapiclient "go.putnami.dev/cloud/clients/observability-api/go"
)

// The item shapes the logs, traces and metrics commands print. Each one keeps
// the --output=jsonl bytes these commands have always written: keys in the
// server's declaration order, and the same fields written even when empty.
// observability-api's generated types (observabilityapiclient) carry the same
// wire fields, but they marshal keys alphabetically and omit every absent
// field, so the CLI converts each generated item into one of these once and
// renders it from here.

// logEntry is one log record as the logs command prints it.
type logEntry struct {
	Timestamp    time.Time         `json:"timestamp"`
	Severity     int32             `json:"severity,omitempty"`
	SeverityText string            `json:"severityText,omitempty"`
	Body         string            `json:"body,omitempty"`
	TraceID      string            `json:"traceId,omitempty"`
	SpanID       string            `json:"spanId,omitempty"`
	Attributes   map[string]string `json:"attributes,omitempty"`
}

// metricPoint is one sample of a metric series as the metrics command prints it.
type metricPoint struct {
	Timestamp time.Time `json:"timestamp"`
	Value     float64   `json:"value"`
}

// metricSeries is one metric time series as the metrics command prints it.
type metricSeries struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	Points []metricPoint     `json:"points"`
}

// traceSpan is one span of a trace as the traces command prints it.
type traceSpan struct {
	TraceID      string            `json:"traceId"`
	SpanID       string            `json:"spanId"`
	ParentSpanID string            `json:"parentSpanId,omitempty"`
	Name         string            `json:"name"`
	Start        time.Time         `json:"start"`
	Duration     time.Duration     `json:"duration"`
	Attributes   map[string]string `json:"attributes,omitempty"`
}

// traceSummary is one trace of the listing as the traces command prints it.
// observability-api's listing reads Cloud Trace's ROOTSPAN view, so Spans
// holds the trace's root span alone and the listing's spanCount is always 1
// whatever the trace holds. The summary therefore carries no span count: a
// count that reads as the whole trace would be wrong. `traces --trace <id>`
// prints the whole trace as a traceDetail.
type traceSummary struct {
	TraceID    string            `json:"traceId"`
	Name       string            `json:"name"`
	Start      time.Time         `json:"start"`
	Duration   time.Duration     `json:"duration"`
	Attributes map[string]string `json:"attributes,omitempty"`
	Spans      []traceSpan       `json:"spans,omitempty"`
}

// traceDetail is one whole trace as `traces --trace <id>` prints it: every span
// observability-api's trace read returns, and their count.
type traceDetail struct {
	TraceID    string            `json:"traceId"`
	Name       string            `json:"name"`
	Start      time.Time         `json:"start"`
	Duration   time.Duration     `json:"duration"`
	SpanCount  int               `json:"spanCount"`
	Attributes map[string]string `json:"attributes,omitempty"`
	Spans      []traceSpan       `json:"spans,omitempty"`
}

// logEntryFrom converts a generated log entry into the printed shape. The live
// tail and the paged query both go through it, so they share one renderer.
func logEntryFrom(in observabilityapiclient.LogEntry) logEntry {
	return logEntry{
		Timestamp:    deref(in.Timestamp),
		Severity:     deref(in.Severity),
		SeverityText: deref(in.SeverityText),
		Body:         deref(in.Body),
		TraceID:      deref(in.TraceId),
		SpanID:       deref(in.SpanId),
		Attributes:   deref(in.Attributes),
	}
}

// metricSeriesFrom converts a generated metric series into the printed shape.
// A present but empty points list stays an empty list ("points":[]); an absent
// one stays nil ("points":null), as before.
func metricSeriesFrom(in observabilityapiclient.MetricSeries) metricSeries {
	out := metricSeries{
		Name:   deref(in.Name),
		Labels: deref(in.Labels),
	}
	if in.Points != nil {
		out.Points = make([]metricPoint, 0, len(*in.Points))
		for _, p := range *in.Points {
			out.Points = append(out.Points, metricPoint{Timestamp: deref(p.Timestamp), Value: deref(p.Value)})
		}
	}
	return out
}

// traceSummaryFrom converts a generated listing item into the printed shape,
// dropping the listing's root-span-only spanCount (see traceSummary). The
// server writes duration as int64 nanoseconds, which is time.Duration.
func traceSummaryFrom(in observabilityapiclient.TraceSummary) traceSummary {
	return traceSummary{
		TraceID:    deref(in.TraceId),
		Name:       deref(in.Name),
		Start:      deref(in.Start),
		Duration:   time.Duration(deref(in.Duration)),
		Attributes: deref(in.Attributes),
		Spans:      traceSpansFrom(in.Spans),
	}
}

// traceDetailFrom converts the generated answer of the trace read into the
// printed shape. SpanCount is the number of spans printed, so it can never
// disagree with Spans.
func traceDetailFrom(in observabilityapiclient.TraceSummary) traceDetail {
	spans := traceSpansFrom(in.Spans)
	return traceDetail{
		TraceID:    deref(in.TraceId),
		Name:       deref(in.Name),
		Start:      deref(in.Start),
		Duration:   time.Duration(deref(in.Duration)),
		SpanCount:  len(spans),
		Attributes: deref(in.Attributes),
		Spans:      spans,
	}
}

// traceSpansFrom converts a generated span list, absent or not.
func traceSpansFrom(in *[]observabilityapiclient.Span) []traceSpan {
	return convertAll(in, func(s observabilityapiclient.Span) traceSpan {
		return traceSpan{
			TraceID:      deref(s.TraceId),
			SpanID:       deref(s.SpanId),
			ParentSpanID: deref(s.ParentSpanId),
			Name:         deref(s.Name),
			Start:        deref(s.Start),
			Duration:     time.Duration(deref(s.Duration)),
			Attributes:   deref(s.Attributes),
		}
	})
}

// convertAll converts a generated item list, absent or not, with convert.
func convertAll[In, Out any](in *[]In, convert func(In) Out) []Out {
	if in == nil {
		return nil
	}
	out := make([]Out, 0, len(*in))
	for _, item := range *in {
		out = append(out, convert(item))
	}
	return out
}

// deref reads an optional generated field, or its zero value when absent.
func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
