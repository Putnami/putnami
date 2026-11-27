package fileindex

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withClock makes every record opened in the test read clock as its
// filesystem clock, on the device of the test's directory, and treat the
// directory as local.
func withRecordClock(t *testing.T, clock *int64) {
	t.Helper()
	previousClock, previousLocal := recordClock, recordLocal
	recordClock = func(dir string) (uint64, int64, error) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return 0, 0, err
		}
		st, ok := LstatAny(dir)
		if !ok {
			t.Fatalf("stat %s", dir)
		}
		return st.Dev, *clock, nil
	}
	recordLocal = func(string) bool { return true }
	t.Cleanup(func() { recordClock, recordLocal = previousClock, previousLocal })
}

// partOf stats path for a witness.
func recordPartOf(t *testing.T, path string) RecordPart {
	t.Helper()
	st, ok := LstatAny(path)
	if !ok {
		t.Fatalf("stat %s", path)
	}
	return RecordPart{Path: path, Stat: st}
}

func recordHoldsNow(parts []RecordPart) bool {
	for _, p := range parts {
		st, ok := LstatAny(p.Path)
		if !ok || st != p.Stat {
			return false
		}
	}
	return true
}

func recordFixture(t *testing.T) (root, recordPath, recordFile string) {
	t.Helper()
	root = t.TempDir()
	recordFile = filepath.Join(root, "a.txt")
	if err := os.WriteFile(recordFile, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, RecordPath(root), recordFile
}

func TestARecordedValueIsAnsweredWhileItsPartsHold(t *testing.T) {
	clock := time.Now().Add(time.Hour).UnixNano()
	withRecordClock(t, &clock)
	_, path, recordFile := recordFixture(t)

	r := OpenRecord(path, "id")
	if !r.Put("digests", "k", []byte("v"), []RecordPart{recordPartOf(t, recordFile)}) {
		t.Fatal("a part older than the clock by the margin was refused")
	}
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}

	next := OpenRecord(path, "id")
	value, parts, ok := next.Answer("digests", "k", recordHoldsNow)
	if !ok || string(value) != "v" || len(parts) != 1 || parts[0].Path != recordFile {
		t.Fatalf("Answer = %q, %v, %v; want the recorded value and part", value, parts, ok)
	}
	if _, _, ok := next.Answer("other", "k", recordHoldsNow); ok {
		t.Fatal("a table answered a recordKey another table recorded")
	}

	// Another size: a rewrite within one tick of a coarse file system clock
	// keeps the times, which the margin covers outside this test's clock.
	if err := os.WriteFile(recordFile, []byte("moved"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := OpenRecord(path, "id").Answer("digests", "k", recordHoldsNow); ok {
		t.Fatal("a value whose part moved was answered")
	}
}

func TestAPartWrittenWithinTheMarginIsNotRecorded(t *testing.T) {
	_, path, recordFile := recordFixture(t)
	st, _ := LstatAny(recordFile)
	clock := st.ChangeNano + int64(Margin) // not older by more than the margin
	withRecordClock(t, &clock)

	r := OpenRecord(path, "id")
	if r.Put("digests", "k", []byte("v"), []RecordPart{recordPartOf(t, recordFile)}) {
		t.Fatal("a part written within the margin was recorded")
	}
	if r.Put("digests", "k", []byte("v"), nil) {
		t.Fatal("a value with no part was recorded")
	}
	if r.Put("digests", "k", []byte("v"), []RecordPart{{Path: "a.txt", Stat: st}}) {
		t.Fatal("a relative part was recorded")
	}
}

func TestARecordOfAnotherProgramOrDamagedLoadsEmpty(t *testing.T) {
	clock := time.Now().Add(time.Hour).UnixNano()
	withRecordClock(t, &clock)
	_, path, recordFile := recordFixture(t)
	r := OpenRecord(path, "id")
	r.Put("digests", "k", []byte("v"), []RecordPart{recordPartOf(t, recordFile)})
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := OpenRecord(path, "other").Answer("digests", "k", recordHoldsNow); ok {
		t.Fatal("a record another program wrote answered")
	}
	if _, _, ok := OpenRecord(path, "").Answer("digests", "k", recordHoldsNow); ok {
		t.Fatal("a process with no identity answered")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, damage := range []func([]byte) []byte{
		func(b []byte) []byte { b[len(b)/2] ^= 0xff; return b },
		func(b []byte) []byte { return b[:len(b)-1] },
		func(b []byte) []byte { return append(b, 0) },
	} {
		if err := os.WriteFile(path, damage(append([]byte(nil), data...)), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, ok := OpenRecord(path, "id").Answer("digests", "k", recordHoldsNow); ok {
			t.Fatal("a damaged record answered")
		}
	}
}

func TestARecordAheadOfTheClockDiscardsItself(t *testing.T) {
	clock := time.Now().Add(time.Hour).UnixNano()
	withRecordClock(t, &clock)
	_, path, recordFile := recordFixture(t)
	r := OpenRecord(path, "id")
	r.Put("digests", "k", []byte("v"), []RecordPart{recordPartOf(t, recordFile)})
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	clock -= int64(time.Minute)
	if _, _, ok := OpenRecord(path, "id").Answer("digests", "k", recordHoldsNow); ok {
		t.Fatal("a record whose stamp lies ahead of the clock answered")
	}
}

func TestAClockThatWentBackDuringTheRunRecordsNothing(t *testing.T) {
	clock := time.Now().Add(time.Hour).UnixNano()
	withRecordClock(t, &clock)
	_, path, recordFile := recordFixture(t)
	r := OpenRecord(path, "id")
	r.Put("digests", "k", []byte("v"), []RecordPart{recordPartOf(t, recordFile)})
	clock -= int64(time.Minute)
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := OpenRecord(path, "id").Answer("digests", "k", recordHoldsNow); ok {
		t.Fatal("a value recorded before the clock went back was kept")
	}
}

func TestConcurrentRunsMergeAndDropWhatTheyFoundOutOfDate(t *testing.T) {
	clock := time.Now().Add(time.Hour).UnixNano()
	withRecordClock(t, &clock)
	root, path, recordFile := recordFixture(t)
	other := filepath.Join(root, "b.txt")
	if err := os.WriteFile(other, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}

	first, second := OpenRecord(path, "id"), OpenRecord(path, "id")
	first.Put("digests", "a", []byte("1"), []RecordPart{recordPartOf(t, recordFile)})
	second.Put("digests", "b", []byte("2"), []RecordPart{recordPartOf(t, other)})
	if err := first.Save(); err != nil {
		t.Fatal(err)
	}
	if err := second.Save(); err != nil {
		t.Fatal(err)
	}
	merged := OpenRecord(path, "id")
	for k, want := range map[string]string{"a": "1", "b": "2"} {
		if value, _, ok := merged.Answer("digests", k, recordHoldsNow); !ok || string(value) != want {
			t.Fatalf("after two runs merged, %s = %q, %v; want %q", k, value, ok, want)
		}
	}

	if err := os.WriteFile(recordFile, []byte("moved"), 0o644); err != nil {
		t.Fatal(err)
	}
	finding := OpenRecord(path, "id")
	if _, _, ok := finding.Answer("digests", "a", recordHoldsNow); ok {
		t.Fatal("a value whose part moved was answered")
	}
	if err := finding.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordFile, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	after := OpenRecord(path, "id")
	if _, _, ok := after.Answer("digests", "a", func([]RecordPart) bool { return true }); ok {
		t.Fatal("a value a run found out of date stayed in the file")
	}
	if _, _, ok := after.Answer("digests", "b", recordHoldsNow); !ok {
		t.Fatal("a value no run found out of date was dropped")
	}
}

func TestAnUnchangedRunWritesNothing(t *testing.T) {
	clock := time.Now().Add(time.Hour).UnixNano()
	withRecordClock(t, &clock)
	_, path, recordFile := recordFixture(t)
	r := OpenRecord(path, "id")
	r.Put("digests", "k", []byte("v"), []RecordPart{recordPartOf(t, recordFile)})
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	before, _ := LstatAny(path)
	confirming := OpenRecord(path, "id")
	if _, _, ok := confirming.Answer("digests", "k", recordHoldsNow); !ok {
		t.Fatal("the recorded value was not answered")
	}
	if err := confirming.Save(); err != nil {
		t.Fatal(err)
	}
	if after, _ := LstatAny(path); after != before {
		t.Fatal("a run that only confirmed entries rewrote the record")
	}
}

func TestAReadOnlyRecordAnswersAndWritesNothing(t *testing.T) {
	clock := time.Now().Add(time.Hour).UnixNano()
	withRecordClock(t, &clock)
	_, path, recordFile := recordFixture(t)
	r := OpenRecord(path, "id")
	r.Put("digests", "k", []byte("v"), []RecordPart{recordPartOf(t, recordFile)})
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	readOnly := OpenRecordReadOnly(path, "id", clock)
	if _, _, ok := readOnly.Answer("digests", "k", recordHoldsNow); !ok {
		t.Fatal("a read-only record did not answer")
	}
	if readOnly.Put("digests", "other", []byte("v"), []RecordPart{recordPartOf(t, recordFile)}) {
		t.Fatal("a read-only record recorded")
	}
}

func TestAnAbsentPartHoldsWhileNothingIsThere(t *testing.T) {
	clock := time.Now().Add(time.Hour).UnixNano()
	withRecordClock(t, &clock)
	root, path, file := recordFixture(t)
	missing := filepath.Join(root, "missing", "x.txt")
	r := OpenRecord(path, "id")
	if !r.Put("digests", "k", []byte("v"), []RecordPart{recordPartOf(t, file), AbsentPart(missing)}) {
		t.Fatal("a value with an absent part was refused")
	}
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	next := OpenRecord(path, "id")
	if _, _, ok := next.Answer("digests", "k", next.Hold); !ok {
		t.Fatal("an absent part that stayed absent did not hold")
	}
	if err := os.MkdirAll(filepath.Dir(missing), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(missing, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	after := OpenRecord(path, "id")
	if _, _, ok := after.Answer("digests", "k", after.Hold); ok {
		t.Fatal("an absent part held once its path existed")
	}
}

// A stat Hold takes while a write lands and Forget runs is stored after the
// Forget: the run keeps the stat from before the write.
func TestAStatTakenAcrossAForgetIsNotHeld(t *testing.T) {
	clock := time.Now().Add(time.Hour).UnixNano()
	withRecordClock(t, &clock)
	root := t.TempDir()
	file := filepath.Join(root, "a.txt")
	if err := os.WriteFile(file, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	part := recordPartOf(t, file)
	r := OpenRecord(RecordPath(root), "id")
	// Prime stats so Forget does not take its empty-map early return.
	r.Hold([]RecordPart{recordPartOf(t, root)})

	statTaken, release := make(chan struct{}), make(chan struct{})
	saved := recordLstat
	recordLstat = func(p string) (Stat, bool) {
		st, ok := saved(p)
		if p == file {
			close(statTaken)
			<-release
		}
		return st, ok
	}
	t.Cleanup(func() { recordLstat = saved })

	done := make(chan bool)
	go func() { done <- r.Hold([]RecordPart{part}) }()
	<-statTaken
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(file, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Forget([]string{file})
	close(release)
	<-done
	recordLstat = saved
	if r.Hold([]RecordPart{part}) {
		t.Errorf("after a write and its Forget, Hold still answers the pre-write stat")
	}
}

func TestForgetAllDropsEveryHeldStat(t *testing.T) {
	clock := time.Now().Add(time.Hour).UnixNano()
	withRecordClock(t, &clock)
	root, path, file := recordFixture(t)
	part := recordPartOf(t, file)
	r := OpenRecord(path, "id")
	if !r.Hold([]RecordPart{part}) {
		t.Fatal("a part that holds did not")
	}
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(file, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !r.Hold([]RecordPart{part}) {
		t.Fatal("a held stat was taken again without a Forget")
	}
	r.ForgetAll()
	if r.Hold([]RecordPart{part}) {
		t.Fatal("ForgetAll kept the stat from before the write")
	}
	_ = root
}

// TestADirectoryStatMovesWithItsListing checks, on each filesystem the
// machine offers here (the test's temporary directory, and tmpfs at /dev/shm
// where it exists), what the input record relies on (tooling/cli ADR 0113
// §2): a directory's stat moves when an entry is created, removed, renamed in
// or out, replaced by a rename or linked.
func TestADirectoryStatMovesWithItsListing(t *testing.T) {
	roots := []string{t.TempDir()}
	if info, err := os.Stat("/dev/shm"); err == nil && info.IsDir() {
		if dir, err := os.MkdirTemp("/dev/shm", "record-test-"); err == nil {
			t.Cleanup(func() { os.RemoveAll(dir) })
			roots = append(roots, dir)
		}
	}
	for _, root := range roots {
		if !recordLocal(root) {
			continue
		}
		dir := filepath.Join(root, "d")
		other := filepath.Join(root, "other")
		for _, p := range []string{dir, other} {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte("f"), 0o644); err != nil {
			t.Fatal(err)
		}
		steps := []struct {
			name  string
			write func() error
		}{
			{"create a file", func() error { return os.WriteFile(filepath.Join(dir, "n"), nil, 0o644) }},
			{"remove a file", func() error { return os.Remove(filepath.Join(dir, "n")) }},
			{"create a directory", func() error { return os.Mkdir(filepath.Join(dir, "s"), 0o755) }},
			{"remove a directory", func() error { return os.Remove(filepath.Join(dir, "s")) }},
			{"rename within", func() error { return os.Rename(filepath.Join(dir, "f"), filepath.Join(dir, "g")) }},
			{"rename out", func() error { return os.Rename(filepath.Join(dir, "g"), filepath.Join(other, "g")) }},
			{"rename in", func() error { return os.Rename(filepath.Join(other, "g"), filepath.Join(dir, "f")) }},
			{"replace by a rename", func() error {
				if err := os.WriteFile(filepath.Join(other, "t"), []byte("t"), 0o644); err != nil {
					return err
				}
				return os.Rename(filepath.Join(other, "t"), filepath.Join(dir, "f"))
			}},
			{"create a link", func() error { return os.Symlink("f", filepath.Join(dir, "l")) }},
		}
		for _, step := range steps {
			before, ok := LstatAny(dir)
			if !ok {
				t.Skip("this platform's stat is not decoded")
			}
			time.Sleep(15 * time.Millisecond)
			if err := step.write(); err != nil {
				t.Fatal(err)
			}
			if after, _ := LstatAny(dir); after == before {
				t.Errorf("%s: %s left the directory's stat unchanged", root, step.name)
			}
		}
	}
}

func TestASaveKeepsTheRecordWithinItsCaps(t *testing.T) {
	clock := time.Now().Add(time.Hour).UnixNano()
	withRecordClock(t, &clock)
	_, path, file := recordFixture(t)
	savedEntries, savedParts := recordMaxEntries, recordMaxParts
	recordMaxEntries, recordMaxParts = 3, 100
	t.Cleanup(func() { recordMaxEntries, recordMaxParts = savedEntries, savedParts })
	r := OpenRecord(path, "id")
	for i := range 5 {
		r.Put("digests", fmt.Sprint("k", i), []byte("v"), []RecordPart{recordPartOf(t, file)})
	}
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	disk, err := decodeRecord(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(disk.entries) != 3 {
		t.Fatalf("the record holds %d entries past its cap of 3", len(disk.entries))
	}
}

func TestADiscardIsReportedOnceAndANegativeRateSamplesNothing(t *testing.T) {
	clock := time.Now().Add(time.Hour).UnixNano()
	withRecordClock(t, &clock)
	_, path, _ := recordFixture(t)
	r := OpenRecord(path, "id")
	r.SetSampleRate(-1)
	for range 100 {
		if r.Sample() {
			t.Fatal("a negative rate sampled an answer")
		}
	}
	var reasons []string
	r.OnDiscard(func(reason string) { reasons = append(reasons, reason) })
	r.Discard("first")
	r.Discard("second")
	if len(reasons) != 1 || reasons[0] != "first" || r.Discarded() != "first" {
		t.Fatalf("reported %q, discarded %q: want the first reason, once", reasons, r.Discarded())
	}
}

func TestLinkHopsNamesEachLinkTheResolutionMeets(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"real/sub", "other"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{"a": "real", "real/up": "../other", "chain": "a"} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		dir  string
		hops []string
		ok   bool
	}{
		{"real/sub", nil, true},
		{"a/sub", []string{"a"}, true},
		{"chain/sub", []string{"chain", "a"}, true},
		{"a/up", []string{"a", "real/up"}, true},
		{"a/missing", nil, false},
	}
	for _, c := range cases {
		hops, ok := LinkHops(filepath.Join(root, c.dir))
		var want []string
		for _, hop := range c.hops {
			want = append(want, filepath.Join(root, hop))
		}
		if ok != c.ok || fmt.Sprint(hops) != fmt.Sprint(want) {
			t.Errorf("LinkHops(%s) = %v, %v; want %v, %v", c.dir, hops, ok, want, c.ok)
		}
	}
	loop := filepath.Join(root, "loop")
	if err := os.Symlink("loop", loop); err != nil {
		t.Fatal(err)
	}
	if _, ok := LinkHops(loop); ok {
		t.Error("a link to itself resolved")
	}
}
