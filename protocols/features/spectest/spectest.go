// Package spectest binds ordinary Go tests to the executable-spec
// verification wire: a test that protects a declared acceptance
// check calls Proves, a test that measures a declared threshold check calls
// ObserveMeasurement, and the observation is written as a process-local
// fragment the Putnami test adapter merges into the project's
// putnami-feature-verification report artifact.
//
// The package is deliberately inert outside a Putnami-provided fragment
// directory: `go test` without the adapter runs every test exactly as before,
// writes nothing, and can never fail over reporting. It is equally
// deliberately dumb: it states what THIS process observed — feature,
// requirement, check, verdict, declaration site — and nothing else. The
// adapter owns validation, project attribution, and merging; core recomputes
// every verdict against the authored criterion; and no call here can make a
// gate green that the test's own outcome does not support, because the
// fragment carries the outcome testing recorded, captured in t.Cleanup after
// the test body finished.
//
// It imports the standard library only, so a zero-dependency framework module
// stays zero-dependency at runtime; the wire types stay owned by
// go.putnami.dev/protocol/features, and the adapter's conformance tests pin
// this package's fragments against that wire.
package spectest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// FragmentDirEnv names the directory the Putnami test adapter provides for
// this process's fragments. The adapter mints a fresh directory per run, which
// also keys go's own test cache: a test that called Proves consulted this
// variable, so a new run re-executes it instead of serving a cached verdict
// whose fragment was never written.
const FragmentDirEnv = "PUTNAMI_SPEC_FRAGMENTS"

// Fragment is one process-local observation: the declared check a test
// protects, and either the verdict testing recorded for it (an acceptance
// check) or the aggregate it measured (a threshold check) — never both, the
// same exclusivity the verification wire enforces so a producer cannot state
// a numeric objective and then declare its own result for it. File is the
// absolute declaration site; the adapter resolves it to a project-relative
// provenance path and refuses one outside the reporting project.
type Fragment struct {
	Feature     string `json:"feature"`
	Requirement string `json:"requirement"`
	Check       string `json:"check"`
	// Status is passed, failed, or skipped — the acceptance vocabulary. Empty
	// on a measurement fragment.
	Status string `json:"status,omitempty"`
	// Measurement is the observed aggregate of a threshold check, with the
	// Window it covers and, for rolling objectives, the Environment it was
	// taken in. Core recomputes the verdict against the authored criterion;
	// no verdict travels here.
	Measurement *FragmentMeasurement `json:"measurement,omitempty"`
	Window      *FragmentWindow      `json:"window,omitempty"`
	Environment string               `json:"environment,omitempty"`
	File        string               `json:"file"`
	Symbol      string               `json:"symbol"`
}

// FragmentMeasurement mirrors the verification wire's observed aggregate:
// the bounded semantic metric name, the reduction that produced Value, and
// its unit.
type FragmentMeasurement struct {
	Name        string  `json:"name"`
	Aggregation string  `json:"aggregation"`
	Value       float64 `json:"value"`
	Unit        string  `json:"unit"`
}

// FragmentWindow is the closed RFC 3339 period a measurement covers.
type FragmentWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// Proves declares that the calling test is the protecting check of one
// feature requirement. It records the declaration site now and the verdict in
// t.Cleanup, so a failing or skipped test reports itself honestly; the
// standard test verdict is never altered. Concurrent tests may call it
// freely: every fragment is written atomically under a content-derived name,
// so identical observations collapse and nothing interleaves.
func Proves(t *testing.T, feature, requirement, check string) {
	t.Helper()
	_, file, _, _ := runtime.Caller(1)
	proves(t, fragmentDirectory(), Fragment{
		Feature:     feature,
		Requirement: requirement,
		Check:       check,
		File:        file,
		Symbol:      rootTestName(t.Name()),
	})
}

// Measurement is the observed aggregate a measuring test publishes for a
// declared threshold check: the bounded semantic metric name,
// the reduction that produced Value (value, count, sum, avg, min, max, p50,
// p95, p99, ratio), and its unit. The authored target lives in
// putnami.features.json only; nothing here can state a verdict.
type Measurement struct {
	Name        string
	Aggregation string
	Value       float64
	Unit        string
}

// ObserveMeasurement declares that the calling test measured one declared
// threshold check and publishes the observed aggregate in t.Cleanup. The
// observed window is the honest invocation span — from this call to the
// cleanup write — which is what an invocation-window criterion evaluates. A
// test that failed or was skipped publishes NOTHING: its measurement was
// taken under conditions the test itself rejected, and an absent observation
// resolves as missing, the fail-closed direction. The standard test verdict
// is never altered.
//
// Rolling objectives (a production SLO over a declared window and
// environment) are not measured by unit tests; their observations come from
// delivery/runtime producers that state the real observed window and
// environment on the report wire directly.
func ObserveMeasurement(t *testing.T, feature, requirement, check string, measurement Measurement) {
	t.Helper()
	_, file, _, _ := runtime.Caller(1)
	// Measurement and FragmentMeasurement are field-identical on purpose; the
	// type conversion is the compiler-checked copy, so a field added to one
	// without the other refuses to build instead of silently dropping data.
	measured := FragmentMeasurement(measurement)
	observe(t, fragmentDirectory(), time.Now().UTC(), Fragment{
		Feature:     feature,
		Requirement: requirement,
		Check:       check,
		Measurement: &measured,
		File:        file,
		Symbol:      rootTestName(t.Name()),
	})
}

// outcome is the slice of testing.T Proves reads, split out so the verdict
// capture is testable without failing the suite that tests it.
type outcome interface {
	Cleanup(func())
	Failed() bool
	Skipped() bool
}

func observe(t outcome, directory string, start time.Time, fragment Fragment) {
	if directory == "" {
		return
	}
	t.Cleanup(func() {
		if t.Failed() || t.Skipped() {
			return
		}
		fragment.Window = &FragmentWindow{
			Start: start.Format(time.RFC3339),
			End:   time.Now().UTC().Format(time.RFC3339),
		}
		writeFragment(directory, fragment)
	})
}

func proves(t outcome, directory string, fragment Fragment) {
	if directory == "" {
		return
	}
	t.Cleanup(func() {
		switch {
		case t.Failed():
			fragment.Status = "failed"
		case t.Skipped():
			fragment.Status = "skipped"
		default:
			fragment.Status = "passed"
		}
		writeFragment(directory, fragment)
	})
}

// writeFragment publishes one fragment atomically. Failures are deliberately
// silent: reporting is the adapter's concern, and a full disk must not turn a
// green test red from inside its cleanup.
func writeFragment(directory string, fragment Fragment) {
	encoded, err := json.Marshal(fragment)
	if err != nil {
		return
	}
	sum := sha256.Sum256(encoded)
	final := filepath.Join(directory, hex.EncodeToString(sum[:16])+".json")
	staging, err := os.CreateTemp(directory, ".fragment-*.tmp")
	if err != nil {
		return
	}
	name := staging.Name()
	_, writeErr := staging.Write(encoded)
	closeErr := staging.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(name)
		return
	}
	if err := os.Rename(name, final); err != nil {
		_ = os.Remove(name)
	}
}

func fragmentDirectory() string {
	return os.Getenv(FragmentDirEnv)
}

// rootTestName reduces a subtest path to the Go test symbol that declares it:
// TestLogger/flush names TestLogger, which is what a reader can find in the
// file the fragment points at.
func rootTestName(name string) string {
	if index := strings.IndexByte(name, '/'); index >= 0 {
		return name[:index]
	}
	return name
}
