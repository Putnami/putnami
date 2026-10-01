package logtest

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"go.putnami.dev/logger"
)

// The manifest lives four levels up: go/framework/logger/logtest → repo root.
const manifestPath = "../../../../protocols/logging/conformance/manifest.json"

// The tests below pin the harness's OWN semantics — tokens, $open, closed
// objects, the sorted-key 2-space canonical form — because every boundary test in
// both runtimes trusts them. They are the Go twin of
// typescript/framework/runtime/test/testing/log-conformance.test.ts; the two
// implementations must accept and reject exactly the same records.

func caseOf(t *testing.T, record map[string]any) Case {
	t.Helper()
	return Case{ID: "harness.case", Level: "required", Boundary: "http", Record: record}
}

func mustJSON(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestLoadCasesReadsTheCanonicalCorpus(t *testing.T) {
	suite := LoadCases(t, manifestPath, "http")
	if len(suite.Cases) != 3 {
		t.Fatalf("http boundary cases = %d (%v), want the corpus's 3", len(suite.Cases), suite.IDs())
	}
	c := suite.Case(t, "http.terminal.success")
	if c.Logger() != "http" || c.Message() != "[GET] /conformance/orders" {
		t.Errorf("case identity = %q / %q", c.Logger(), c.Message())
	}
	// Every boundary of the corpus must be loadable: a boundary typo in a test
	// would otherwise silently assert nothing.
	for _, boundary := range []string{"http", "event", "database", "migration"} {
		if got := LoadCases(t, manifestPath, boundary); len(got.Cases) == 0 {
			t.Errorf("boundary %q has no cases", boundary)
		}
	}
}

func TestCompareRecordAcceptsTokenLeaves(t *testing.T) {
	want := caseOf(t, map[string]any{
		"severity":  "INFO",
		"message":   "ok",
		"timestamp": TokenTimestamp,
		"traceId":   TokenString,
		"http":      map[string]any{"durationMs": TokenNumber},
	})
	line := mustJSON(t, map[string]any{
		"severity":  "INFO",
		"message":   "ok",
		"timestamp": "2026-07-25T10:11:12.345Z",
		"traceId":   "req-1",
		"http":      map[string]any{"durationMs": 7},
	})
	if ok, detail := CompareRecord(line, want); !ok {
		t.Fatalf("token leaves must match by type:\n%s", detail)
	}
}

func TestCompareRecordRejectsTokenTypeMismatch(t *testing.T) {
	want := caseOf(t, map[string]any{"http": map[string]any{"durationMs": TokenNumber}})

	// A duration emitted as a STRING is exactly the drift the token guards
	// against ("durationMs": "7ms" would satisfy a looser check).
	line := mustJSON(t, map[string]any{"http": map[string]any{"durationMs": "7"}})
	ok, detail := CompareRecord(line, want)
	if ok {
		t.Fatal("a string in a <number> slot must fail")
	}
	if !strings.Contains(detail, TokenNumber) {
		t.Errorf("failure detail must show the token, got:\n%s", detail)
	}

	// A non-UTC / non-millisecond timestamp must fail the pattern.
	tsWant := caseOf(t, map[string]any{"timestamp": TokenTimestamp})
	for _, bad := range []string{"2026-07-25T10:11:12Z", "2026-07-25T10:11:12.345678Z", "2026-07-25T10:11:12.345+02:00"} {
		if ok, _ := CompareRecord(mustJSON(t, map[string]any{"timestamp": bad}), tsWant); ok {
			t.Errorf("timestamp %q must fail the <iso8601-millis-utc> pattern", bad)
		}
	}
	// An empty string is not a <string>: an absent-but-present field is drift too.
	if ok, _ := CompareRecord(mustJSON(t, map[string]any{"traceId": ""}), caseOf(t, map[string]any{"traceId": TokenString})); ok {
		t.Error("an empty string must fail the <string> token")
	}
}

func TestCompareRecordRejectsUnexpectedKeys(t *testing.T) {
	want := caseOf(t, map[string]any{
		"message": "ok",
		"http":    map[string]any{"status": float64(200)},
	})

	// Drift by ADDITION: a new top-level key.
	line := mustJSON(t, map[string]any{
		"message":   "ok",
		"http":      map[string]any{"status": 200},
		"requestId": "leaked",
	})
	ok, detail := CompareRecord(line, want)
	if ok {
		t.Fatal("an unexpected top-level key must fail")
	}
	if !strings.Contains(detail, "requestId") {
		t.Errorf("failure detail must name the unexpected key, got:\n%s", detail)
	}

	// Drift by addition INSIDE a closed group.
	line = mustJSON(t, map[string]any{
		"message": "ok",
		"http":    map[string]any{"status": 200, "duration": 7},
	})
	if ok, detail := CompareRecord(line, want); ok {
		t.Fatalf("an unexpected key in a closed group must fail:\n%s", detail)
	}

	// …and a missing expected key fails too.
	if ok, _ := CompareRecord(mustJSON(t, map[string]any{"message": "ok"}), want); ok {
		t.Error("a missing expected group must fail")
	}
}

func TestCompareRecordStripsExtrasOnlyUnderOpen(t *testing.T) {
	want := caseOf(t, map[string]any{
		"error": map[string]any{openMarker: true, "name": TokenString, "message": "boom"},
	})
	line := mustJSON(t, map[string]any{
		// Go's ErrorInfo legitimately adds code/category/retryable/stack; the
		// TypeScript twin adds cause. $open is what keeps both honest without
		// pinning runtime-specific extras.
		"error": map[string]any{"name": "Error", "message": "boom", "stack": "…", "code": "x", "retryable": true},
	})
	if ok, detail := CompareRecord(line, want); !ok {
		t.Fatalf("$open must strip runtime-specific extras:\n%s", detail)
	}
	// $open still pins the keys it declares.
	if ok, _ := CompareRecord(mustJSON(t, map[string]any{"error": map[string]any{"name": "Error", "message": "other"}}), want); ok {
		t.Error("$open must still compare the keys it declares")
	}
	// The marker never leaks into the canonical text.
	_, detail := CompareRecord(mustJSON(t, map[string]any{"error": "boom"}), want)
	if strings.Contains(detail, openMarker) {
		t.Errorf("the $open marker must not reach the canonical form:\n%s", detail)
	}
}

func TestCompareRecordComparesArraysElementWise(t *testing.T) {
	want := caseOf(t, map[string]any{
		"event": map[string]any{
			"publishCount": float64(2),
			"publishes": []any{
				map[string]any{"topic": "t", "messageId": TokenString},
				map[string]any{"topic": "t", "messageId": TokenString},
			},
		},
	})
	line := mustJSON(t, map[string]any{
		"event": map[string]any{
			"publishCount": 2,
			"publishes": []any{
				map[string]any{"topic": "t", "messageId": "m-1"},
				map[string]any{"topic": "t", "messageId": "m-2"},
			},
		},
	})
	if ok, detail := CompareRecord(line, want); !ok {
		t.Fatalf("appended list must match element-wise:\n%s", detail)
	}
	// A dropped publish (accumulation regression) must fail.
	short := mustJSON(t, map[string]any{
		"event": map[string]any{"publishCount": 2, "publishes": []any{map[string]any{"topic": "t", "messageId": "m-1"}}},
	})
	if ok, _ := CompareRecord(short, want); ok {
		t.Error("a missing appended element must fail")
	}
}

func TestFailureDetailNamesTheCorpusAndBothRuntimes(t *testing.T) {
	want := Case{ID: "event.terminal.dlq", Boundary: "event", Record: map[string]any{"message": "message dead-lettered"}}
	_, detail := CompareRecord(mustJSON(t, map[string]any{"message": "message handled"}), want)
	for _, needle := range []string{
		"protocols/logging/conformance/manifest.json",
		"go/framework/events/logging_cross_language_test.go",
		"typescript/framework/events/test/logging-cross-language.test.ts",
	} {
		if !strings.Contains(detail, needle) {
			t.Errorf("failure detail must name %q, got:\n%s", needle, detail)
		}
	}
}

// The recorder must capture what the REAL JSONSink renders — including the
// reserved-key protection and the plain traceId key — and select a case's record
// by its pinned (logger, message) pair.
func TestRecorderCapturesRealSinkOutput(t *testing.T) {
	rec := NewRecorder(t)
	log := rec.Named("http")

	ctx := logger.ContextWithTraceID(context.Background(), "trace-1")
	log.InfoCtx(ctx, "[GET] /conformance/orders", slog.Any("http", map[string]any{"status": 200}))

	want := Case{ID: "http.terminal.success", Boundary: "http", Record: map[string]any{
		"severity":  "INFO",
		"message":   "[GET] /conformance/orders",
		"timestamp": TokenTimestamp,
		"logger":    "http",
		"traceId":   TokenString,
		"http":      map[string]any{"status": float64(200)},
	}}
	AssertRecord(t, rec.Record(t, want), want)

	// Reset clears the capture so a second case in the same test cannot match the
	// first case's record.
	rec.Reset()
	if got := rec.Lines(); len(got) != 0 {
		t.Errorf("Reset left %d lines", len(got))
	}
}

func TestRecorderRecordRequiresExactlyOne(t *testing.T) {
	rec := NewRecorder(t)
	log := rec.Named("events.handler")
	log.Info("message handled")
	log.Info("message handled")

	if got := len(rec.Records("events.handler", "message handled")); got != 2 {
		t.Fatalf("Records = %d, want 2", got)
	}
	if !strings.Contains(rec.Dump(), "events.handler message handled") {
		t.Errorf("Dump must list the captured records, got %s", rec.Dump())
	}
}
