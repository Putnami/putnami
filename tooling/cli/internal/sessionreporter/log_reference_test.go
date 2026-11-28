package sessionreporter

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/sessionrefs"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
)

// TestTheLogReporterSendsTheRecordsACacheEntryHolds runs the log reporter's
// delivery loop over a session that records a reused task by reference: its
// new checkpoint is version 6, whose frames carry every record of the stream,
// none trimmed, and the records the entry holds in place of the reference, so
// the receiver reads the records a recorder that kept every record would have
// written.
func TestTheLogReporterSendsTheRecordsACacheEntryHolds(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "the-session-reporter-sends-the-rebuilt-records")
	entries := referenceStore(t)
	a, b := "/a:test", "/b:build"
	h := newReferenceHarness(t, LogReporter, entries)
	if h.run.state.Version != resolvedRawCheckpointVersion || !h.run.viewFraming() {
		t.Fatalf("a new log checkpoint of a session by reference is version %d, want %d framed from the whole view", h.run.state.Version, resolvedRawCheckpointVersion)
	}
	records := []string{startRecord(a), startRecord(b), progressRecord(b), logRecord(b, "debug", "b detail", 64), referencedEnd(a), endRecord(b, protocolcli.TaskStatusSuccess), sessionEndRecord}
	for _, record := range records {
		h.append(record)
	}
	if err := h.log.Close(); err != nil {
		t.Fatal(err)
	}
	h.settle()
	close(h.run.finish)
	if err := <-h.done; err != nil {
		t.Fatalf("terminal drain: %v", err)
	}
	raw := stream(records...)
	want := bytes.Replace(raw, stream(records[4]), resolvedBytes(t, entries, records[4]), 1)
	got, final := reassemble(t, h.sent(), sessionstream.EventsFile)
	if !final || !bytes.Equal(got, want) {
		t.Fatalf("the receiver read (final %v)\n%s\nwant every record, the rebuilt ones in place of the reference\n%s", final, got, want)
	}
	if h.run.state.EventsRaw != int64(len(raw)) || h.run.state.Events.Offset != int64(len(want)) {
		t.Fatalf("checkpoint acknowledges raw offset %d at view offset %d, want %d at %d", h.run.state.EventsRaw, h.run.state.Events.Offset, len(raw), len(want))
	}
}

// TestASessionByReferenceIsNeverFramedRaw opens the log reporter's checkpoint
// of a session that declares it records by reference: a new one, and a
// version 1 one that framed nothing yet, take version 6; a version 1 one that
// already framed raw bytes is refused, since raw framing would deliver every
// reused task without its records. A session that holds every record keeps
// version 1, and so does the session reporter its version 2.
func TestASessionByReferenceIsNeverFramedRaw(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "the-session-reporter-sends-the-rebuilt-records")
	open := func(t *testing.T, capability Capability, declared bool, checkpoint *state) (*Run, error) {
		t.Helper()
		dir := t.TempDir()
		if declared {
			if err := sessionrefs.Declare(dir, t.TempDir()); err != nil {
				t.Fatal(err)
			}
		}
		if checkpoint != nil {
			data, err := json.Marshal(checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, capability.StateFile), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		r, err := openRun(context.Background(), capability, dir, batchSessionID, "@test/provider")
		if r != nil {
			t.Cleanup(r.release)
		}
		return r, err
	}
	for name, tc := range map[string]struct {
		capability Capability
		declared   bool
		checkpoint *state
		want       int
	}{
		"a new log checkpoint by reference":           {LogReporter, true, nil, resolvedRawCheckpointVersion},
		"an unframed log checkpoint by reference":     {LogReporter, true, &state{Version: rawCheckpointVersion, Provider: "@test/provider", SessionID: batchSessionID}, resolvedRawCheckpointVersion},
		"a new log checkpoint":                        {LogReporter, false, nil, rawCheckpointVersion},
		"a new session checkpoint":                    {SessionReporter, false, nil, viewCheckpointVersion},
		"a new session checkpoint by reference":       {SessionReporter, true, nil, resolvedViewCheckpointVersion},
		"an unframed session checkpoint by reference": {SessionReporter, true, &state{Version: rawCheckpointVersion, Provider: "@test/provider", SessionID: batchSessionID}, resolvedViewCheckpointVersion},
	} {
		t.Run(name, func(t *testing.T) {
			r, err := open(t, tc.capability, tc.declared, tc.checkpoint)
			if err != nil || r.state.Version != tc.want {
				t.Fatalf("openRun = %+v, %v; want version %d", r, err, tc.want)
			}
		})
	}

	framed := &state{Version: rawCheckpointVersion, Provider: "@test/provider", SessionID: batchSessionID, Events: cursor{Offset: 10, Sequence: 1}}
	if _, err := open(t, LogReporter, true, framed); err == nil || !strings.Contains(err.Error(), "frames raw a session that records reused tasks by reference") {
		t.Fatalf("a raw checkpoint that framed a session by reference opened: %v", err)
	}
	if r, err := open(t, LogReporter, false, framed); err != nil || r.state.Version != rawCheckpointVersion {
		t.Fatalf("a raw checkpoint of a session that holds every record = %+v, %v", r, err)
	}
}
