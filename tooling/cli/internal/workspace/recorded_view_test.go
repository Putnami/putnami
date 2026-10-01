package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wsproto "go.putnami.dev/protocol/workspace"
)

// The local model of `cli.workspace-probe-view.v1`: what a read of the recorded
// index reports about its own age.

func recordedFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(wsproto.WorkspaceConfigFilename, `{"name":"recorded","includes":["packages/app"]}`)
	write("packages/app/"+wsproto.ConfigFilename, `{"name":"app","type":"application"}`)
	InvalidateLoadCache(dir)
	return dir
}

// recordAt writes the index as if the provider answers had been observed at
// observedAt, which is what lets a freshness bound be exercised by moving a
// clock instead of waiting a day.
func recordAt(t *testing.T, dir string, observedAt time.Time) {
	t.Helper()
	ws, err := Load(dir)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	snapshot := newSnapshotAt(ws, ws.ProbeDigest(), nil, observedAt)
	if err := WriteSnapshot(dir, snapshot, SnapshotWritePolicy{}); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	InvalidateLoadCache(dir)
}

// TestRecordedViewReportsAbsenceRatherThanAnEmptyAnswer is the whole point of
// the contract's `onMissing: fail-closed`. A workspace nobody has probed has no
// copy of the provider answers, and a reader must be able to tell that from "the
// providers answered nothing".
func TestRecordedViewReportsAbsenceRatherThanAnEmptyAnswer(t *testing.T) {
	dir := recordedFixture(t)
	view := RecordedIndexView(dir, time.Now())
	if view.Freshness != RecordedAbsent || view.Usable() {
		t.Fatalf("view = %+v, want an unusable absent view", view)
	}
	if view.Message == "" || !containsAll(view.Message, "projects sync", WorkspaceIndexFilename) {
		t.Errorf("message = %q, want the file and the command that rebuilds it", view.Message)
	}
	if view.ObservedAt != "" || view.ProbeDigest != "" {
		t.Errorf("view = %+v, want no provenance for a copy that does not exist", view)
	}
}

// TestRecordedViewStampsFreshnessAgainstTheDeclaredBound walks the two states a
// present copy can be in. `onStale: use-stale` is why the stale one stays
// usable: the answer is served and says it is old, rather than being withheld.
func TestRecordedViewStampsFreshnessAgainstTheDeclaredBound(t *testing.T) {
	dir := recordedFixture(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	recordAt(t, dir, now.Add(-time.Hour))
	fresh := RecordedIndexView(dir, now)
	if fresh.Freshness != RecordedFresh || !fresh.Usable() {
		t.Fatalf("view = %+v, want a fresh, usable copy", fresh)
	}
	if fresh.ObservedAt == "" || fresh.ProbeDigest == "" {
		t.Errorf("view = %+v, want the observation time and the provenance it carries", fresh)
	}
	if fresh.AgeSeconds != int64(time.Hour/time.Second) {
		t.Errorf("age = %ds, want one hour", fresh.AgeSeconds)
	}

	recordAt(t, dir, now.Add(-RecordedMaxStaleness-time.Minute))
	stale := RecordedIndexView(dir, now)
	if stale.Freshness != RecordedStale {
		t.Fatalf("view = %+v, want stale past the declared bound", stale)
	}
	if !stale.Usable() {
		t.Error("a stale copy is unusable; the contract declares use-stale, so it is answered from and marked")
	}
	if !containsAll(stale.Message, "projects sync") {
		t.Errorf("message = %q, want the refresh command named", stale.Message)
	}

	// Exactly at the bound is still fresh: the bound is the maximum age a copy
	// may have, not the age at which it expires.
	recordAt(t, dir, now.Add(-RecordedMaxStaleness))
	if got := RecordedIndexView(dir, now).Freshness; got != RecordedFresh {
		t.Errorf("freshness at exactly the bound = %q, want %q", got, RecordedFresh)
	}
}

// TestRecordedViewRefusesACopyThatCannotStateItsAge pins the two shapes that are
// present on disk and still unusable: an index from a superseded format, and one
// whose observation time does not parse. Guessing an age for either would invent
// the fact the contract requires the copy to state.
func TestRecordedViewRefusesACopyThatCannotStateItsAge(t *testing.T) {
	dir := recordedFixture(t)
	now := time.Now()
	recordAt(t, dir, now)

	rewrite := func(mutate func(*Snapshot)) {
		snapshot, err := LoadSnapshot(dir)
		if err != nil || snapshot == nil {
			t.Fatalf("load snapshot: %v", err)
		}
		mutate(snapshot)
		if err := WriteSnapshot(dir, snapshot, SnapshotWritePolicy{}); err != nil {
			t.Fatal(err)
		}
	}

	rewrite(func(s *Snapshot) { s.Version = snapshotFormatVersion - 1 })
	if view := RecordedIndexView(dir, now); view.Freshness != RecordedAbsent {
		t.Errorf("view = %+v, want absent for a superseded index format", view)
	}

	rewrite(func(s *Snapshot) { s.Version = snapshotFormatVersion; s.ObservedAt = "" })
	if view := RecordedIndexView(dir, now); view.Freshness != RecordedAbsent {
		t.Errorf("view = %+v, want absent for an index with no observation time", view)
	}

	rewrite(func(s *Snapshot) { s.ObservedAt = "yesterday" })
	if view := RecordedIndexView(dir, now); view.Freshness != RecordedAbsent {
		t.Errorf("view = %+v, want absent for an unreadable observation time", view)
	}
}

// TestRecordedViewTreatsAFutureObservationAsAtTheBound pins the clock-skew
// direction. A copy observed in the future is a clock that moved, and reporting
// it as fresh would let a skewed builder serve an arbitrarily old answer.
func TestRecordedViewTreatsAFutureObservationAsAtTheBound(t *testing.T) {
	dir := recordedFixture(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	recordAt(t, dir, now.Add(72*time.Hour))
	view := RecordedIndexView(dir, now)
	if view.Freshness != RecordedFresh || view.AgeSeconds != 0 {
		t.Errorf("view = %+v, want a zero-age fresh copy rather than a negative age", view)
	}
}

// TestRecordedSnapshotCarriesTheObservationTime pins the write side: the field
// the projection declares is persisted, in UTC, by the ordinary refresh path.
func TestRecordedSnapshotCarriesTheObservationTime(t *testing.T) {
	dir := recordedFixture(t)
	ws, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RefreshSnapshot(ws, SnapshotWritePolicy{}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadSnapshot(dir)
	if err != nil || snapshot == nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if snapshot.Version != snapshotFormatVersion {
		t.Errorf("version = %d, want %d", snapshot.Version, snapshotFormatVersion)
	}
	observed, err := time.Parse(time.RFC3339Nano, snapshot.ObservedAt)
	if err != nil {
		t.Fatalf("observedAt = %q, want RFC 3339: %v", snapshot.ObservedAt, err)
	}
	if observed.Location() != time.UTC {
		t.Errorf("observedAt = %q, want UTC so two machines record comparable times", snapshot.ObservedAt)
	}
}

func containsAll(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(haystack, needle) {
			return false
		}
	}
	return true
}
