package sessionreporter

import (
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

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
)

// The records of a view test, each one line of events.jsonl without its LF.

func taskName(key string) string {
	_, name, _ := strings.Cut(key, ":")
	return name
}

func startRecord(key string) string {
	return fmt.Sprintf(`{"protocolVersion":2,"record":"task:start","identity":{"key":%q,"task":{"name":%q}}}`, key, taskName(key))
}

func endRecord(key, status string) string {
	return fmt.Sprintf(`{"protocolVersion":2,"record":"task:end","identity":{"key":%q,"task":{"name":%q}},"task":{"status":%q}}`, key, taskName(key), status)
}

func eventRecord(key, event string) string {
	return fmt.Sprintf(`{"protocolVersion":2,"record":"task:event","identity":{"key":%q,"task":{"name":%q}},"event":%s}`, key, taskName(key), event)
}

// logRecord is a log line of key, its message padded to size bytes.
func logRecord(key, level, message string, size int) string {
	return eventRecord(key, fmt.Sprintf(`{"type":"log","level":%q,"message":%q}`, level, pad(message, size)))
}

func progressRecord(key string) string {
	return eventRecord(key, `{"type":"progress","current":1,"total":2,"message":"step"}`)
}

// batchRecord is a log line of the batch task batchKey that context
// attributes to its members.
func batchRecord(batchKey, level, message, context string) string {
	return eventRecord(batchKey, fmt.Sprintf(`{"type":"log","level":%q,"message":%q,"context":%s}`, level, message, context))
}

const sessionEndRecord = `{"protocolVersion":2,"record":"session:end"}`

func pad(message string, size int) string {
	if len(message) >= size {
		return message
	}
	return message + strings.Repeat(".", size-len(message))
}

// stream is records as events.jsonl holds them.
func stream(records ...string) []byte {
	var data bytes.Buffer
	for _, record := range records {
		data.WriteString(record)
		data.WriteByte('\n')
	}
	return data.Bytes()
}

// viewOf builds the view of a finished events.jsonl holding data and returns
// its bytes and the raw offset its end acknowledges.
func viewOf(t *testing.T, data []byte) ([]byte, int64) {
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
	defer func() { _ = sub.Close() }()
	v := newView(sub, 0)
	if err := v.fill(math.MaxInt64); err != nil || !v.done {
		t.Fatalf("the view of a final stream is not done: %v", err)
	}
	var out []byte
	for int64(len(out)) < v.end {
		chunk, err := v.read(int64(len(out)), protocolcli.SessionReportingChunkBytes)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, chunk...)
	}
	return out, v.acknowledged(v.end)
}

// assertView fails t unless the view of records is want.
func assertView(t *testing.T, records []string, want ...string) {
	t.Helper()
	data := stream(records...)
	got, raw := viewOf(t, data)
	if expected := stream(want...); !bytes.Equal(got, expected) {
		t.Fatalf("view =\n%s\nwant\n%s", got, expected)
	}
	if raw != int64(len(data)) {
		t.Fatalf("the done view acknowledges raw offset %d, want the stream end %d", raw, len(data))
	}
}

