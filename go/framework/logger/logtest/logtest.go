// Package logtest is the Go half of the cross-runtime logging conformance
// harness: it makes the canonical corpus in protocols/logging/conformance
// EXECUTABLE against the records the framework really emits.
//
// A boundary test loads the corpus cases for its boundary, drives REAL boundary
// code (middleware chain / broker dispatch / pool) with its records captured
// through the REAL JSONSink, and asserts each captured line against its case:
//
//	suite := logtest.LoadCases(t, manifestPath, "http")
//	rec := logtest.NewRecorder(t)
//	// … drive real code with rec.Named("http") …
//	want := suite.Case(t, "http.terminal.success")
//	logtest.AssertRecord(t, rec.Record(t, want), want)
//
// Capturing through logger.NewJSONSinkWriter (rather than a memory sink) is
// deliberate: slogValueToAny group rendering, reserved-key protection, the
// trace-key choice, and the timestamp format are all part of the contract, so
// they must be under test too.
//
// The comparison semantics (tokens, $open objects, sorted-key 2-space
// re-serialization, byte compare) are normative in
// protocols/logging/conformance/README.md and are implemented identically by the
// TypeScript half (@putnami/runtime/testing's log-conformance).
//
// This package deliberately imports "testing" from non-test source: it exists to
// be called by other packages' test binaries, exactly like
// go.putnami.dev/database/conformance.
package logtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/logger"
)

// Corpus tokens. A leaf whose EXPECTED value is one of these is type/pattern
// checked and then replaced by the token text on both sides, so a value that
// legitimately varies per run (a timestamp, a duration, a generated id) is
// pinned by type instead of by value.
// The names carry the corpus's own vocabulary ("tokens"), which trips gosec's
// credential-name heuristic; these are golden-fixture placeholders, not secrets.
const (
	TokenTimestamp = "<iso8601-millis-utc>"
	TokenNumber    = "<number>"
	TokenString    = "<string>"
)

// openMarker flags an expected object that tolerates extra keys in the actual
// record; the extras are stripped before the byte compare and the marker itself
// never reaches the canonical form.
const openMarker = "$open"

// timestampPattern is the corpus's `<iso8601-millis-utc>` contract: UTC, exactly
// three fractional digits, `Z` suffix. Both runtimes format timestamps this way,
// so a runtime that switches to nanoseconds or a local offset fails here.
var timestampPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)

// Case is one corpus case: the expected record plus the metadata a failure
// message needs.
type Case struct {
	ID       string         `json:"id"`
	Level    string         `json:"level"`
	Boundary string         `json:"boundary"`
	Summary  string         `json:"summary"`
	Record   map[string]any `json:"record"`
}

// Logger returns the pinned logger name of the case's record ("" when the case
// pins none).
func (c Case) Logger() string { return stringField(c.Record, "logger") }

// Message returns the pinned stable message of the case's record.
func (c Case) Message() string { return stringField(c.Record, "message") }

func stringField(record map[string]any, key string) string {
	if s, ok := record[key].(string); ok {
		return s
	}
	return ""
}

// Suite is the set of corpus cases for one boundary, plus where they came from.
type Suite struct {
	// ManifestPath is the path the cases were loaded from, quoted in failures.
	ManifestPath string
	// Boundary is the boundary the suite was filtered to.
	Boundary string
	// Cases are the boundary's cases in manifest order.
	Cases []Case
}

type manifestFile struct {
	Protocol        string            `json:"protocol"`
	Suite           string            `json:"suite"`
	ProtocolVersion int               `json:"protocolVersion"`
	Tokens          map[string]string `json:"tokens"`
	Cases           []Case            `json:"cases"`
}

