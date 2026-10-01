package spectest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOutcome drives the verdict capture without failing the suite.
type fakeOutcome struct {
	failed, skipped bool
	cleanups        []func()
}

func (f *fakeOutcome) Cleanup(cleanup func()) { f.cleanups = append(f.cleanups, cleanup) }
func (f *fakeOutcome) Failed() bool           { return f.failed }
func (f *fakeOutcome) Skipped() bool          { return f.skipped }
func (f *fakeOutcome) finish() {
	for index := len(f.cleanups) - 1; index >= 0; index-- {
		f.cleanups[index]()
	}
}

func readFragments(t *testing.T, directory string) []Fragment {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read fragments: %v", err)
	}
	fragments := make([]Fragment, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") || !strings.HasSuffix(entry.Name(), ".json") {
			t.Fatalf("unexpected fragment entry %q", entry.Name())
		}
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var fragment Fragment
		if err := json.Unmarshal(data, &fragment); err != nil {
			t.Fatalf("fragment %s is not JSON: %v", entry.Name(), err)
		}
		fragments = append(fragments, fragment)
	}
	return fragments
}

func TestProvesRecordsTheVerdictTestingRecorded(t *testing.T) {
	for name, tc := range map[string]struct {
		failed, skipped bool
		want            string
	}{
		"passed":  {want: "passed"},
		"failed":  {failed: true, want: "failed"},
		"skipped": {skipped: true, want: "skipped"},
	} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			fake := &fakeOutcome{failed: tc.failed, skipped: tc.skipped}
			proves(fake, directory, Fragment{
				Feature: "go/structured-logging", Requirement: "lifecycle",
				Check: "flush-visits-every-sink", File: "/abs/coverage_test.go", Symbol: "TestFlush",
			})
			fake.finish()

			fragments := readFragments(t, directory)
			if len(fragments) != 1 {
				t.Fatalf("fragments = %+v, want one", fragments)
			}
			got := fragments[0]
			if got.Status != tc.want || got.Check != "flush-visits-every-sink" ||
				got.File != "/abs/coverage_test.go" || got.Symbol != "TestFlush" {
				t.Fatalf("fragment = %+v", got)
			}
		})
	}
}

func TestProvesIsInertWithoutTheAdapter(t *testing.T) {
	fake := &fakeOutcome{}
	proves(fake, "", Fragment{Feature: "go/x", Requirement: "r", Check: "c"})
	if len(fake.cleanups) != 0 {
		t.Fatal("Proves registered a cleanup outside a Putnami run")
	}
}

// TestProvesIsConcurrencySafeAndCollapsesDuplicates pins the two properties
// the adapter relies on: parallel writers never interleave bytes (every
// fragment is staged and renamed), and byte-identical observations land under
// one content-derived name instead of multiplying.
func TestProvesIsConcurrencySafeAndCollapsesDuplicates(t *testing.T) {
	directory := t.TempDir()
	var wait sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			fake := &fakeOutcome{}
			proves(fake, directory, Fragment{
				Feature: "go/bounded-parallel-work", Requirement: "concurrency",
				Check: "bounded-inflight", File: "/abs/parallel_test.go", Symbol: "TestBounded",
			})
			fake.finish()
		}()
	}
	wait.Wait()
	fragments := readFragments(t, directory)
	if len(fragments) != 1 {
		t.Fatalf("32 identical observations left %d fragments, want 1", len(fragments))
	}
}

// TestProvesEndToEndThroughARealTestingT is the one real binding: this test
// calls Proves on itself under a fragment directory, and the sibling
// assertion below (source order runs it afterwards) reads the fragment its
// cleanup wrote.
var endToEndDirectory string

