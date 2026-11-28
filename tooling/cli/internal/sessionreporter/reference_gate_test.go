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
	"go.putnami.dev/tooling/cli/internal/output"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
)

// TestASessionByReferenceStaysOnFramesForAReceiverOfPackagesOnly delivers a
// session that records a reused task by reference to a receiver that offers
// packages but not rebuilt records in them: the checkpoint stays at version
// 4, sends no put, and frames the view with the records the entry holds in
// place of the reference.
func TestASessionByReferenceStaysOnFramesForAReceiverOfPackagesOnly(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "rebuilt-packages-need-a-receiver-that-reads-them")
	entries := referenceStore(t)
	a := "/a:test"
	records := []string{startRecord(a), referencedEnd(a), sessionEndRecord}
	events := make([][]byte, len(records))
	for i, record := range records {
		events[i] = []byte(record)
	}
	h := newPlanHarness(t, planOf(10), events...)
	h.run.entries = entries
	h.run.state.Version = SessionReporter.checkpointVersionOf(true)
	rec := serveRebuiltPackages(t, h.run, false, acceptAll)
	if err := finishWith(t, h, []byte(`{"protocolVersion":2}`)); err != nil {
		t.Fatalf("Finish = %v", err)
	}
	if s := h.checkpoint(); !s.Complete || s.Version != resolvedViewCheckpointVersion || s.Packages != nil {
		t.Fatalf("checkpoint = %+v, want a complete version 4 session on frames", s)
	}
	_, puts, _, _ := rec.snapshot()
	if len(puts) != 0 {
		t.Fatalf("the engine sent %d puts to a receiver that takes no rebuilt records", len(puts))
	}
	rec.mu.Lock()
	chunks := append([]protocolcli.SessionReportingChunk(nil), rec.chunks...)
	rec.mu.Unlock()
	want := append(append(stream(records[0]), resolvedBytes(t, entries, records[1])...), stream(records[2])...)
	if got, final := reassemble(t, chunks, sessionstream.EventsFile); !final || !bytes.Equal(got, want) {
		t.Fatalf("the receiver read (final %v)\n%s\nwant\n%s", final, got, want)
	}
}

// TestAReferenceThisCLICannotRebuildStopsDelivery delivers a session whose
// reused task was recorded by another projection of the records: on frames
// and on packages alike, delivery fails with a diagnostic that names the
// projection, and no byte of the task leaves, so a CLI that rebuilds that
// projection delivers the session from where it stopped.
func TestAReferenceThisCLICannotRebuildStopsDelivery(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "a-reference-this-cli-cannot-rebuild-is-named")
	entries := referenceStore(t)
	a := "/a:test"
	other := referencedEndOf(a, output.CachedRecordsProjection+1)
	says := []string{"reporting cannot rebuild a reused task's records", "records projection 2"}

	t.Run("frames", func(t *testing.T) {
		h := newReferenceHarness(t, SessionReporter, entries)
		if err := os.WriteFile(filepath.Join(h.run.dir, sessionFile), []byte(`{"protocolVersion":2}`), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, record := range []string{startRecord(a), other, sessionEndRecord} {
			h.append(record)
		}
		if err := h.log.Close(); err != nil {
			t.Fatal(err)
		}
		close(h.run.finish)
		var err error
		select {
		case err = <-h.done:
		case <-time.After(time.Minute):
			t.Fatal("delivery never stopped")
		}
		for _, say := range says {
			if err == nil || !strings.Contains(err.Error(), say) {
				t.Fatalf("delivery stopped with %v, want a diagnostic that says %q", err, say)
			}
		}
		for _, chunk := range h.sent() {
			if chunk.Artifact == sessionstream.EventsFile && len(chunk.Data) > 0 {
				t.Fatalf("an events.jsonl frame left: %s", chunk.Data)
			}
		}
		if h.run.state.Events != (cursor{}) || h.run.state.EventsRaw != 0 {
			t.Fatalf("checkpoint acknowledges %+v at raw offset %d, want nothing of the stream", h.run.state.Events, h.run.state.EventsRaw)
		}
	})

	t.Run("packages", func(t *testing.T) {
		h := newPlanHarness(t, planOf(10), []byte(startRecord(a)), []byte(other), []byte(sessionEndRecord))
		h.run.entries = entries
		h.run.state.Version = SessionReporter.checkpointVersionOf(true)
		rec := serveRebuiltPackages(t, h.run, true, acceptAll)
		err := finishWith(t, h, []byte(`{"protocolVersion":2}`))
		for _, say := range says {
			if err == nil || !strings.Contains(err.Error(), say) {
				t.Fatalf("Finish = %v, want a diagnostic that says %q", err, say)
			}
		}
		_, puts, manifests, _ := rec.snapshot()
		for _, data := range taskPuts(puts) {
			if p := decodeTask(t, data); *p.header.Key == a {
				t.Fatalf("the task a CLI cannot rebuild left in a package: %s", data)
			}
		}
		if s := h.checkpoint(); s.Complete || len(manifests) != 0 {
			t.Fatalf("checkpoint = %+v after %d manifests, want an incomplete session", s, len(manifests))
		}
	})
}
