package sessionreporter

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

// TestLogReporterBatchesEveryTwoSeconds drives the real delivery loop of the log
// reporter under a manual clock: a trickle waits two seconds from the previous
// chunk, never the session reporter's ten.
func TestLogReporterBatchesEveryTwoSeconds(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "bounded-transport", "log-reporter-chunks-wait-for-a-full-frame-or-two-seconds")
	h := newCapabilityHarness(t, LogReporter)
	h.append(`{"record":"task:start"}`)
	if wait := h.settle(); wait != 2*time.Second {
		t.Fatalf("a trickle at the worker's start waits %s, want 2s", wait)
	}
	if h.clock.Advance(time.Second) {
		t.Fatal("the batch timer fired before two seconds")
	}
	h.append(`{"record":"task:event"}`)
	if wait := h.settle(); wait != time.Second {
		t.Fatalf("a second record re-armed the wait for %s, want the remaining 1s", wait)
	}
	if got := h.sent(); len(got) != 0 {
		t.Fatalf("a trickle left before two seconds: %d chunks", len(got))
	}
	if !h.clock.Advance(time.Second) {
		t.Fatal("the batch timer did not fire after two seconds")
	}
	if wait := h.settle(); wait != 2*time.Second {
		t.Fatalf("after a send the next trickle waits %s, want 2s", wait)
	}
	got := h.sent()
	want := "{\"record\":\"task:start\"}\n{\"record\":\"task:event\"}\n"
	if len(got) != 1 || got[0].Artifact != "events.jsonl" || string(got[0].Data) != want || got[0].Offset != 0 || got[0].Sequence != 0 || got[0].Final {
		t.Fatalf("after two seconds: %+v, want one events chunk with both records", got)
	}
}

// TestLogReporterBoundsChunksByDuration sends one record per second for a
// minute: the log reporter sends one chunk per two seconds, and the chunks
// reassemble the stream.
func TestLogReporterBoundsChunksByDuration(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "bounded-transport", "log-reporter-chunks-wait-for-a-full-frame-or-two-seconds")
	const seconds = 60
	h := newCapabilityHarness(t, LogReporter)
	var stream bytes.Buffer
	for second := 0; second < seconds; second++ {
		record := `{"record":"task:event","second":` + strings.Repeat("0", 40) + `}`
		h.append(record)
		stream.WriteString(record + "\n")
		h.settle()
		if h.clock.Advance(time.Second) {
			h.settle()
		}
	}
	got := h.sent()
	if want := seconds / 2; len(got) != want {
		t.Fatalf("%ds of steady trickle sent %d chunks, want %d", seconds, len(got), want)
	}
	var delivered bytes.Buffer
	for i, chunk := range got {
		if chunk.Sequence != int64(i) || chunk.Offset != int64(delivered.Len()) || bytes.Count(chunk.Data, []byte("\n")) != 2 {
			t.Fatalf("chunk %d = offset %d sequence %d with %d records", i, chunk.Offset, chunk.Sequence, bytes.Count(chunk.Data, []byte("\n")))
		}
		delivered.Write(chunk.Data)
	}
	if !bytes.Equal(delivered.Bytes(), stream.Bytes()) {
		t.Fatal("batched chunks do not reassemble the stream")
	}
}

func TestLogReporterSendsAFullFrameAtOnce(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "bounded-transport", "log-reporter-chunks-wait-for-a-full-frame-or-two-seconds")
	h := newCapabilityHarness(t, LogReporter)
	h.append(strings.Repeat("x", protocolcli.SessionReportingChunkBytes))
	wait := h.settle()
	got := h.sent()
	if len(got) != 1 || len(got[0].Data) != protocolcli.SessionReportingChunkBytes {
		t.Fatalf("a committed full frame was held: %d chunks", len(got))
	}
	if wait != 2*time.Second {
		t.Fatalf("the 1-byte remainder waits %s, want 2s from the frame's send", wait)
	}
}

// TestLogReporterDrainsEventsOnlyWhenTerminal ends the graph with a finalized
// session.json beside the stream: the log reporter drains the rest of the
// events at once and closes them, and never frames session.json.
func TestLogReporterDrainsEventsOnlyWhenTerminal(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "bounded-transport", "log-reporter-chunks-wait-for-a-full-frame-or-two-seconds")
	h := newCapabilityHarness(t, LogReporter)
	h.append(`{"record":"task:end"}`)
	h.append(`{"record":"session:end"}`)
	h.settle()
	if err := h.log.Close(); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if err := os.WriteFile(filepath.Join(h.run.dir, "session.json"), []byte(`{"protocolVersion":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	close(h.run.finish)
	if err := <-h.done; err != nil {
		t.Fatalf("terminal drain: %v", err)
	}
	got := h.sent()
	order := make([]string, 0, len(got))
	for _, chunk := range got {
		order = append(order, chunk.Artifact+map[bool]string{true: ":final", false: ""}[chunk.Final])
	}
	if strings.Join(order, ",") != "events.jsonl,events.jsonl:final" || string(got[0].Data) != "{\"record\":\"task:end\"}\n{\"record\":\"session:end\"}\n" {
		t.Fatalf("terminal drain sent %v", order)
	}
	if !h.run.state.Complete || h.run.state.Session != (cursor{}) || h.run.state.Events.Sequence != 2 {
		t.Fatalf("terminal drain left checkpoint %+v", h.run.state)
	}
}