// LoadCases reads the conformance manifest at manifestPath and returns the cases
// whose boundary matches. It fails the test when the manifest is missing,
// malformed, or contains no case for the boundary — a silently empty suite would
// make a boundary test pass while asserting nothing.
func LoadCases(t *testing.T, manifestPath, boundary string) Suite {
	t.Helper()

	raw, err := os.ReadFile(manifestPath) //nolint:gosec // test-only read of a committed corpus path
	if err != nil {
		t.Fatalf("read logging conformance manifest %q: %v", manifestPath, err)
	}
	var file manifestFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parse logging conformance manifest %q: %v", manifestPath, err)
	}
	if file.Protocol != "putnami.logging.v1" {
		t.Fatalf("manifest %q declares protocol %q, want putnami.logging.v1", manifestPath, file.Protocol)
	}

	suite := Suite{ManifestPath: manifestPath, Boundary: boundary}
	for _, c := range file.Cases {
		if c.Boundary == boundary {
			suite.Cases = append(suite.Cases, c)
		}
	}
	if len(suite.Cases) == 0 {
		t.Fatalf("logging conformance manifest %q has no case for boundary %q", manifestPath, boundary)
	}
	return suite
}

// Case returns the case with the given id, failing when the boundary has none —
// so a renamed or deleted corpus case breaks the boundary test instead of
// silently disabling it.
func (s Suite) Case(t *testing.T, id string) Case {
	t.Helper()
	for _, c := range s.Cases {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("logging conformance case %q not found for boundary %q in %s (have: %s)",
		id, s.Boundary, s.ManifestPath, strings.Join(s.IDs(), ", "))
	return Case{}
}

// IDs returns the case ids of the suite in manifest order.
func (s Suite) IDs() []string {
	out := make([]string, 0, len(s.Cases))
	for _, c := range s.Cases {
		out = append(out, c.ID)
	}
	return out
}

// syncWriter serializes writes and reads of the captured output. JSONSink holds
// its own mutex per write, but a boundary test reads the buffer while transport
// goroutines may still be writing, so the buffer needs a lock of its own.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) snapshot() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func (w *syncWriter) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Reset()
}

// Recorder captures the JSON lines the framework's REAL JSONSink renders, so a
// boundary test asserts the bytes a log aggregator would receive.
type Recorder struct {
	out  *syncWriter
	root *logger.Logger
}

// NewRecorder returns a Recorder whose root logger is unnamed and at debug level
// (so DEBUG records such as "query executed" are captured, and a derived name is
// exactly the contract's pinned name).
//
// It clears GOOGLE_CLOUD_PROJECT / GCP_PROJECT for the duration of the test
// BEFORE constructing the sink: JSONSink resolves the project once at
// construction and, when one is set, renames the trace key to
// logging.googleapis.com/trace. The corpus pins the plain traceId, so the
// harness must be hermetic against a developer's or CI's cloud environment.
func NewRecorder(t *testing.T) *Recorder {
	t.Helper()
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("GCP_PROJECT", "")

	out := &syncWriter{}
	return &Recorder{out: out, root: logger.New("", logger.LevelDebug, logger.NewJSONSinkWriter(out))}
}

// Root returns the unnamed root logger. Derive the boundary's pinned name from
// it with Named (or hand it to a package's own eventLoggersFrom-style helper).
func (r *Recorder) Root() *logger.Logger { return r.root }

// Named returns the root logger renamed to name, which — because the root is
// unnamed — is exactly the contract's pinned logger name.
func (r *Recorder) Named(name string) *logger.Logger { return r.root.Named(name) }