// finishedSession is a session directory whose final events.jsonl holds data,
// beside a finalized session.json.
func finishedSession(t *testing.T, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, sessionstream.EventsFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionFile), []byte(`{"protocolVersion":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// assertFrames fails t unless chunks, the events.jsonl chunks one or more
// receivers read in order, are frames of view: every copy of a sequence is
// the same frame, each frame holds the view's bytes at its offset, and the
// sequences chain from 0 to a final marker at the view's end.
func assertFrames(t *testing.T, view []byte, chunks []protocolcli.SessionReportingChunk) {
	t.Helper()
	frames := map[int64]protocolcli.SessionReportingChunk{}
	for _, chunk := range chunks {
		if chunk.Artifact != sessionstream.EventsFile {
			continue
		}
		if seen, ok := frames[chunk.Sequence]; ok && !sameFrame(t, seen, chunk) {
			t.Fatalf("sequence %d was sent with two contents", chunk.Sequence)
		}
		frames[chunk.Sequence] = chunk
		end := chunk.Offset + int64(len(chunk.Data))
		if end > int64(len(view)) || !bytes.Equal(chunk.Data, view[chunk.Offset:end]) {
			t.Fatalf("sequence %d at view offset %d is not the view's bytes", chunk.Sequence, chunk.Offset)
		}
	}
	offset := int64(0)
	for sequence := int64(0); ; sequence++ {
		chunk, ok := frames[sequence]
		if !ok || chunk.Offset != offset {
			t.Fatalf("sequence %d missing or at offset %d, want %d", sequence, chunk.Offset, offset)
		}
		offset += int64(len(chunk.Data))
		if chunk.Final {
			if offset != int64(len(view)) || len(frames) != int(sequence)+1 {
				t.Fatalf("final marker at %d after %d frames, want %d after %d", offset, sequence, len(view), len(frames)-1)
			}
			return
		}
	}
}

// TestTheViewDropsProgressAndThePassingTasksDebugLines runs the live delivery
// loop over a task whose debug lines leave raw frames before its task:end: no
// byte of them, and no progress event, reaches the receiver. Another task's
// info lines leave live while the first holds.
func TestTheViewDropsProgressAndThePassingTasksDebugLines(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "trimmed-upload", "progress-events-and-the-debug-lines-of-a-passing-task-are-not-sent")
	h := newBatchHarness(t)
	a, b := "/a:test", "/b:build"
	var records, want []string
	// Each append settles: a wait the worker armed while the test was still
	// appending would otherwise end a later settle early.
	add := func(record string, sent bool) {
		records = append(records, record)
		h.append(record)
		h.settle()
		if sent {
			want = append(want, record)
		}
	}
	add(startRecord(a), true)
	add(startRecord(b), true)
	for i := range 8 {
		add(logRecord(a, "debug", fmt.Sprintf("a debug %d", i), 1024), false)
		add(progressRecord(a), false)
	}
	lastDebug := len(stream(records...))
	for i := range 80 {
		add(logRecord(b, "info", fmt.Sprintf("b info %d", i), 1024), true)
		add(progressRecord(b), false)
	}
	end := len(stream(records...))
	add(endRecord(a, protocolcli.TaskStatusSuccess), true)
	add(endRecord(b, protocolcli.TaskStatusSkipped), true)
	add(sessionEndRecord, true)
	if end/protocolcli.SessionReportingChunkBytes == lastDebug/protocolcli.SessionReportingChunkBytes || end-lastDebug <= protocolcli.SessionReportingChunkBytes {
		t.Fatalf("the task:end at raw offset %d shares a raw frame with the last debug line, which ends at %d", end, lastDebug)
	}
	if got := h.sent(); len(got) == 0 {
		t.Fatal("a full frame of live info lines did not leave while the debug lines were held")
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
	got, final := reassemble(t, h.sent(), sessionstream.EventsFile)
	if !final || !bytes.Equal(got, stream(want...)) {
		t.Fatalf("the receiver read %d bytes (final %v), want the %d bytes without debug lines and progress", len(got), final, len(stream(want...)))
	}
	if bytes.Contains(got, []byte(`"level":"debug"`)) || bytes.Contains(got, []byte(`"type":"progress"`)) {
		t.Fatal("a debug line or a progress event reached the receiver")
	}
	if h.run.state.Version != viewCheckpointVersion || h.run.state.EventsRaw != int64(len(stream(records...))) {
		t.Fatalf("checkpoint version %d acknowledges raw offset %d, want version 2 at the stream end %d", h.run.state.Version, h.run.state.EventsRaw, len(stream(records...)))
	}
}

// TestTheViewBatchesOnTheBytesItSends holds a full raw frame of debug lines:
// nothing is ready, so the worker waits a whole interval instead of sending
// at once. The task fails, its lines are ready, and a full frame leaves at
// once.
func TestTheViewBatchesOnTheBytesItSends(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "trimmed-upload", "progress-events-and-the-debug-lines-of-a-passing-task-are-not-sent")
	h := newBatchHarness(t)
	a := "/a:test"
	for i := range 80 {
		h.append(logRecord(a, "debug", fmt.Sprintf("held %d", i), 1024))
		if wait := h.settle(); wait != EventsBatchInterval || len(h.sent()) != 0 {
			t.Fatalf("a held raw frame waits %s with %d chunks sent, want a whole %s interval and none", wait, len(h.sent()), EventsBatchInterval)
		}
	}
	h.append(endRecord(a, protocolcli.TaskStatusFailed))
	h.settle()
	got := h.sent()
	if len(got) != 1 || len(got[0].Data) != protocolcli.SessionReportingChunkBytes || got[0].Sequence != 0 {
		t.Fatalf("the released lines left as %d chunks, want one full frame at once", len(got))
	}
}

// TestAFailingTaskSendsItsHeldLinesInOrderBeforeItsEnd holds every event of a
// task from its first debug line (D-R1): they leave in stream order, just
// before its task:end, after the other task's later lines. A passing task
// drops only the debug lines among them.
func TestAFailingTaskSendsItsHeldLinesInOrderBeforeItsEnd(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "trimmed-upload", "a-failing-task-sends-its-held-lines-in-order-before-its-end")
	a, b := "/a:test", "/b:test"
	before := logRecord(a, "info", "before the first debug line", 0)
	d1 := logRecord(a, "debug", "d1", 0)
	b1 := logRecord(b, "info", "b1", 0)
	i2 := logRecord(a, "info", "i2", 0)
	w := logRecord(a, "warn", "w", 0)
	d2 := logRecord(a, "debug", "d2", 0)
	diagnostic := eventRecord(a, `{"type":"diagnostic","severity":"error","message":"boom"}`)
	b2 := logRecord(b, "error", "b2", 0)
	for _, status := range []string{protocolcli.TaskStatusFailed, protocolcli.TaskStatusCanceled, "unknown"} {
		end := endRecord(a, status)
		records := []string{startRecord(a), before, d1, b1, i2, w, progressRecord(a), d2, diagnostic, b2, end}
		assertView(t, records, startRecord(a), before, b1, b2, d1, i2, w, d2, diagnostic, end)
	}
	for _, status := range []string{protocolcli.TaskStatusSuccess, protocolcli.TaskStatusSkipped} {
		end := endRecord(a, status)
		records := []string{startRecord(a), before, d1, b1, i2, w, d2, diagnostic, b2, end}
		assertView(t, records, startRecord(a), before, b1, b2, i2, w, diagnostic, end)
	}
	// Once its task:end left, the owner holds again from its next debug line.
	again := logRecord(a, "info", "again", 0)
	assertView(t, []string{d1, endRecord(a, protocolcli.TaskStatusSuccess), again, d2, i2, endRecord(a, protocolcli.TaskStatusFailed)},
		endRecord(a, protocolcli.TaskStatusSuccess), again, d2, i2, endRecord(a, protocolcli.TaskStatusFailed))
}

// TestASharedBatchLineLeavesAtItsFirstFailingMember attributes batch lines to
// the members their context names (D-R3): a shared debug line leaves just
// before the first failing member's task:end and is dropped when every member
// passes; a shared info line leaves at the first member's end.
func TestASharedBatchLineLeavesAtItsFirstFailingMember(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "trimmed-upload", "a-shared-batch-line-leaves-at-its-first-failing-member")
	batch, a, b := "/workspace:test~test-batch", "/a:test~test", "/b:test~test"
	shared := batchRecord(batch, "debug", "shared", `{"batchProjectIds":["/a","/b",5,"/a",""]}`)
	onlyA := batchRecord(batch, "debug", "only a", `{"batchProjectId":"/a"}`)
	sharedInfo := batchRecord(batch, "info", "shared info", `{"batchProjectId":"/a","batchProjectIds":["/b"]}`)
	own := logRecord(batch, "info", "the batch's own line", 0)
	records := func(endA, endB string) []string {
		return []string{startRecord(batch), shared, onlyA, sharedInfo, own, endA, endB, endRecord(batch, protocolcli.TaskStatusSuccess)}
	}
	passA, passB := endRecord(a, protocolcli.TaskStatusSuccess), endRecord(b, protocolcli.TaskStatusSuccess)
	failA, failB := endRecord(a, protocolcli.TaskStatusFailed), endRecord(b, protocolcli.TaskStatusFailed)
	assertView(t, records(passA, failB), startRecord(batch), own, sharedInfo, passA, shared, failB, endRecord(batch, protocolcli.TaskStatusSuccess))
	assertView(t, records(failA, passB), startRecord(batch), own, shared, onlyA, sharedInfo, failA, passB, endRecord(batch, protocolcli.TaskStatusSuccess))
	assertView(t, records(passA, passB), startRecord(batch), own, sharedInfo, passA, passB, endRecord(batch, protocolcli.TaskStatusSuccess))
}

// TestARecordSplitAcrossRawFramesIsDecidedOnce writes records that straddle a
// 64 KiB raw boundary, and one longer than a frame, while the delivery loop
// runs: each is decided whole, so a passing task's debug line is absent
// entirely and a failing task's is sent whole.
func TestARecordSplitAcrossRawFramesIsDecidedOnce(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "trimmed-upload", "a-record-split-across-raw-frames-is-decided-once")
	h := newBatchHarness(t)
	a, b, c := "/a:test", "/b:test", "/c:test"
	first := logRecord(b, "info", "fills most of the first raw frame", 60*1024)
	straddling := logRecord(a, "debug", "straddles the first raw boundary", 10*1024)
	long := logRecord(b, "info", "longer than a frame", 100*1024)
	straddlingFailure := logRecord(c, "debug", "straddles a later raw boundary", 30*1024)
	records := []string{first, straddling, long, straddlingFailure, endRecord(a, protocolcli.TaskStatusSuccess), endRecord(c, protocolcli.TaskStatusFailed)}
	raw := stream(records...)
	if start := len(first) + 1; start >= protocolcli.SessionReportingChunkBytes || start+len(straddling) <= protocolcli.SessionReportingChunkBytes {
		t.Fatal("the debug line does not straddle the first raw boundary")
	}
	for _, record := range records {
		h.append(record)
		h.settle()
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
	want := stream(first, long, endRecord(a, protocolcli.TaskStatusSuccess), straddlingFailure, endRecord(c, protocolcli.TaskStatusFailed))
	got, final := reassemble(t, h.sent(), sessionstream.EventsFile)
	if !final || !bytes.Equal(got, want) {
		t.Fatalf("the receiver read %d bytes, want %d", len(got), len(want))
	}
	if view, _ := viewOf(t, raw); !bytes.Equal(view, want) {
		t.Fatal("the view built at once differs from the one built while the stream grew")
	}
}

// TestSessionEndAndTheFinalMarkerReleaseWhatIsHeld ends a session while a
// task still holds lines: they leave, in stream order, just before the
// session:end record. A stream without one releases them at the final
// marker, then its torn tail leaves last, as it is.
func TestSessionEndAndTheFinalMarkerReleaseWhatIsHeld(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "trimmed-upload", "session-end-and-the-final-marker-release-what-is-held")
	a, b := "/a:test", "/b:test"
	d1, i1, d2 := logRecord(a, "debug", "d1", 0), logRecord(a, "info", "i1", 0), logRecord(b, "debug", "d2", 0)
	assertView(t, []string{startRecord(a), d1, startRecord(b), d2, i1, endRecord(b, protocolcli.TaskStatusSuccess), sessionEndRecord, logRecord(a, "info", "after", 0)},
		startRecord(a), startRecord(b), endRecord(b, protocolcli.TaskStatusSuccess), d1, i1, sessionEndRecord, logRecord(a, "info", "after", 0))

	torn := []byte(`{"protocolVersion":2,"record":"task:event","identity":{"key":"/a:test"`)
	data := append(stream(startRecord(a), d1, logRecord(b, "info", "live", 0), i1), torn...)
	sent, s := resumePlan(t, finishedSession(t, data))
	want := append(stream(startRecord(a), logRecord(b, "info", "live", 0), d1, i1), torn...)
	if got, final := reassemble(t, sent, sessionstream.EventsFile); !final || !bytes.Equal(got, want) {
		t.Fatalf("the receiver read\n%s\n(final %v), want\n%s", got, final, want)
	}
	if !s.Complete || s.EventsRaw != int64(len(data)) || s.Events.Offset != int64(len(want)) {
		t.Fatalf("checkpoint = %+v, want view cursor %d at raw offset %d", s, len(want), len(data))
	}
}

// TestHeldRecordsPastTheCapReleaseTheTaskThatHoldsTheMost lowers the cap
// (D-R2): past it, the owner that holds the most releases every record, as a
// failure would, and sends live from then on. A tie goes to the owner whose
// first held record comes first, then to the smaller owner key, a shared
// record counts once, and a record that names a live owner leaves live.
func TestHeldRecordsPastTheCapReleaseTheTaskThatHoldsTheMost(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "trimmed-upload", "held-records-past-the-cap-release-the-task-that-holds-the-most")
	defer func(held int) { viewHeldCap = held }(viewHeldCap)
	if viewHeldCap != 1_000_000 {
		t.Fatalf("the view holds at most %d records, want 1,000,000", viewHeldCap)
	}
	a, b := "/a:test", "/b:test"
	debug := func(key, message string) string { return logRecord(key, "debug", message, 0) }
	pass := protocolcli.TaskStatusSuccess

	viewHeldCap = 3
	assertView(t, []string{debug(a, "a1"), debug(a, "a2"), debug(a, "a3"), debug(b, "b1"), debug(a, "a4"), progressRecord(a), endRecord(a, pass), endRecord(b, pass)},
		debug(a, "a1"), debug(a, "a2"), debug(a, "a3"), debug(a, "a4"), endRecord(a, pass), endRecord(b, pass))
	// a and b hold two each: the tie goes to b, whose first held record comes
	// first. Its later debug line leaves live, and a still drops its own.
	assertView(t, []string{debug(b, "b1"), debug(a, "a1"), debug(a, "a2"), debug(b, "b2"), debug(b, "b3"), endRecord(b, pass), debug(a, "a3"), endRecord(a, pass)},
		debug(b, "b1"), debug(b, "b2"), debug(b, "b3"), endRecord(b, pass), endRecord(a, pass))

	// Three records held by two owners count three, not four: nothing is
	// released at a cap of three, and both owners pass.
	batch, x, y := "/workspace:test-batch", "/x:test", "/y:test"
	shared := batchRecord(batch, "debug", "shared", `{"batchProjectIds":["/x","/y"]}`)
	assertView(t, []string{shared, debug(x, "x1"), debug(y, "y1"), endRecord(x, pass), endRecord(y, pass)}, endRecord(x, pass), endRecord(y, pass))
	// A second shared record breaks the cap: x and y hold three each, from
	// the same first record, and the tie goes to the smaller owner key. Both
	// shared records leave once, and y then holds only its own lines.
	again := batchRecord(batch, "debug", "shared again", `{"batchProjectIds":["/y","/x"]}`)
	assertView(t, []string{shared, debug(x, "x1"), debug(y, "y1"), again, debug(y, "y2"), endRecord(y, pass), endRecord(x, pass)},
		shared, debug(x, "x1"), again, endRecord(y, pass), endRecord(x, pass))

	// The cap makes b live. A shared debug line that names a and b then
	// leaves live: it is not held for a, which passes, while b fails. a
	// starts holding at that line, its first debug line, so its info line
	// leaves after c's, at its end.
	c := "/c:test"
	withLive := batchRecord(batch, "debug", "shared with a live owner", `{"batchProjectIds":["/a","/b"]}`)
	a1, c1 := logRecord(a, "info", "a1", 0), logRecord(c, "info", "c1", 0)
	failed := endRecord(b, protocolcli.TaskStatusFailed)
	assertView(t, []string{debug(b, "b1"), debug(b, "b2"), debug(b, "b3"), debug(b, "b4"), withLive, a1, c1, debug(a, "a2"), endRecord(a, pass), failed},
		debug(b, "b1"), debug(b, "b2"), debug(b, "b3"), debug(b, "b4"), withLive, c1, a1, endRecord(a, pass), failed)
}

// TestASharedRecordIsHeldOnce holds batch lines that name every member of a
// 200-member batch: each line is one entry in one group, and each member
// references that group once, whatever the number of lines. When every
// member passes, the lines are dropped.
func TestASharedRecordIsHeldOnce(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "trimmed-upload", "held-records-past-the-cap-release-the-task-that-holds-the-most")
	spectest.Proves(t, "cli/native-session-reporting", "trimmed-upload", "a-shared-batch-line-leaves-at-its-first-failing-member")
	ids := make([]string, 200)
	for i := range ids {
		ids[i] = fmt.Sprintf("/m%03d", i)
	}
	context, err := json.Marshal(map[string][]string{"batchProjectIds": ids})
	if err != nil {
		t.Fatal(err)
	}
	for _, lines := range []int{10, 1000} {
		log, err := sessionstream.Create(t.TempDir(), batchSessionID)
		if err != nil {
			t.Fatal(err)
		}
		for i := range lines {
			if err := log.Append([]byte(batchRecord("/workspace:test-batch", "debug", fmt.Sprintf("shared %d", i), string(context)))); err != nil {
				t.Fatal(err)
			}
		}
		sub, err := log.Subscribe("view-test", 0)
		if err != nil {
			t.Fatal(err)
		}
		v := newView(sub, 0)
		if err := v.fill(math.MaxInt64); err != nil {
			t.Fatal(err)
		}
		references, groups := 0, map[*viewGroup]bool{}
		for _, o := range v.owners {
			references += len(o.groups)
			for _, g := range o.groups {
				groups[g] = true
			}
		}
		entries := 0
		for g := range groups {
			entries += len(g.debug) + len(g.other)
		}
		if len(v.owners) != len(ids) || references != len(ids) || len(groups) != 1 || entries != lines || v.held != lines {
			t.Fatalf("%d shared lines: %d owners hold %d entries in %d groups through %d references (held %d), want %d owners, %d entries, 1 group, %d references",
				lines, len(v.owners), entries, len(groups), references, v.held, len(ids), lines, len(ids))
		}
		var ends []string
		for _, id := range ids {
			ends = append(ends, endRecord(id+":test", protocolcli.TaskStatusSuccess))
			if err := log.Append([]byte(ends[len(ends)-1])); err != nil {
				t.Fatal(err)
			}
		}
		if err := v.fill(math.MaxInt64); err != nil {
			t.Fatal(err)
		}
		if v.held != 0 || len(v.owners) != 0 || len(v.groups) != 0 || v.end != int64(len(stream(ends...))) {
			t.Fatalf("after every member passed: held %d, %d owners, %d open groups, %d view bytes", v.held, len(v.owners), len(v.groups), v.end)
		}
		_ = sub.Close()
		_ = log.Close()
	}
}

// TestViewFramesKeepTheirBytesAfterARetry has the receiver refuse every
// events.jsonl frame once, with retry: each sequence is resent with the same
// bytes.
func TestViewFramesKeepTheirBytesAfterARetry(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "trimmed-upload", "view-frames-keep-their-bytes-after-a-retry-a-reporter-restart-and-an-engine-restart")
	records := viewSession(0, 3)
	h := newPlanHarness(t, nil, toBytes(records)...)
	refused := map[int64]bool{}
	h.serve(func(chunk protocolcli.SessionReportingChunk) *protocolcli.SessionReportingAck {
		ack := chunk.Ack()
		if chunk.Artifact == sessionstream.EventsFile && !refused[chunk.Sequence] {
			refused[chunk.Sequence] = true
			ack.OK, ack.Retryable, ack.Code = false, true, "receiver_unavailable"
		}
		return &ack
	})
	if err := h.finish(); err != nil {
		t.Fatalf("Finish = %v", err)
	}
	view, _ := viewOf(t, stream(records...))
	chunks := h.sent()
	assertFrames(t, view, chunks)
	if events := len(chunks) - strings.Count(frameOrder(chunks), "session.json"); events != 2*len(refused) || len(refused) < 3 {
		t.Fatalf("%d events.jsonl chunks for %d sequences, want each sent twice", events, len(refused))
	}
}

// viewSession is the records of one owner's run in parts: part i of n holds
// a passing task's debug lines, a failing task's held lines, and live lines,
// each part more than a frame of view bytes.
func viewSession(i, n int) []string {
	var records []string
	for part := i; part < n; part++ {
		pass, fail, live := fmt.Sprintf("/pass%d:test", part), fmt.Sprintf("/fail%d:test", part), fmt.Sprintf("/live%d:test", part)
		records = append(records, startRecord(pass), startRecord(fail), startRecord(live))
		for line := range 40 {
			records = append(records,
				logRecord(pass, "debug", fmt.Sprintf("pass %d %d", part, line), 900),
				progressRecord(live),
				logRecord(fail, "debug", fmt.Sprintf("fail %d %d", part, line), 300),
				logRecord(live, "info", fmt.Sprintf("live %d %d", part, line), 1200),
			)
		}
		records = append(records, endRecord(pass, protocolcli.TaskStatusSuccess), endRecord(fail, protocolcli.TaskStatusFailed), endRecord(live, protocolcli.TaskStatusSuccess))
	}
	return records
}

func toBytes(records []string) [][]byte {
	out := make([][]byte, len(records))
	for i, record := range records {
		out[i] = []byte(record)
	}
	return out
}

// TestViewFramesKeepTheirBytesAfterAReporterRestart runs a real reporter
// that exits without an answer at the second events.jsonl frame, once: the
// worker starts it again and resends that sequence with the same bytes.
func TestViewFramesKeepTheirBytesAfterAReporterRestart(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "trimmed-upload", "view-frames-keep-their-bytes-after-a-retry-a-reporter-restart-and-an-engine-restart")
	defer runcredential.SetForTest("")()
	fx := newPlanReporterFixture(t, planCrashOnceReporter)
	dir := t.TempDir()
	log, err := sessionstream.Create(dir, batchSessionID)
	if err != nil {
		t.Fatal(err)
	}
	records := viewSession(0, 3)
	for _, record := range records {
		if err := log.Append([]byte(record)); err != nil {
			t.Fatal(err)
		}
	}
	run, err := Start(WithEventsBatchInterval(Capture(context.Background()), 10*time.Millisecond), SessionReporter, log, batchSessionID, fx.resolve)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionFile), []byte(`{"protocolVersion":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run.Finish(); err != nil {
		t.Fatalf("Finish = %v", err)
	}
	processes := fx.processes(t)
	if len(processes) != 2 {
		t.Fatalf("reporter processes = %d, want one that exited and one started again", len(processes))
	}
	crashed := processes[0][len(processes[0])-1]
	if crashed.Artifact != sessionstream.EventsFile || crashed.Sequence != 1 || !sameFrame(t, crashed, processes[1][0]) {
		t.Fatalf("the restarted reporter read %s first, want the frame the first one exited at", frameOrder(processes[1][:1]))
	}
	view, _ := viewOf(t, stream(records...))
	assertFrames(t, view, append(processes[0], processes[1]...))
}

// TestViewFramesKeepTheirBytesAfterAnEngineRestart stops a run mid-session
// and starts it again from its checkpoint, on the live stream and as a replay
// of the finalized session: the view is rebuilt from offset 0, skips to the
// checkpoint's cursor, and every sequence keeps its bytes.
func TestViewFramesKeepTheirBytesAfterAnEngineRestart(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "trimmed-upload", "view-frames-keep-their-bytes-after-a-retry-a-reporter-restart-and-an-engine-restart")
	defer runcredential.SetForTest("")()
	for _, replay := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "replay"}[replay], func(t *testing.T) {
			fx := newPlanReporterFixture(t, "")
			ctx := WithEventsBatchInterval(Capture(context.Background()), 10*time.Millisecond)
			dir := t.TempDir()
			log, err := sessionstream.Create(dir, batchSessionID)
			if err != nil {
				t.Fatal(err)
			}
			// A task that holds across the restart keeps the raw
			// acknowledgement behind its first line.
			held := "/held:test"
			first := append([]string{startRecord(held), logRecord(held, "debug", "held across the restart", 0)}, viewSession(0, 2)...)
			for _, record := range first {
				if err := log.Append([]byte(record)); err != nil {
					t.Fatal(err)
				}
			}
			run, err := Start(ctx, SessionReporter, log, batchSessionID, fx.resolve)
			if err != nil {
				t.Fatal(err)
			}
			awaitCheckpoint(t, dir, func(s state) bool { return s.Events.Sequence >= 2 })
			run.cancel()
			<-run.done
			_ = run.events.Close()
			stopped := readCheckpoint(t, dir)
			heldAt := int64(len(first[0]) + 1)
			if stopped.Version != viewCheckpointVersion || stopped.EventsRaw > heldAt || stopped.Events.Offset <= heldAt {
				t.Fatalf("checkpoint version %d at view offset %d acknowledges raw offset %d, want version 2 ahead of a raw offset at or before the held line at %d",
					stopped.Version, stopped.Events.Offset, stopped.EventsRaw, heldAt)
			}
			second := append(viewSession(2, 4), logRecord(held, "info", "after the restart", 0), endRecord(held, protocolcli.TaskStatusFailed))
			for _, record := range second {
				if err := log.Append([]byte(record)); err != nil {
					t.Fatal(err)
				}
			}
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, sessionFile), []byte(`{"protocolVersion":2}`), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := resumedAcknowledgement(t, dir, log); got.Offset != stopped.EventsRaw {
				t.Fatalf("the resumed run subscribes at raw offset %d, want the checkpoint's %d", got.Offset, stopped.EventsRaw)
			}
			var chunks []protocolcli.SessionReportingChunk
			var s state
			if replay {
				var sent []protocolcli.SessionReportingChunk
				sent, s = resumePlan(t, dir)
				for _, p := range fx.processes(t) {
					chunks = append(chunks, p...)
				}
				chunks = append(chunks, sent...)
			} else {
				run, err := Start(ctx, SessionReporter, log, batchSessionID, fx.resolve)
				if err != nil {
					t.Fatal(err)
				}
				if err := run.Finish(); err != nil {
					t.Fatalf("Finish = %v", err)
				}
				for _, p := range fx.processes(t) {
					chunks = append(chunks, p...)
				}
				s = readCheckpoint(t, dir)
			}
			data := stream(append(first, second...)...)
			view, _ := viewOf(t, data)
			assertFrames(t, view, chunks)
			if !s.Complete || s.EventsRaw != int64(len(data)) || sessionReporterEvidence(t, dir) != protocolcli.SubscriberEvidenceDelivered {
				t.Fatalf("checkpoint after the restart = %+v", s)
			}
		})
	}
}

