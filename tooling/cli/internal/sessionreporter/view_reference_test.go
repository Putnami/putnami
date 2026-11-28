package sessionreporter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/cli/model/jobs"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/output"
	"go.putnami.dev/tooling/cli/internal/sessionrefs"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
	"go.putnami.dev/tooling/cli/internal/store"
)

var (
	referenceKey    = strings.Repeat("a", 64)
	referenceDigest = "sha256:" + strings.Repeat("c", 64)
)

// referenceEntries is a store holding the entries a test names, by key.
type referenceEntries map[string]*store.TaskEntry

func (e referenceEntries) ReadReferencedResult(key, result string) *store.EntryResult {
	if entry := e[key]; entry != nil && entry.ResultDigest == result {
		return entry.Result
	}
	return nil
}

// referenceResult is the result of the entry of referenceStore: one
// diagnostic event and two test cases.
func referenceResult() *store.EntryResult {
	var result store.EntryResult
	if err := json.Unmarshal([]byte(`{"status":"success",`+
		`"data":{"testCases":[{"name":"TestA","suite":"a","status":"passed","durationMs":4},{"name":"TestB","suite":"a","status":"failed","durationMs":2,"output":"`+strings.Repeat("x", 300)+`"}]},`+
		`"events":[{"v":2,"type":"diagnostic","data":{"type":"diagnostic","severity":"warning","message":"deprecated call"}}]}`), &result); err != nil {
		panic(err)
	}
	return &result
}

// referencedEnd is the task:end of key that names the entry of
// referenceStore, recorded by this CLI's projection.
func referencedEnd(key string) string {
	return referencedEndOf(key, output.CachedRecordsProjection)
}

// referencedEndOf is the task:end of key that names the entry of
// referenceStore, the reference naming projection, and the digest this CLI's
// projection rebuilds.
func referencedEndOf(key string, projection int) string {
	end := `{"protocolVersion":2,"record":"task:end","time":"2026-11-27T10:00:02Z","identity":{"key":"` + key +
		`","task":{"name":"` + taskName(key) + `"}},"task":{"status":"success","reuse":"local-cache"}}`
	result := cachekey.JobResultFromEntryResult(referenceResult())
	cases := jobs.TaskResultOf(nil, result).TestCases
	records, ok := output.CachedRecords([]byte(end), result.Events, cases)
	if !ok {
		panic("the task:end rebuilds no records")
	}
	return strings.TrimSuffix(end, "}") + fmt.Sprintf(`,"cacheReference":{"entry":"sha256:%s","result":"%s","events":%d,"testCases":%d,"projection":%d,"records":"%s"}}`,
		referenceKey, referenceDigest, len(result.Events), len(cases), projection, output.CachedRecordsDigest(records))
}

func referenceStore(t *testing.T) referenceEntries {
	t.Helper()
	return referenceEntries{referenceKey: {Key: referenceKey, ResultDigest: referenceDigest, Result: referenceResult()}}
}

// resolvedBytes is what a reader sends in place of line and its LF.
func resolvedBytes(t *testing.T, entries sessionrefs.Entries, line string) []byte {
	t.Helper()
	data, err := sessionrefs.Resolved(entries, []byte(line))
	if err != nil {
		t.Fatalf("resolve %s: %v", line, err)
	}
	return data
}

// referenceView is the view of the final stream data, cut from cursor, with
// the records a task:end references rebuilt from entries when entries is set.
func referenceView(t *testing.T, data []byte, cursor int64, entries sessionrefs.Entries) ([]byte, *view) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, sessionstream.EventsFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	log, err := sessionstream.Open(dir, batchSessionID)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := log.Subscribe("view-test", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	v := newView(sub, cursor)
	if entries != nil {
		v.resolve = func(line []byte) ([]byte, error) { return sessionrefs.Resolved(entries, line) }
	}
	if err := v.fill(math.MaxInt64); err != nil || !v.done {
		t.Fatalf("the view of a final stream is not done: %v", err)
	}
	out := []byte{}
	for cursor+int64(len(out)) < v.end {
		chunk, err := v.read(cursor+int64(len(out)), 512)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, chunk...)
	}
	return out, v
}