// Lines returns the captured JSON lines in emission order.
func (r *Recorder) Lines() []string {
	text := strings.TrimSpace(r.out.snapshot())
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// Reset drops every captured line, so one test can drive several cases through
// the same recorder without their records colliding.
func (r *Recorder) Reset() { r.out.reset() }

// Records returns the captured lines emitted by loggerName with the given
// message — the (logger, message) pair the corpus pins for each case.
func (r *Recorder) Records(loggerName, message string) []string {
	var out []string
	for _, line := range r.Lines() {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if stringField(rec, "logger") == loggerName && stringField(rec, "message") == message {
			out = append(out, line)
		}
	}
	return out
}

// Record returns the single captured line matching want's pinned logger and
// message. Exactly one is required: the contract's terminal-record policy is
// "exactly one terminal record per boundary", so a duplicate is a contract
// violation, not a test inconvenience.
func (r *Recorder) Record(t *testing.T, want Case) string {
	t.Helper()
	found := r.Records(want.Logger(), want.Message())
	if len(found) != 1 {
		t.Fatalf("logging conformance case %q: expected exactly 1 %q record from logger %q, got %d.%s",
			want.ID, want.Message(), want.Logger(), len(found), r.Dump())
	}
	return found[0]
}

// Dump renders every captured record as "[severity] logger message" for failure
// messages.
func (r *Recorder) Dump() string {
	lines := r.Lines()
	if len(lines) == 0 {
		return "\n  (no records captured)"
	}
	var b strings.Builder
	for _, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			b.WriteString("\n  (unparseable) " + line)
			continue
		}
		b.WriteString("\n  [" + stringField(rec, "severity") + "] " +
			stringField(rec, "logger") + " " + stringField(rec, "message"))
	}
	return b.String()
}

// AssertRecord asserts that gotLine — a real rendered JSON log line — matches
// want after the corpus canonicalization. On failure it prints both canonical
// forms and names the three places a contract change must touch together.
func AssertRecord(t *testing.T, gotLine string, want Case) {
	t.Helper()
	ok, detail := CompareRecord(gotLine, want)
	if !ok {
		t.Errorf("%s", detail)
	}
}

// CompareRecord reports whether gotLine matches want after canonicalization,
// returning the failure detail otherwise. It is the pure core of AssertRecord,
// exported so the harness's own semantics are unit-testable.
func CompareRecord(gotLine string, want Case) (bool, string) {
	var got any
	if err := json.Unmarshal([]byte(gotLine), &got); err != nil {
		return false, fmt.Sprintf("logging conformance case %q: captured line is not JSON: %v\nline: %s",
			want.ID, err, gotLine)
	}

	wantCanon, gotCanon := canonicalize(want.Record, got)
	wantText, err := marshalCanonical(wantCanon)
	if err != nil {
		return false, fmt.Sprintf("logging conformance case %q: cannot serialize the expected record: %v", want.ID, err)
	}
	gotText, err := marshalCanonical(gotCanon)
	if err != nil {
		return false, fmt.Sprintf("logging conformance case %q: cannot serialize the captured record: %v", want.ID, err)
	}
	if wantText == gotText {
		return true, ""
	}
	return false, failureDetail(want, gotText, wantText)
}

// failureDetail is the shared failure text of both runtimes' harnesses: it names
// the corpus AND the twin runtime's test, because a divergence here means either
// the change missed the other runtime or it missed the corpus.
func failureDetail(want Case, gotText, wantText string) string {
	return fmt.Sprintf(`logging conformance case %q (boundary %q) does not match the corpus.
got (canonical):
%s
want (canonical):
%s
This record is a public contract (dashboards, alerts, log-based metrics). Either the
emitter drifted, or the change is intentional — in which case update the fixture AND
the other runtime's test in the same commit:
  protocols/logging/conformance/manifest.json   (the normative corpus)
  %s
  %s`, want.ID, want.Boundary, gotText, wantText, goTwin(want.Boundary), tsTwin(want.Boundary))
}

// goTwin / tsTwin name the boundary's harness callers so a failure in one
// runtime points at the file that must change in the other.
func goTwin(boundary string) string {
	switch boundary {
	case "http":
		return "go/framework/http/logging_cross_language_test.go"
	case "event":
		return "go/framework/events/logging_cross_language_test.go"
	default: // database + migration records are both emitted by go.putnami.dev/database
		return "go/framework/database/logging_cross_language_test.go"
	}
}

func tsTwin(boundary string) string {
	switch boundary {
	case "http":
		return "typescript/framework/application/test/http/logging-cross-language.test.ts"
	case "event":
		return "typescript/framework/events/test/logging-cross-language.test.ts"
	default:
		return "typescript/framework/database/test/logging-cross-language.test.ts"
	}
}

