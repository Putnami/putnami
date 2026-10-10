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

// traceSummary is one trace as the traces command prints it.
type traceSummary struct {
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

// traceSummaryFrom converts a generated trace summary into the printed shape.
// The server writes duration as int64 nanoseconds, which is time.Duration.
func traceSummaryFrom(in observabilityapiclient.TraceSummary) traceSummary {
	out := traceSummary{
		TraceID:    deref(in.TraceId),
		Name:       deref(in.Name),
		Start:      deref(in.Start),
		Duration:   time.Duration(deref(in.Duration)),
		SpanCount:  int(deref(in.SpanCount)),
		Attributes: deref(in.Attributes),
	}
	if in.Spans != nil {
		out.Spans = make([]traceSpan, 0, len(*in.Spans))
		for _, s := range *in.Spans {
			out.Spans = append(out.Spans, traceSpan{
				TraceID:      deref(s.TraceId),
				SpanID:       deref(s.SpanId),
				ParentSpanID: deref(s.ParentSpanId),
				Name:         deref(s.Name),
				Start:        deref(s.Start),
				Duration:     time.Duration(deref(s.Duration)),
				Attributes:   deref(s.Attributes),
			})
		}
	}
	return out
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
