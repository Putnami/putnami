package specreport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/jsonl"
)

// captureEvents runs fn with os.Stdout redirected and returns the JSONL
// events it emitted. The reader drains concurrently so a chatty merge cannot
// deadlock on the pipe buffer.
func captureEvents(t *testing.T, fn func(emit *jsonl.Emitter)) []map[string]any {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan []byte, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		drained <- buf.Bytes()
	}()

	original := os.Stdout
	os.Stdout = w
	fn(jsonl.New())
	os.Stdout = original
	_ = w.Close()
	raw := <-drained
	_ = r.Close()

	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("unparseable event %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

func eventsMention(events []map[string]any, substring string) bool {
	for _, event := range events {
		if strings.Contains(fmt.Sprint(event), substring) {
			return true
		}
	}
	return false
}

func writeSpecFragment(t *testing.T, directory string, fragment spectest.Fragment) {
	t.Helper()
	encoded, err := json.Marshal(fragment)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.ReplaceAll(fragment.Check+"-"+fragment.Status+"-"+filepath.Base(fragment.File), "/", "_")
	if err := os.WriteFile(filepath.Join(directory, name+".json"), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
}

func specProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "logger_test.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestProjectReportResolvesProvenanceAndFailsClosed pins the merge's trust
// rules: provenance becomes project-relative, a declaration outside the
// project is set aside as foreign, several observations of one check reduce
// to a single wire observation with the failure winning (the wire admits one
// observation per check, and an active contradiction must reach core), and
// an observation the wire refuses is dropped alone with a warning.
func TestProjectReportResolvesProvenanceAndFailsClosed(t *testing.T) {
	root := specProject(t)
	inside := filepath.Join(root, "logger_test.go")
	fragments := []spectest.Fragment{
		{Feature: "go/structured-logging", Requirement: "lifecycle", Check: "flush", Status: "passed", File: inside, Symbol: "TestFlush"},
		{Feature: "go/structured-logging", Requirement: "lifecycle", Check: "flush", Status: "passed", File: inside, Symbol: "TestFlush"},
		{Feature: "go/structured-logging", Requirement: "lifecycle", Check: "flush", Status: "failed", File: inside, Symbol: "TestFlushTwin"},
		{Feature: "go/structured-logging", Requirement: "lifecycle", Check: "close", Status: "passed", File: "/somewhere/else_test.go", Symbol: "TestElse"},
		{Feature: "Not A Feature", Requirement: "lifecycle", Check: "broken", Status: "passed", File: inside, Symbol: "TestBroken"},
	}
	report, foreign, warnings := ProjectReport(fragments, root)
	if report == nil {
		t.Fatal("no report was built")
	}
	if len(report.Observations) != 1 {
		t.Fatalf("observations = %+v, want the single reduced flush observation", report.Observations)
	}
	observation := report.Observations[0]
	if observation.Check != "flush" || observation.Status != features.ObservationFailed ||
		observation.Provenance.Path != "logger_test.go" || observation.Provenance.Symbol != "TestFlushTwin" {
		t.Fatalf("observation = %+v, want the failing verdict to win the reduction", observation)
	}
	if len(foreign) != 1 || foreign[0].Check != "close" {
		t.Fatalf("foreign = %+v", foreign)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %v, want the conflict notice and the wire refusal", warnings)
	}
	if !strings.Contains(warnings[0], "conflicting verdicts") || !strings.Contains(warnings[1], "broken") {
		t.Fatalf("warnings = %v", warnings)
	}

	if empty, _, _ := ProjectReport(nil, root); empty != nil {
		t.Fatal("zero fragments produced a report")
	}
}

// TestProjectReportMapsMeasurementFragments pins the threshold half of the
// merge: a measurement fragment becomes a measured observation —
// aggregate, window, environment, no verdict — validated by the same strict
// wire; a fragment carrying both a verdict and a measurement keeps only its
// verdict, loudly, so a recorded failure can never be dropped by the measured
// reduction and a self-declared threshold result never reaches core as a
// measurement.
func TestProjectReportMapsMeasurementFragments(t *testing.T) {
	root := specProject(t)
	inside := filepath.Join(root, "logger_test.go")
	measurement := &spectest.FragmentMeasurement{Name: "logger.flush.duration", Aggregation: "p95", Value: 12.5, Unit: "ms"}
	window := &spectest.FragmentWindow{Start: "2026-08-18T10:00:00Z", End: "2026-08-18T10:00:01Z"}
	fragments := []spectest.Fragment{
		{Feature: "go/structured-logging", Requirement: "flush-latency", Check: "flush-benchmark",
			Measurement: measurement, Window: window, File: inside, Symbol: "BenchmarkFlush"},
		{Feature: "go/structured-logging", Requirement: "delivery-slo", Check: "self-certified",
			Status: "passed", Measurement: measurement, Window: window, File: inside, Symbol: "TestForged"},
	}
	report, foreign, warnings := ProjectReport(fragments, root)
	if len(foreign) != 0 {
		t.Fatalf("foreign = %+v", foreign)
	}
	if report == nil || len(report.Observations) != 2 {
		t.Fatalf("report = %+v, want the honest measurement and the stripped verdict", report)
	}
	observation := report.Observations[0]
	if observation.Status != "" || observation.Measurement == nil ||
		observation.Measurement.Value != 12.5 || string(observation.Measurement.Aggregation) != "p95" ||
		observation.Window == nil || observation.Window.Start != "2026-08-18T10:00:00Z" {
		t.Fatalf("observation = %+v", observation)
	}
	stripped := report.Observations[1]
	if stripped.Check != "self-certified" || stripped.Status != features.ObservationPassed ||
		stripped.Measurement != nil || stripped.Window != nil || stripped.Environment != "" {
		t.Fatalf("stripped observation = %+v, want the bare verdict with every measured field dropped", stripped)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "both a verdict and a measurement") {
		t.Fatalf("warnings = %v, want the defective-fragment notice", warnings)
	}
}

// TestReduceMeasuredObservations pins the criterion-blind reduction: one
// measurement per check survives — the freshest observed window first, then
// provenance order — with a warning when the aggregates diverge (the merge
// cannot know which value the authored target would judge worse) and none
// when they agree.
func TestReduceMeasuredObservations(t *testing.T) {
	root := specProject(t)
	inside := filepath.Join(root, "logger_test.go")
	fragment := func(symbol string, value float64) spectest.Fragment {
		return spectest.Fragment{
			Feature: "go/structured-logging", Requirement: "flush-latency", Check: "flush-benchmark",
			Measurement: &spectest.FragmentMeasurement{Name: "logger.flush.duration", Aggregation: "p95", Value: value, Unit: "ms"},
			Window:      &spectest.FragmentWindow{Start: "2026-08-18T10:00:00Z", End: "2026-08-18T10:00:01Z"},
			File:        inside, Symbol: symbol,
		}
	}

	report, _, warnings := ProjectReport([]spectest.Fragment{fragment("BenchmarkB", 20), fragment("BenchmarkA", 10)}, root)
	if report == nil || len(report.Observations) != 1 || report.Observations[0].Provenance.Symbol != "BenchmarkA" ||
		report.Observations[0].Measurement.Value != 10 {
		t.Fatalf("report = %+v, want the first provenance in order", report)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "diverging aggregates") {
		t.Fatalf("warnings = %v, want the divergence notice", warnings)
	}

	report, _, warnings = ProjectReport([]spectest.Fragment{fragment("BenchmarkB", 10), fragment("BenchmarkA", 10)}, root)
	if report == nil || len(report.Observations) != 1 || len(warnings) != 0 {
		t.Fatalf("agreeing measurements drew report %+v warnings %v", report, warnings)
	}

	// Between agreeing measurements the freshest observed window wins even
	// against provenance order: keeping the older window where a fresher
	// observation exists could only fail rolling freshness spuriously.
	stale := fragment("BenchmarkA", 10)
	stale.Window = &spectest.FragmentWindow{Start: "2026-08-18T09:00:00Z", End: "2026-08-18T09:00:01Z"}
	report, _, warnings = ProjectReport([]spectest.Fragment{stale, fragment("BenchmarkB", 10)}, root)
	if report == nil || len(report.Observations) != 1 ||
		report.Observations[0].Provenance.Symbol != "BenchmarkB" ||
		report.Observations[0].Window.End != "2026-08-18T10:00:01Z" {
		t.Fatalf("report = %+v, want the freshest window to survive", report)
	}
	if len(warnings) != 0 {
		t.Fatalf("agreeing values over different windows drew warnings %v", warnings)
	}

	// A group mixing a verdict and a measurement is a producer defect: the
	// verdict side wins (a failure must reach core) and the mix is surfaced.
	mixed := []spectest.Fragment{
		fragment("BenchmarkA", 10),
		{Feature: "go/structured-logging", Requirement: "flush-latency", Check: "flush-benchmark",
			Status: "failed", File: inside, Symbol: "TestFlush"},
	}
	report, _, warnings = ProjectReport(mixed, root)
	if report == nil || len(report.Observations) != 1 || report.Observations[0].Status != features.ObservationFailed {
		t.Fatalf("report = %+v, want the failed verdict to win the mixed group", report)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "mixes acceptance verdicts and measurements") {
		t.Fatalf("warnings = %v", warnings)
	}

	// When the verdict side of a mixed group also disagrees with itself, the
	// mix must not swallow the conflict: one warning names both defects.
	conflicted := []spectest.Fragment{
		fragment("BenchmarkA", 10),
		{Feature: "go/structured-logging", Requirement: "flush-latency", Check: "flush-benchmark",
			Status: "failed", File: inside, Symbol: "TestFlush"},
		{Feature: "go/structured-logging", Requirement: "flush-latency", Check: "flush-benchmark",
			Status: "passed", File: inside, Symbol: "TestFlushTwin"},
	}
	report, _, warnings = ProjectReport(conflicted, root)
	if report == nil || len(report.Observations) != 1 || report.Observations[0].Status != features.ObservationFailed {
		t.Fatalf("report = %+v, want the failure to win the conflicted mixed group", report)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "has conflicting verdicts") ||
		!strings.Contains(warnings[0], "mixes acceptance verdicts and measurements") {
		t.Fatalf("warnings = %v, want the combined mix-and-conflict notice", warnings)
	}
}

// TestProjectReportCanonicalizesFilesystemAliases pins the physical
// containment rule (ported with the logic from go/extension): a symlinked
// project root still owns its real files — macOS
// exposes temp directories through both /var and /private/var — while a
// symlink inside the project that escapes outside it fails closed as
// foreign.
func TestProjectReportCanonicalizesFilesystemAliases(t *testing.T) {
	realRoot := t.TempDir()
	inside := filepath.Join(realRoot, "inside_test.go")
	if err := os.WriteFile(inside, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	aliasRoot := filepath.Join(t.TempDir(), "project")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Skipf("filesystem symlinks are unavailable: %v", err)
	}

	outsideRoot := t.TempDir()
	outside := filepath.Join(outsideRoot, "outside_test.go")
	if err := os.WriteFile(outside, []byte("package outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(realRoot, "escape_test.go")
	if err := os.Symlink(outside, escape); err != nil {
		t.Skipf("file symlinks are unavailable: %v", err)
	}

	fragments := []spectest.Fragment{
		{Feature: "go/structured-logging", Requirement: "lifecycle", Check: "alias", Status: "passed", File: inside, Symbol: "TestAlias"},
		{Feature: "go/structured-logging", Requirement: "lifecycle", Check: "escape", Status: "passed", File: escape, Symbol: "TestEscape"},
	}
	report, foreign, warnings := ProjectReport(fragments, aliasRoot)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if report == nil || len(report.Observations) != 1 || report.Observations[0].Provenance.Path != "inside_test.go" {
		t.Fatalf("report = %+v, want the physically contained declaration", report)
	}
	if len(foreign) != 1 || foreign[0].Check != "escape" {
		t.Fatalf("foreign = %+v, want the symlink escape rejected", foreign)
	}

	// OwnedBy applies the identical rule for batch attribution.
	if !OwnedBy(fragments[0], aliasRoot) {
		t.Error("an alias-rooted owned fragment was reported foreign")
	}
	if OwnedBy(fragments[1], aliasRoot) {
		t.Error("a symlink escape was claimed")
	}
}

// TestProjectReportReducesAgreeingObservationsSilently pins the common
// legitimate shape — several tests proving one check with the same verdict:
// one observation survives, its provenance is the first in (path, symbol)
// order, no warning is drawn, and a skip never erases a real pass.
func TestProjectReportReducesAgreeingObservationsSilently(t *testing.T) {
	root := specProject(t)
	inside := filepath.Join(root, "logger_test.go")
	fragments := []spectest.Fragment{
		{Feature: "go/structured-logging", Requirement: "lifecycle", Check: "flush", Status: "passed", File: inside, Symbol: "TestFlushB"},
		{Feature: "go/structured-logging", Requirement: "lifecycle", Check: "flush", Status: "passed", File: inside, Symbol: "TestFlushA"},
		{Feature: "go/structured-logging", Requirement: "lifecycle", Check: "flush", Status: "skipped", File: inside, Symbol: "TestFlushSkipped"},
	}
	report, _, warnings := ProjectReport(fragments, root)
	if report == nil || len(report.Observations) != 1 {
		t.Fatalf("report = %+v", report)
	}
	observation := report.Observations[0]
	if observation.Status != features.ObservationPassed || observation.Provenance.Symbol != "TestFlushA" {
		t.Fatalf("observation = %+v, want the pass with the first provenance in order", observation)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none for agreeing observations", warnings)
	}
}

func TestMergeSoloWritesTheReservedArtifact(t *testing.T) {
	root := specProject(t)
	fragmentsDir := t.TempDir()
	outDir := t.TempDir()
	writeSpecFragment(t, fragmentsDir, spectest.Fragment{
		Feature: "go/structured-logging", Requirement: "lifecycle", Check: "flush-visits-every-sink",
		Status: "passed", File: filepath.Join(root, "logger_test.go"), Symbol: "TestFlush",
	})
	writeSpecFragment(t, fragmentsDir, spectest.Fragment{
		Feature: "go/structured-logging", Requirement: "lifecycle", Check: "outside",
		Status: "passed", File: "/elsewhere/other_test.go", Symbol: "TestOther",
	})
	if err := os.WriteFile(filepath.Join(fragmentsDir, "junk.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	events := captureEvents(t, func(emit *jsonl.Emitter) {
		MergeSolo(emit, fragmentsDir, root, outDir)
	})

	written, err := os.ReadFile(filepath.Join(outDir, ReportFilename))
	if err != nil {
		t.Fatalf("no report was written: %v", err)
	}
	report, findings := features.ParseAndValidateVerificationReport(written)
	if report == nil || len(findings) > 0 {
		t.Fatalf("the published report fails its own strict reader: %v", findings)
	}
	if len(report.Observations) != 1 || report.Observations[0].Check != "flush-visits-every-sink" {
		t.Fatalf("observations = %+v", report.Observations)
	}
	if !eventsMention(events, features.VerificationReportArtifactID) {
		t.Fatalf("no artifact event on the wire: %v", events)
	}
	if !eventsMention(events, "outside the reporting project") || !eventsMention(events, "junk.json") {
		t.Fatalf("dropped inputs were not surfaced: %v", events)
	}
}

func TestMergeSoloWritesNothingWithoutObservations(t *testing.T) {
	outDir := t.TempDir()
	_ = captureEvents(t, func(emit *jsonl.Emitter) {
		MergeSolo(emit, t.TempDir(), specProject(t), outDir)
	})
	if _, err := os.Stat(filepath.Join(outDir, ReportFilename)); !os.IsNotExist(err) {
		t.Fatalf("an observation-free run wrote a report (stat err = %v)", err)
	}
}

func TestEmitReportSkipsNilAndAnnouncesWrites(t *testing.T) {
	if path, err := EmitReport(nil, nil, t.TempDir()); err != nil || path != "" {
		t.Fatalf("EmitReport(nil report) = (%q, %v)", path, err)
	}

	outDir := t.TempDir()
	report := &features.VerificationReport{
		ProtocolVersion: features.VerificationReportProtocolVersion,
		Observations: []features.VerificationObservation{{
			Feature: "go/alpha", Requirement: "holds", Check: "alpha-check",
			Status:     features.ObservationPassed,
			Provenance: features.ObservationProvenance{Path: "alpha_test.go", Symbol: "TestAlpha"},
		}},
	}
	// A nil emitter must write without announcing: the batch adapters return
	// artifacts through their wire struct instead of the event stream.
	path, err := EmitReport(nil, report, outDir)
	if err != nil || path != filepath.Join(outDir, ReportFilename) {
		t.Fatalf("EmitReport = (%q, %v)", path, err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if parsed, findings := features.ParseAndValidateVerificationReport(written); parsed == nil || len(findings) > 0 {
		t.Fatalf("published report fails the strict reader: %v", findings)
	}
}

func TestOwnedBy(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "alpha")
	owned := filepath.Join(root, "x_test.go")
	stranger := filepath.Join(workspace, "gamma", "x_test.go")
	for _, path := range []string{owned, stranger} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if !OwnedBy(spectest.Fragment{File: owned}, root) {
		t.Error("an owned fragment was reported foreign")
	}
	if OwnedBy(spectest.Fragment{File: stranger}, root) {
		t.Error("a foreign fragment was claimed")
	}
	if OwnedBy(spectest.Fragment{File: workspace}, root) {
		t.Error("the parent directory was claimed")
	}
	// A declaration that does not physically exist cannot be canonicalized and
	// is never owned: attribution refuses to guess.
	if OwnedBy(spectest.Fragment{File: filepath.Join(root, "ghost_test.go")}, root) {
		t.Error("a nonexistent declaration was claimed")
	}
}
