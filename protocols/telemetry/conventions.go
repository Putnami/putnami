package telemetry

// HTTP server metric conventions. Every runtime records each HTTP request it
// serves under these two metrics, with the same unit, labels and bucket
// bounds, so a dashboard or an alert finds a Go and a TypeScript service under
// the same series. The names and label keys are Putnami's own, not the current
// OpenTelemetry semantic conventions.
//
// The values are also committed in fixtures/conventions/http-server-metrics.json,
// which the TypeScript runtime reads in its tests; conventions_test.go keeps
// these constants equal to that file.
const (
	// MetricHTTPServerRequestCount counts the HTTP requests a service answers.
	MetricHTTPServerRequestCount = "http.server.request_count"
	// MetricHTTPServerDuration is the histogram of HTTP request durations.
	MetricHTTPServerDuration = "http.server.duration"

	// UnitMilliseconds is the unit of MetricHTTPServerDuration.
	UnitMilliseconds = "ms"

	// AttrHTTPMethod labels a request with its HTTP method.
	AttrHTTPMethod = "http.method"
	// AttrHTTPRoute labels a request with its matched route pattern, never
	// with the request path, so the label stays bounded.
	AttrHTTPRoute = "http.route"
	// AttrHTTPStatusCode labels a request with its response status, an integer.
	AttrHTTPStatusCode = "http.status_code"
)

// MetricKind names the OTLP data type a metric convention renders as.
type MetricKind string

// MetricKind values.
const (
	MetricKindCounter   MetricKind = "counter"
	MetricKindHistogram MetricKind = "histogram"
)

// MetricConvention is one metric every runtime records under the same name,
// unit, description and label keys.
type MetricConvention struct {
	Name        string     `json:"name"`
	Kind        MetricKind `json:"kind"`
	Unit        string     `json:"unit,omitempty"`
	Description string     `json:"description"`
	// Attributes are the label keys of every data point, in canonical order.
	Attributes []string `json:"attributes"`
	// Bounds are the explicit bucket bounds of a histogram, ascending.
	Bounds []float64 `json:"bounds,omitempty"`
}

// HTTPServerMetricAttrs are the label keys of both HTTP server metrics.
func HTTPServerMetricAttrs() []string {
	return []string{AttrHTTPMethod, AttrHTTPRoute, AttrHTTPStatusCode}
}

// HTTPServerDurationBounds returns the explicit bucket bounds of
// MetricHTTPServerDuration, in milliseconds: the OpenTelemetry SDK's default
// histogram bounds.
func HTTPServerDurationBounds() []float64 {
	return []float64{0, 5, 10, 25, 50, 75, 100, 250, 500, 750, 1000, 2500, 5000, 7500, 10000}
}

// HTTPServerMetrics returns the HTTP server metric conventions, in name order.
func HTTPServerMetrics() []MetricConvention {
	return []MetricConvention{
		{
			Name:        MetricHTTPServerDuration,
			Kind:        MetricKindHistogram,
			Unit:        UnitMilliseconds,
			Description: "HTTP request duration in milliseconds",
			Attributes:  HTTPServerMetricAttrs(),
			Bounds:      HTTPServerDurationBounds(),
		},
		{
			Name:        MetricHTTPServerRequestCount,
			Kind:        MetricKindCounter,
			Description: "Total HTTP requests",
			Attributes:  HTTPServerMetricAttrs(),
		},
	}
}