// resumedAcknowledgement opens the session reporter's checkpoint in dir on
// log, a final stream, as a resumed run does, and returns the position its
// subscription starts acknowledged at.
func resumedAcknowledgement(t *testing.T, dir string, log *sessionstream.Log) sessionstream.Position {
	t.Helper()
	r, err := openRun(context.Background(), SessionReporter, dir, batchSessionID, holderProvider)
	if err != nil {
		t.Fatal(err)
	}
	defer r.release()
	if err := r.subscribe(log); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.events.Close() }()
	evidence, err := r.events.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	return evidence.Acknowledged
}

// TestAResumedViewRunSubscribesAtItsRawOffset resumes version 2 checkpoints:
// the subscription starts at eventsRaw, behind the line a task still holds,
// not at the view cursor. A checkpoint that acknowledged the view's final
// marker, with spec-verification.json still open, acknowledges the raw
// stream's final marker, and the evidence reads delivered.
func TestAResumedViewRunSubscribesAtItsRawOffset(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "trimmed-upload", "the-view-final-marker-reads-delivered")
	held := "/held:test"
	records := append([]string{startRecord(held), logRecord(held, "debug", "held", 0)}, viewSession(0, 2)...)
	data := stream(records...)
	view, _ := viewOf(t, data)
	heldAt := int64(len(records[0]) + 1)
	frames := int64(len(view)+protocolcli.SessionReportingChunkBytes-1) / protocolcli.SessionReportingChunkBytes
	dir := finishedSession(t, data)
	write := func(s state) {
		t.Helper()
		s.Version, s.Provider, s.SessionID = viewCheckpointVersion, holderProvider, batchSessionID
		checkpoint, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, StateFile), checkpoint, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	log, err := sessionstream.Open(dir, batchSessionID)
	if err != nil {
		t.Fatal(err)
	}

	write(state{Events: cursor{Offset: 2 * protocolcli.SessionReportingChunkBytes, Sequence: 2}, EventsRaw: heldAt, PlanOmitted: planAbsent})
	if got, want := resumedAcknowledgement(t, dir, log), (sessionstream.Position{Offset: heldAt, Records: 1}); got != want {
		t.Fatalf("the resumed run starts acknowledged at %+v, want the checkpoint's raw offset %+v", got, want)
	}

	write(state{
		Events: cursor{Offset: int64(len(view)), Sequence: frames + 1, Final: true}, EventsRaw: int64(len(data)),
		Session: cursor{Offset: 21, Sequence: 2, Final: true}, PlanOmitted: planAbsent,
	})
	sent, s := resumePlan(t, dir)
	if len(sent) != 0 || !s.Complete || s.SpecVerificationOmitted != specVerificationAbsent || s.EventsRaw != int64(len(data)) {
		t.Fatalf("replay sent %s and left %+v", frameOrder(sent), s)
	}
	if got := sessionReporterEvidence(t, dir); got != protocolcli.SubscriberEvidenceDelivered {
		t.Fatalf("evidence %q, want delivered", got)
	}
}