// TestTheViewSendsTheRecordsACacheEntryHolds cuts the view of a stream that
// records a reused task by reference: the receiver reads the records the entry
// holds and the task:end without its reference, in place of that task:end, the
// other records around them as they are. A view rebuilt from a cursor inside
// the rebuilt bytes reads the same bytes on, and acknowledges no raw byte past
// the task:end before the receiver acknowledged all of them.
func TestTheViewSendsTheRecordsACacheEntryHolds(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "the-session-reporter-sends-the-rebuilt-records")
	entries := referenceStore(t)
	a, b := "/a:test", "/b:build"
	before := []string{startRecord(a), startRecord(b), logRecord(b, "info", "b works", 64)}
	after := []string{endRecord(b, protocolcli.TaskStatusSuccess), sessionEndRecord}
	data := stream(append(append(append([]string{}, before...), referencedEnd(a)), after...)...)
	rebuilt := resolvedBytes(t, entries, referencedEnd(a))
	if bytes.Count(rebuilt, []byte{'\n'}) != 4 || bytes.Contains(rebuilt, []byte("cacheReference")) {
		t.Fatalf("the reference rebuilt as\n%s\nwant one event, two cases and the task:end without its reference", rebuilt)
	}
	want := append(append(stream(before...), rebuilt...), stream(after...)...)

	got, v := referenceView(t, data, 0, entries)
	if !bytes.Equal(got, want) {
		t.Fatalf("view =\n%s\nwant\n%s", got, want)
	}
	if raw := v.acknowledged(v.end); raw != int64(len(data)) {
		t.Fatalf("the done view acknowledges raw offset %d, want the stream end %d", raw, len(data))
	}

	referenceAt := int64(len(stream(before...)))
	cursor := referenceAt + int64(len(rebuilt))/2
	rest, resumed := referenceView(t, data, cursor, entries)
	if !bytes.Equal(rest, want[cursor:]) {
		t.Fatalf("a view resumed inside the rebuilt bytes reads\n%s\nwant\n%s", rest, want[cursor:])
	}
	if raw := resumed.acknowledged(cursor); raw > referenceAt {
		t.Fatalf("a cursor inside the rebuilt bytes acknowledges raw offset %d, past the task:end at %d", raw, referenceAt)
	}

	plain, _ := referenceView(t, data, 0, nil)
	if !bytes.Equal(plain, data) {
		t.Fatal("a view that rebuilds nothing changed the stream")
	}
}

// newReferenceHarness is newCapabilityHarness for a new checkpoint of
// capability on a session that records by reference, its view rebuilding
// referenced records from entries.
func newReferenceHarness(t *testing.T, capability Capability, entries sessionrefs.Entries) *batchHarness {
	t.Helper()
	dir := t.TempDir()
	log, err := sessionstream.Create(dir, batchSessionID)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := log.Subscribe(capability.Name, 0)
	if err != nil {
		t.Fatal(err)
	}
	p, remote := pipeProcess()
	ctx, cancel := context.WithCancel(context.Background())
	h := &batchHarness{t: t, log: log, done: make(chan error, 1)}
	h.clock = &fakeClock{now: time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC), wake: sub.Wake(), armed: make(chan armedWait, 64)}
	h.run = &Run{
		capability: capability,
		dir:        dir, events: sub, process: p, ctx: ctx, graphCtx: context.Background(), cancel: cancel,
		finish: make(chan struct{}), done: make(chan struct{}), opTimeout: time.Minute,
		clock: h.clock, batchInterval: eventsBatchInterval(context.Background(), capability),
		state:   state{Version: capability.checkpointVersionOf(true), Provider: "@test/provider", SessionID: batchSessionID},
		entries: entries,
	}
	go func() {
		scanner := bufio.NewScanner(remote)
		scanner.Buffer(make([]byte, 4096), protocolcli.SessionReportingLineBytes)
		for scanner.Scan() {
			chunk, err := protocolcli.ParseSessionReportingChunk(scanner.Bytes())
			if err != nil {
				return
			}
			h.mu.Lock()
			h.chunks = append(h.chunks, *chunk)
			h.mu.Unlock()
			_ = json.NewEncoder(remote).Encode(chunk.Ack())
		}
	}()
	go func() { h.done <- h.run.deliver() }()
	t.Cleanup(func() {
		cancel()
		_ = remote.Close()
		_ = log.Close()
		_ = sub.Close()
	})
	h.settle()
	return h
}

// TestTheSessionReporterDeliversTheRecordsACacheEntryHolds runs the delivery
// loop of a new checkpoint of a session that records by reference, version 4,
// over a stream that records a reused task by reference: the receiver reads
// the records a recorder that kept every record would have written, rebuilt
// in place of the reference. A version 2 checkpoint's view rebuilds nothing.
func TestTheSessionReporterDeliversTheRecordsACacheEntryHolds(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "the-session-reporter-sends-the-rebuilt-records")
	entries := referenceStore(t)
	a := "/a:test"
	h := newReferenceHarness(t, SessionReporter, entries)
	if h.run.state.Version != resolvedViewCheckpointVersion {
		t.Fatalf("a new checkpoint is version %d, want %d", h.run.state.Version, resolvedViewCheckpointVersion)
	}
	records := []string{startRecord(a), referencedEnd(a), sessionEndRecord}
	for _, record := range records {
		h.append(record)
	}
	if err := h.log.Close(); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if err := os.WriteFile(filepath.Join(h.run.dir, sessionFile), []byte(`{"protocolVersion":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	close(h.run.finish)
	if err := <-h.done; err != nil {
		t.Fatalf("terminal drain: %v", err)
	}
	want := append(append(stream(records[0]), resolvedBytes(t, entries, records[1])...), stream(records[2])...)
	got, final := reassemble(t, h.sent(), sessionstream.EventsFile)
	if !final || !bytes.Equal(got, want) {
		t.Fatalf("the receiver read (final %v)\n%s\nwant\n%s", final, got, want)
	}
	if h.run.state.EventsRaw != int64(len(stream(records...))) || h.run.state.Events.Offset != int64(len(want)) {
		t.Fatalf("checkpoint acknowledges raw offset %d at view offset %d, want %d at %d",
			h.run.state.EventsRaw, h.run.state.Events.Offset, len(stream(records...)), len(want))
	}

	old := &Run{capability: SessionReporter, state: state{Version: viewCheckpointVersion}, entries: entries}
	if old.eventsView().resolve != nil {
		t.Fatal("a version 2 view rebuilds referenced records")
	}
}