func TestProvesEndToEndThroughARealTestingT(t *testing.T) {
	// Not t.TempDir(): its removal cleanup would run AFTER Proves' cleanup
	// (LIFO) but before the sibling test reads the fragment. The sibling owns
	// the removal instead.
	directory, err := os.MkdirTemp("", "spectest-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	endToEndDirectory = directory
	t.Setenv(FragmentDirEnv, endToEndDirectory)
	Proves(t, "go/structured-logging", "lifecycle", "flush-visits-every-sink")
}

func TestProvesEndToEndWroteTheFragment(t *testing.T) {
	if endToEndDirectory == "" {
		t.Skip("the end-to-end binding did not run")
	}
	t.Cleanup(func() { _ = os.RemoveAll(endToEndDirectory) })
	fragments := readFragments(t, endToEndDirectory)
	if len(fragments) != 1 {
		t.Fatalf("fragments = %+v, want the one the previous test's cleanup wrote", fragments)
	}
	got := fragments[0]
	if got.Status != "passed" || got.Symbol != "TestProvesEndToEndThroughARealTestingT" {
		t.Fatalf("fragment = %+v", got)
	}
	if !strings.HasSuffix(got.File, "spectest_test.go") || !filepath.IsAbs(got.File) {
		t.Fatalf("declaration site = %q, want this file's absolute path", got.File)
	}
}

// TestObserveMeasurementPublishesOnlyOnAPassingTest pins the measurement
// half's fail-closed rule: a passing test publishes its aggregate with an
// honest invocation window, a failed or skipped test publishes nothing (an
// absent observation resolves as missing), and the fragment carries no
// verdict of its own.
func TestObserveMeasurementPublishesOnlyOnAPassingTest(t *testing.T) {
	measurement := Measurement{Name: "logger.flush.duration", Aggregation: "p95", Value: 12.5, Unit: "ms"}
	start := mustParseInstant(t, "2026-08-18T10:00:00Z")

	for name, tc := range map[string]struct {
		failed, skipped bool
		wantFragments   int
	}{
		"passed":  {wantFragments: 1},
		"failed":  {failed: true},
		"skipped": {skipped: true},
	} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			fake := &fakeOutcome{failed: tc.failed, skipped: tc.skipped}
			observe(fake, directory, start, Fragment{
				Feature: "gate/example", Requirement: "flush-latency", Check: "flush-benchmark",
				Measurement: &FragmentMeasurement{
					Name: measurement.Name, Aggregation: measurement.Aggregation,
					Value: measurement.Value, Unit: measurement.Unit,
				},
				File: "/abs/bench_test.go", Symbol: "BenchmarkFlush",
			})
			fake.finish()

			fragments := readFragments(t, directory)
			if len(fragments) != tc.wantFragments {
				t.Fatalf("fragments = %+v, want %d", fragments, tc.wantFragments)
			}
			if tc.wantFragments == 0 {
				return
			}
			fragment := fragments[0]
			if fragment.Status != "" {
				t.Fatalf("a measurement fragment carries verdict %q", fragment.Status)
			}
			if fragment.Measurement == nil || fragment.Measurement.Value != 12.5 ||
				fragment.Measurement.Aggregation != "p95" || fragment.Measurement.Unit != "ms" ||
				fragment.Measurement.Name != "logger.flush.duration" {
				t.Fatalf("measurement = %+v", fragment.Measurement)
			}
			if fragment.Window == nil || fragment.Window.Start != "2026-08-18T10:00:00Z" {
				t.Fatalf("window = %+v, want the observation span starting at the call instant", fragment.Window)
			}
			if _, err := time.Parse(time.RFC3339, fragment.Window.End); err != nil {
				t.Fatalf("window end %q is not RFC 3339: %v", fragment.Window.End, err)
			}
		})
	}
}

// TestObserveMeasurementIsInertWithoutAFragmentDirectory mirrors Proves'
// inertness contract: plain `go test` without the adapter registers no
// cleanup work and writes nothing.
func TestObserveMeasurementIsInertWithoutAFragmentDirectory(t *testing.T) {
	fake := &fakeOutcome{}
	observe(fake, "", mustParseInstant(t, "2026-08-18T10:00:00Z"), Fragment{
		Feature: "gate/example", Requirement: "flush-latency", Check: "flush-benchmark",
	})
	if len(fake.cleanups) != 0 {
		t.Fatal("an inert observation registered cleanup work")
	}
}

func mustParseInstant(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
