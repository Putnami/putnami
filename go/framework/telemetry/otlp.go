package telemetry

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"

	otlp "go.putnami.dev/protocol/telemetry"
)

// OTLPConfig configures the built-in OTLP/JSON-over-HTTP exporters. It is the
// one push transport Putnami supports (see the telemetry protocol): metrics,
// traces, and logs are POSTed as OTLP/JSON to a standard collector. No OTel
// exporter SDK is pulled in — the envelopes are rendered through
// go.putnami.dev/protocol/telemetry.
//
// Construct the exporters from a shared OTLPConfig and wire them into the
// plugin:
//
//	cfg := telemetry.OTLPConfig{Endpoint: "https://collector:4318", ServiceName: "checkout"}
//	app.New("checkout").Use(telemetry.NewPlugin(telemetry.Config{
//	    ServiceName:   "checkout",
//	    OTLP:          &cfg,                       // auto-wires metrics + traces
//	}))
//	log := logger.New("checkout", logger.LevelInfo, telemetry.NewOTLPLogSink(cfg))
type OTLPConfig struct {
	// Endpoint is the collector base URL (e.g. "https://collector:4318"). The
	// signal path (/v1/metrics, /v1/traces, /v1/logs) is appended per request.
	Endpoint string

	// ServiceName and ServiceVersion populate the OTLP resource. They default
	// from the plugin Config when the exporters are auto-wired.
	ServiceName    string
	ServiceVersion string

	// Headers are sent on every request (e.g. tenant routing). Optional.
	Headers map[string]string

	// BearerToken, when set, is sent as "Authorization: Bearer <token>".
	BearerToken string

	// BearerTokenSource, when set, resolves the bearer token per request and
	// wins over BearerToken. Use it for credentials that expire and refresh —
	// NewGCPIDTokenSource is the built-in source for GCP service-to-service
	// auth. A source error fails the request, and the exporter drops the batch
	// like any other collector failure.
	BearerTokenSource func(ctx context.Context) (string, error)

	// HTTPClient, when set, is used verbatim for collector requests. This lets
	// callers provide a transport that attaches refreshing credentials. The
	// caller owns the injected client's timeout; Timeout below is ignored.
	HTTPClient *http.Client

	// FlushInterval is the periodic flush cadence. Defaults to the protocol's
	// 10s when zero. A final flush always runs on shutdown.
	FlushInterval time.Duration

	// Timeout bounds each HTTP request. Defaults to the protocol's 5s when zero.
	Timeout time.Duration
}

func (c OTLPConfig) flushInterval() time.Duration {
	if c.FlushInterval > 0 {
		return c.FlushInterval
	}
	return time.Duration(otlp.DefaultContract().Export.DefaultFlushIntervalMS) * time.Millisecond
}

func (c OTLPConfig) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return time.Duration(otlp.DefaultContract().Export.DefaultTimeoutMS) * time.Millisecond
}

// otlpClient POSTs OTLP/JSON envelopes to the collector. It is best-effort:
// transport and non-2xx responses are surfaced as errors, and each exporter
// drops the batch (the telemetry protocol's drop-on-collector-error rule) so a
// wedged collector never affects the workload.
type otlpClient struct {
	endpoint     string
	headers      map[string]string
	bearer       string
	bearerSource func(ctx context.Context) (string, error)
	http         *http.Client
}

func newOTLPClient(cfg OTLPConfig) *otlpClient {
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: cfg.timeout()}
	}
	return &otlpClient{
		endpoint:     strings.TrimRight(cfg.Endpoint, "/"),
		headers:      cfg.Headers,
		bearer:       cfg.BearerToken,
		bearerSource: cfg.BearerTokenSource,
		http:         httpClient,
	}
}

// post sends body to "<endpoint><path>". Returns an error on transport failure
// or a non-2xx status; the caller decides whether to drop.
func (c *otlpClient) post(ctx context.Context, path string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", otlp.ContentType)
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if c.bearerSource != nil {
		token, err := c.bearerSource(ctx)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	} else if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close on HTTP response
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &collectorError{status: resp.StatusCode}
	}
	return nil
}

// collectorError reports a non-2xx collector response.
type collectorError struct{ status int }

func (e *collectorError) Error() string {
	return "otlp collector returned status " + http.StatusText(e.status)
}

// ---------------------------------------------------------------------------
// Shared conversion helpers
// ---------------------------------------------------------------------------

// otlpAttrs converts OTel attributes into a sorted OTLP KeyValue slice. OTel
// attribute sets are already key-sorted; we sort defensively so output stays
// byte-stable regardless of source ordering.
func otlpAttrs(kvs []attribute.KeyValue) []otlp.KeyValue {
	if len(kvs) == 0 {
		return nil
	}
	out := make([]otlp.KeyValue, 0, len(kvs))
	for _, kv := range kvs {
		out = append(out, otlp.Attr(string(kv.Key), otlpValue(kv.Value)))
	}
	return otlp.SortAttrs(out)
}

// otlpValue maps an OTel attribute value to an OTLP AnyValue. Non-scalar kinds
// (slices) are flattened to their string form, which the collector accepts.
func otlpValue(v attribute.Value) otlp.AnyValue {
	switch v.Type() {
	case attribute.BOOL:
		return otlp.BoolVal(v.AsBool())
	case attribute.INT64:
		return otlp.IntVal(v.AsInt64())
	case attribute.FLOAT64:
		return otlp.DoubleVal(v.AsFloat64())
	case attribute.STRING:
		return otlp.StringVal(v.AsString())
	default:
		return otlp.StringVal(v.Emit())
	}
}

// otlpResourceFromSDK builds an OTLP resource from an OTel SDK resource,
// ensuring putnami.framework=go is present so collectors can attribute by
// runtime. A nil resource yields one carrying just the framework marker.
func otlpResourceFromSDK(res *resource.Resource) otlp.Resource {
	var kvs []attribute.KeyValue
	if res != nil {
		kvs = res.Attributes()
	}
	attrs := otlpAttrs(kvs)
	return otlp.Resource{Attributes: ensureFramework(attrs)}
}

// otlpResourceFromFields builds an OTLP resource from explicit service fields
// plus the framework marker. Used by the log sink, which has no SDK resource.
func otlpResourceFromFields(serviceName, serviceVersion string) otlp.Resource {
	m := map[string]string{otlp.AttrPutnamiFramework: otlp.FrameworkGo}
	if serviceName != "" {
		m[otlp.AttrServiceName] = serviceName
	}
	if serviceVersion != "" {
		m[otlp.AttrServiceVersion] = serviceVersion
	}
	return otlp.ResourceFromStrings(m)
}

// ensureFramework appends putnami.framework=go when absent and re-sorts.
func ensureFramework(attrs []otlp.KeyValue) []otlp.KeyValue {
	for _, kv := range attrs {
		if kv.Key == otlp.AttrPutnamiFramework {
			return attrs
		}
	}
	attrs = append(attrs, otlp.Attr(otlp.AttrPutnamiFramework, otlp.StringVal(otlp.FrameworkGo)))
	return otlp.SortAttrs(attrs)
}

// nanos formats a time as the OTLP uint64 unix-nano string. Zero times yield ""
// so the optional field is omitted.
func nanos(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	ns := t.UnixNano()
	if ns < 0 {
		// Pre-epoch times have no valid OTLP uint64 unix-nano representation;
		// omit rather than wrap around on the int64 -> uint64 conversion.
		return ""
	}
	return otlp.FormatUint(uint64(ns))
}