func readCheckpoint(t *testing.T, dir string) state {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, StateFile))
	if err != nil {
		t.Fatal(err)
	}
	var s state
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// awaitCheckpoint returns the first durable checkpoint in dir that done
// accepts.
func awaitCheckpoint(t *testing.T, dir string, done func(state) bool) state {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(filepath.Join(dir, StateFile)); err == nil {
			var s state
			if json.Unmarshal(data, &s) == nil && done(s) {
				return s
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the checkpoint never reached the expected state")
	return state{}
}

// TestAPendingViewFrameMustBeTheRebuiltViewsBytes resumes checkpoints that
// hold a pending events.jsonl frame: the frame of the rebuilt view is sent as
// it is, and any other one fails delivery as a changed artifact.
func TestAPendingViewFrameMustBeTheRebuiltViewsBytes(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "trimmed-upload", "view-frames-keep-their-bytes-after-a-retry-a-reporter-restart-and-an-engine-restart")
	records := viewSession(0, 2)
	data := stream(records...)
	view, _ := viewOf(t, data)
	offset := int64(protocolcli.SessionReportingChunkBytes)
	frame := view[offset : offset+protocolcli.SessionReportingChunkBytes]
	for _, tc := range []struct {
		name    string
		pending protocolcli.SessionReportingChunk
		ok      bool
	}{
		{"the view's frame", protocolcli.NewSessionReportingChunk(batchSessionID, sessionstream.EventsFile, offset, 1, frame, false), true},
		{"a raw frame", protocolcli.NewSessionReportingChunk(batchSessionID, sessionstream.EventsFile, offset, 1, data[offset:offset+protocolcli.SessionReportingChunkBytes], false), false},
		{"a final marker before the view's end", protocolcli.NewSessionReportingChunk(batchSessionID, sessionstream.EventsFile, offset, 1, nil, true), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := finishedSession(t, data)
			pending := tc.pending
			checkpoint, err := json.Marshal(state{
				Version: viewCheckpointVersion, Provider: "@test/provider", SessionID: batchSessionID,
				Events: cursor{Offset: offset, Sequence: 1}, EventsRaw: 0, Session: cursor{Offset: 21, Sequence: 1, Final: true},
				PlanOmitted: planAbsent, Pending: &pending,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, StateFile), checkpoint, 0o600); err != nil {
				t.Fatal(err)
			}
			log, err := sessionstream.Open(dir, batchSessionID)
			if err != nil {
				t.Fatal(err)
			}
			r, err := openRun(context.Background(), SessionReporter, dir, batchSessionID, "@test/provider")
			if err != nil {
				t.Fatal(err)
			}
			if err := r.subscribe(log); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.events.Close() }()
			err = r.verifyPendingView()
			r.release()
			if tc.ok != (err == nil) || err != nil && err.Error() != "reporting artifact changed: events.jsonl" {
				t.Fatalf("verifyPendingView = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

// TestTheViewFinalMarkerReadsDelivered delivers a trimmed view: once its
// final marker is acknowledged, subscribers.json reads delivered at the end of
// the raw stream, whose every record it counts.
func TestTheViewFinalMarkerReadsDelivered(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "trimmed-upload", "the-view-final-marker-reads-delivered")
	records := append(viewSession(0, 2), startRecord("/open:test"), logRecord("/open:test", "debug", "never ends", 0))
	data := stream(records...)
	dir := finishedSession(t, data)
	sent, s := resumePlan(t, dir)
	view, _ := viewOf(t, data)
	assertFrames(t, view, sent)
	if len(view) >= len(data) || s.Events.Offset != int64(len(view)) || s.EventsRaw != int64(len(data)) || !s.Events.Final {
		t.Fatalf("checkpoint = %+v for a %d-byte view of %d raw bytes", s, len(view), len(data))
	}
	document, err := sessionstream.ReadEvidence(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := protocolcli.SessionStreamPosition{Offset: int64(len(data)), Records: int64(len(records))}
	for _, entry := range document.Subscribers {
		if entry.Name == SessionReporter.Name && (entry.Evidence != protocolcli.SubscriberEvidenceDelivered || entry.Acknowledged != want || document.Stream != want || entry.Lost != 0) {
			t.Fatalf("evidence = %+v of stream %+v, want delivered at %+v", entry, document.Stream, want)
		}
	}
	if got := sessionReporterEvidence(t, dir); got != protocolcli.SubscriberEvidenceDelivered {
		t.Fatalf("evidence %q", got)
	}
}

// TestARawAcknowledgementWaitsForAReleasedRecord releases a held record
// after a later live one: the raw offset the subscription acknowledges stays
// before the released record until the view bytes it left as are
// acknowledged.
func TestARawAcknowledgementWaitsForAReleasedRecord(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "trimmed-upload", "the-view-final-marker-reads-delivered")
	a := "/a:test"
	live, held, later, end := logRecord("/b:test", "info", "live", 0), logRecord(a, "debug", "held", 0), logRecord("/b:test", "info", "later", 0), endRecord(a, protocolcli.TaskStatusFailed)
	data := stream(live, held, later, end)
	dir := finishedSession(t, data)
	log, err := sessionstream.Open(dir, batchSessionID)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := log.Subscribe("view-test", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	v := newView(sub, 0)
	if err := v.fill(math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	heldAt := int64(len(live) + 1)
	for _, tc := range []struct{ cursor, raw int64 }{
		{0, 0},
		{int64(len(live) + 1), heldAt},
		{int64(len(live) + len(later) + 2), heldAt},
		{int64(len(live) + len(later) + len(held) + 3), int64(len(data) - len(end) - 1)},
		{int64(len(data)), int64(len(data))},
	} {
		if got := v.acknowledged(tc.cursor); got != tc.raw {
			t.Fatalf("view cursor %d acknowledges raw offset %d, want %d", tc.cursor, got, tc.raw)
		}
	}
}

// TestAVersion1CheckpointThatSentEventsKeepsRawFraming resumes a version 1
// checkpoint that framed an events.jsonl chunk: its receiver holds raw
// offsets, so the session keeps raw framing. One that framed none moves to
// version 2 and sends the view.
func TestAVersion1CheckpointThatSentEventsKeepsRawFraming(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "trimmed-upload", "a-version-1-checkpoint-that-sent-events-keeps-raw-framing")
	a := "/a:test"
	records := []string{startRecord(a), logRecord(a, "debug", "dropped from the view", 0), progressRecord(a), endRecord(a, protocolcli.TaskStatusSuccess)}
	data := stream(records...)
	first := int64(len(records[0]) + 1)

	dir := finishedSession(t, data)
	sentOne, err := json.Marshal(state{Version: rawCheckpointVersion, Provider: "@test/provider", SessionID: batchSessionID, Events: cursor{Offset: first, Sequence: 1}, PlanOmitted: planLate})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, StateFile), sentOne, 0o600); err != nil {
		t.Fatal(err)
	}
	sent, s := resumePlan(t, dir)
	if got, final := reassemble(t, append([]protocolcli.SessionReportingChunk{protocolcli.NewSessionReportingChunk(batchSessionID, sessionstream.EventsFile, 0, 0, data[:first], false)}, sent...), sessionstream.EventsFile); !final || !bytes.Equal(got, data) {
		t.Fatalf("a raw session resumed as\n%s", got)
	}
	if s.Version != rawCheckpointVersion || s.EventsRaw != 0 || !s.Complete || s.Events.Offset != int64(len(data)) || sessionReporterEvidence(t, dir) != protocolcli.SubscriberEvidenceDelivered {
		t.Fatalf("checkpoint = %+v, want version 1 at the raw end", s)
	}
	if saved, err := os.ReadFile(filepath.Join(dir, StateFile)); err != nil || bytes.Contains(saved, []byte("eventsRaw")) {
		t.Fatalf("a version 1 checkpoint records a raw offset: %s", saved)
	}

	dir = finishedSession(t, data)
	sentNone, err := json.Marshal(state{Version: rawCheckpointVersion, Provider: "@test/provider", SessionID: batchSessionID, Error: "provider_unavailable"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, StateFile), sentNone, 0o600); err != nil {
		t.Fatal(err)
	}
	sent, s = resumePlan(t, dir)
	view := stream(records[0], records[3])
	if got, final := reassemble(t, sent, sessionstream.EventsFile); !final || !bytes.Equal(got, view) {
		t.Fatalf("a session that sent no events resumed as\n%s", got)
	}
	if s.Version != viewCheckpointVersion || s.EventsRaw != int64(len(data)) || s.Events.Offset != int64(len(view)) || !s.Complete {
		t.Fatalf("checkpoint = %+v, want version 2", s)
	}

	// A pending events.jsonl frame is a framed chunk too.
	pending := protocolcli.NewSessionReportingChunk("session", sessionstream.EventsFile, 0, 0, data[:first], false)
	for name, tc := range map[string]struct {
		capability Capability
		checkpoint state
		version    int
	}{
		"pending raw frame":       {SessionReporter, state{Version: rawCheckpointVersion, PlanOmitted: planLate, Pending: &pending}, rawCheckpointVersion},
		"session.json progressed": {SessionReporter, state{Version: rawCheckpointVersion, PlanOmitted: planLate, Session: cursor{Offset: 2, Sequence: 1}}, viewCheckpointVersion},
		"log reporter":            {LogReporter, state{Version: rawCheckpointVersion}, rawCheckpointVersion},
		"view":                    {SessionReporter, state{Version: viewCheckpointVersion, PlanOmitted: planLate, Events: cursor{Offset: 2, Sequence: 1}, EventsRaw: 9}, viewCheckpointVersion},
	} {
		r := openCheckpoint(t, tc.capability, tc.checkpoint)
		if r == nil || r.state.Version != tc.version {
			t.Fatalf("%s: opened as %+v, want version %d", name, r, tc.version)
		}
	}
	for name, tc := range map[string]struct {
		capability Capability
		checkpoint state
	}{
		"log reporter view":      {LogReporter, state{Version: viewCheckpointVersion}},
		"unknown version":        {SessionReporter, state{Version: 3}},
		"raw offset at version1": {SessionReporter, state{Version: rawCheckpointVersion, PlanOmitted: planLate, Events: cursor{Offset: 2, Sequence: 1}, EventsRaw: 2}},
		"negative raw offset":    {SessionReporter, state{Version: viewCheckpointVersion, EventsRaw: -1}},
	} {
		if r := openCheckpoint(t, tc.capability, tc.checkpoint); r != nil {
			t.Fatalf("%s: an invalid checkpoint opened as %+v", name, r.state)
		}
	}
}

// openCheckpoint opens checkpoint, as capability's, for session "session" of
// provider @test/provider, or returns nil when openRun refuses it.
func openCheckpoint(t *testing.T, capability Capability, checkpoint state) *Run {
	t.Helper()
	dir := t.TempDir()
	checkpoint.Provider, checkpoint.SessionID = "@test/provider", "session"
	data, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, capability.StateFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := openRun(context.Background(), capability, dir, "session", "@test/provider")
	if err != nil {
		return nil
	}
	r.release()
	return r
}

// TestTheLogReporterSendsTheRawStream delivers the same records to the log
// reporter: it receives every byte, debug lines and progress events
// included, and its checkpoint stays version 1, without a raw offset.
func TestTheLogReporterSendsTheRawStream(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "trimmed-upload", "the-log-reporter-sends-the-raw-stream")
	h := newCapabilityHarness(t, LogReporter)
	records := viewSession(0, 2)
	for _, record := range records {
		h.append(record)
		h.settle()
	}
	if err := h.log.Close(); err != nil {
		t.Fatal(err)
	}
	h.settle()
	close(h.run.finish)
	if err := <-h.done; err != nil {
		t.Fatalf("terminal drain: %v", err)
	}
	if got, final := reassemble(t, h.sent(), sessionstream.EventsFile); !final || !bytes.Equal(got, stream(records...)) {
		t.Fatalf("the log reporter read %d bytes, want the %d raw bytes", len(got), len(stream(records...)))
	}
	saved, err := json.Marshal(h.run.state)
	if err != nil {
		t.Fatal(err)
	}
	if h.run.state.Version != rawCheckpointVersion || bytes.Contains(saved, []byte("eventsRaw")) {
		t.Fatalf("log reporter checkpoint = %s", saved)
	}
	dir := t.TempDir()
	r, err := openRun(context.Background(), LogReporter, dir, "session", "@test/provider")
	if err != nil {
		t.Fatal(err)
	}
	r.release()
	if fresh, err := os.ReadFile(filepath.Join(dir, LogStateFile)); err != nil || !bytes.HasPrefix(fresh, []byte(`{"version":1,`)) || bytes.Contains(fresh, []byte("eventsRaw")) {
		t.Fatalf("a new log reporter checkpoint is %s", fresh)
	}
}

// TestTheViewLeavesALineThatIsNotARecordAsItIs passes every line it cannot
// decide on through unchanged, in stream order (rule 9), and holds no record
// that names no owner.
func TestTheViewLeavesALineThatIsNotARecordAsItIs(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "trimmed-upload", "progress-events-and-the-debug-lines-of-a-passing-task-are-not-sent")
	a := "/a:test"
	notJSON := "not a record"
	wrongType := `{"record":"task:event","identity":{"key":"/a:test"},"event":{"type":"log","level":5}}`
	array := `[1,2]`
	noOwner := `{"record":"task:event","event":{"type":"log","level":"debug","message":"no owner"}}`
	empty := ""
	assertView(t, []string{logRecord(a, "debug", "held", 0), notJSON, wrongType, array, noOwner, empty, endRecord(a, protocolcli.TaskStatusSuccess)},
		notJSON, wrongType, array, noOwner, empty, endRecord(a, protocolcli.TaskStatusSuccess))
	assertView(t, nil)
}
