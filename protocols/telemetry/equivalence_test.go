package telemetry

import (
	"os"
	"path/filepath"
	"testing"
)

// These tests pin the canonical OTLP/JSON bytes and SHA-256 digests that the
// renderer produces for a fixed input. The TypeScript counterpart
// (typescript/framework/application/test/telemetry/cross-language.test.ts)
// builds the same input and asserts MarshalCanonical produces byte-identical
// output and the same digest. A change to either renderer that breaks
// byte-equivalence fails both suites.
//
// When you intentionally change the canonical encoding or a golden input,
// regenerate the goldens (UPDATE_GOLDEN=1 go test ./...), copy the printed
// digests into the constants below, and update the TS test in lockstep.

const (
	goldenMetricsDigest = "554e2e93797529779aad0d8bceca426c2f91096712544e097568a88436236c03"
	goldenTracesDigest  = "7517ef04da08f3fefce56eb9f547ff2fec7c9bde46dbeec285939175221b9845"
	goldenLogsDigest    = "acb22c6a4901668d1f010ac8b02eed2a8f3c0338766b7f9ed975a3309bc88d47"
)

// goldenMetricsInput is the fixed metrics request the equivalence tests render.
// Attributes are built through AttrsFromStrings (sorted) and metrics are listed
// in name order, matching what the TS renderer must produce.
func goldenMetricsInput() MetricsRequest {
	startNano := FormatUint(1699999999000000000)
	timeNano := FormatUint(1700000000000000000)
	count := FormatUint(2)
	sum := 30.0
	minV := 10.0
	maxV := 20.0
	gaugeV := 3.0
	asInt := FormatInt(5)
	return MetricsRequest{
		ResourceMetrics: []ResourceMetrics{
			{
				Resource: ResourceFromStrings(map[string]string{
					AttrPutnamiFramework: "go",
					AttrServiceName:      "checkout",
					AttrServiceVersion:   "1.4.2",
				}),
				ScopeMetrics: []ScopeMetrics{
					{
						Scope: Scope{Name: "putnami", Version: "1.0.0"},
						Metrics: []Metric{
							{
								Name: "http.duration",
								Unit: "ms",
								Histogram: &Histogram{
									DataPoints: []HistogramDataPoint{{
										StartTimeUnixNano: startNano,
										TimeUnixNano:      timeNano,
										Count:             count,
										Sum:               &sum,
										BucketCounts:      []string{count},
										Min:               &minV,
										Max:               &maxV,
									}},
									AggregationTemporality: TemporalityDelta,
								},
							},
							{
								Name: "http.requests",
								Unit: "1",
								Sum: &Sum{
									DataPoints: []NumberDataPoint{{
										Attributes:        AttrsFromStrings(map[string]string{"method": "GET"}),
										StartTimeUnixNano: startNano,
										TimeUnixNano:      timeNano,
										AsInt:             &asInt,
									}},
									AggregationTemporality: TemporalityDelta,
									IsMonotonic:            true,
								},
							},
							{
								Name: "queue.size",
								Gauge: &Gauge{
									DataPoints: []NumberDataPoint{{
										TimeUnixNano: timeNano,
										AsDouble:     &gaugeV,
									}},
								},
							},
						},
					},
				},
			},
		},
	}
}

func goldenTracesInput() TracesRequest {
	statusOK := &SpanStatus{Code: StatusOK}
	statusErr := &SpanStatus{Code: StatusError, Message: "deadline exceeded"}
	return TracesRequest{
		ResourceSpans: []ResourceSpans{
			{
				Resource: ResourceFromStrings(map[string]string{
					AttrPutnamiFramework: "go",
					AttrServiceName:      "checkout",
				}),
				ScopeSpans: []ScopeSpans{
					{
						Scope: Scope{Name: "putnami", Version: "1.0.0"},
						Spans: []Span{
							{
								TraceID:           "5b8efff798038103d269b633813fc60c",
								SpanID:            "eee19b7ec3c1b174",
								Name:              "GET /checkout",
								Kind:              SpanKindServer,
								StartTimeUnixNano: FormatUint(1700000000000000000),
								EndTimeUnixNano:   FormatUint(1700000000125000000),
								Attributes: SortAttrs([]KeyValue{
									Attr("http.method", StringVal("GET")),
									Attr("http.status_code", IntVal(200)),
								}),
								Status: statusOK,
							},
							{
								TraceID:           "5b8efff798038103d269b633813fc60c",
								SpanID:            "f0e1d2c3b4a59687",
								ParentSpanID:      "eee19b7ec3c1b174",
								Name:              "SELECT carts",
								Kind:              SpanKindClient,
								StartTimeUnixNano: FormatUint(1700000000010000000),
								EndTimeUnixNano:   FormatUint(1700000000040000000),
								Status:            statusErr,
							},
						},
					},
				},
			},
		},
	}
}

func goldenLogsInput() LogsRequest {
	infoBody := StringVal("request handled")
	errBody := StringVal("payment failed")
	return LogsRequest{
		ResourceLogs: []ResourceLogs{
			{
				Resource: ResourceFromStrings(map[string]string{
					AttrPutnamiFramework: "go",
					AttrServiceName:      "checkout",
				}),
				ScopeLogs: []ScopeLogs{
					{
						Scope: Scope{Name: "putnami", Version: "1.0.0"},
						LogRecords: []LogRecord{
							{
								TimeUnixNano:         FormatUint(1700000000000000000),
								ObservedTimeUnixNano: FormatUint(1700000000000000000),
								SeverityNumber:       SeverityInfo,
								SeverityText:         "info",
								Body:                 &infoBody,
								Attributes:           AttrsFromStrings(map[string]string{"route": "/checkout"}),
							},
							{
								TimeUnixNano:   FormatUint(1700000000500000000),
								SeverityNumber: SeverityError,
								SeverityText:   "error",
								Body:           &errBody,
								Attributes:     AttrsFromStrings(map[string]string{"error.code": "card_declined"}),
								TraceID:        "5b8efff798038103d269b633813fc60c",
								SpanID:         "eee19b7ec3c1b174",
							},
						},
					},
				},
			},
		},
	}
}

func TestCrossLanguage_Metrics(t *testing.T) {
	checkGolden(t, "metrics.golden.json", goldenMetricsDigest, goldenMetricsInput())
}

func TestCrossLanguage_Traces(t *testing.T) {
	checkGolden(t, "traces.golden.json", goldenTracesDigest, goldenTracesInput())
}

func TestCrossLanguage_Logs(t *testing.T) {
	checkGolden(t, "logs.golden.json", goldenLogsDigest, goldenLogsInput())
}

// checkGolden marshals input canonically, compares against the committed golden
// bytes, and asserts the digest matches the pinned constant. With
// UPDATE_GOLDEN=1 it (re)writes the golden file and prints the digest to copy
// into the constants above.
func checkGolden(t *testing.T, name, wantDigest string, input any) {
	t.Helper()
	got, err := MarshalCanonical(input)
	if err != nil {
		t.Fatalf("MarshalCanonical: %v", err)
	}
	path := filepath.Join("fixtures", "equivalence", name)

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s digest=%s", name, Digest(got))
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run UPDATE_GOLDEN=1 go test to generate)", name, err)
	}
	if string(got) != string(want) {
		t.Errorf("canonical bytes for %s differ from golden.\n got: %s\nwant: %s", name, got, want)
	}
	if d := Digest(got); d != wantDigest {
		t.Errorf("digest for %s = %q, want %q", name, d, wantDigest)
	}
}