// canonicalize walks the expected and actual trees together and returns the two
// canonical forms to byte-compare:
//
//   - a token leaf is type/pattern checked and, when it matches, replaced by the
//     token text on BOTH sides (so a varying value is pinned by type);
//   - an expected object carrying "$open": true keeps only the keys it declares
//     on the actual side (runtime-specific extras are legitimate there) and
//     drops the marker;
//   - every other object is closed: an unexpected key stays on the actual side
//     and therefore fails the compare, which is how the corpus catches drift by
//     ADDITION as well as by omission.
func canonicalize(want, got any) (any, any) {
	switch w := want.(type) {
	case string:
		if isToken(w) {
			if tokenMatches(w, got) {
				return w, w
			}
			// Keep the raw actual value so the diff shows what arrived instead.
			return w, got
		}
		return w, got
	case map[string]any:
		return canonicalizeMap(w, got)
	case []any:
		return canonicalizeSlice(w, got)
	default:
		return want, got
	}
}

func canonicalizeMap(want map[string]any, got any) (any, any) {
	gotMap, ok := got.(map[string]any)
	if !ok {
		// Shape mismatch (e.g. a group rendered as a string): let the compare
		// report it with the raw actual value.
		return stripMarkers(want), got
	}
	open := want[openMarker] == true

	wantOut := make(map[string]any, len(want))
	gotOut := make(map[string]any, len(gotMap))
	for k, wv := range want {
		if k == openMarker {
			continue
		}
		gv, present := gotMap[k]
		if !present {
			// Missing on the actual side: only the expected key is emitted, so the
			// diff shows it as absent.
			wantOut[k] = stripMarkers(wv)
			continue
		}
		wc, gc := canonicalize(wv, gv)
		wantOut[k] = wc
		gotOut[k] = gc
	}
	for k, gv := range gotMap {
		if _, expected := want[k]; expected {
			continue
		}
		if open {
			continue // runtime-specific extra: stripped by contract
		}
		gotOut[k] = gv // unexpected key: kept so the byte compare fails
	}
	return wantOut, gotOut
}

func canonicalizeSlice(want []any, got any) (any, any) {
	gotSlice, ok := got.([]any)
	if !ok {
		return stripMarkers(want), got
	}
	wantOut := make([]any, 0, len(want))
	gotOut := make([]any, 0, len(gotSlice))
	for i := range want {
		if i >= len(gotSlice) {
			wantOut = append(wantOut, stripMarkers(want[i]))
			continue
		}
		wc, gc := canonicalize(want[i], gotSlice[i])
		wantOut = append(wantOut, wc)
		gotOut = append(gotOut, gc)
	}
	// Extra actual elements are kept so the compare reports the length difference.
	for i := len(want); i < len(gotSlice); i++ {
		gotOut = append(gotOut, gotSlice[i])
	}
	return wantOut, gotOut
}

// stripMarkers removes $open markers from an expected subtree that has no actual
// counterpart, so the canonical want form never leaks the marker.
func stripMarkers(want any) any {
	switch w := want.(type) {
	case map[string]any:
		out := make(map[string]any, len(w))
		for k, v := range w {
			if k == openMarker {
				continue
			}
			out[k] = stripMarkers(v)
		}
		return out
	case []any:
		out := make([]any, 0, len(w))
		for _, v := range w {
			out = append(out, stripMarkers(v))
		}
		return out
	default:
		return want
	}
}

func isToken(s string) bool {
	return s == TokenTimestamp || s == TokenNumber || s == TokenString
}

func tokenMatches(token string, got any) bool {
	switch token {
	case TokenTimestamp:
		s, ok := got.(string)
		return ok && timestampPattern.MatchString(s)
	case TokenNumber:
		_, ok := got.(float64) // every JSON number decodes to float64
		return ok
	case TokenString:
		s, ok := got.(string)
		return ok && s != ""
	default:
		return false
	}
}

// marshalCanonical renders v with sorted keys and a 2-space indent — Go's
// encoding/json sorts map keys natively, which is exactly why the corpus picked
// that canonical form. HTML escaping is disabled so token text such as
// "<number>" stays readable and byte-identical to the TypeScript harness's
// output.
func marshalCanonical(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}
