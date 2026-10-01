// Package specreport is the adapter half of the executable-spec loop,
// shared by every language extension: tests bind themselves to
// declared checks through a runtime-native helper (Go's
// go.putnami.dev/protocol/features/spectest, `specTest` in TypeScript, the
// `putnami_proves` pytest marker in Python), each test process writes
// fragments into a directory the test job provides, and after the runner
// returns the fragments are merged, validated, and published as the reserved
// putnami-feature-verification report artifact — one per project, under
// batching too, so attribution survives every execution shape. The report is
// transport: it carries observations only, and core recomputes every verdict
// against the authored criterion.
//
// The trust rules live here exactly once. Provenance is resolved to a
// project-relative path and a declaration outside the reporting project is
// refused, so a report can never claim another project's source as proof;
// byte-identical observations collapse; and everything is validated against
// the strict wire before it is published. Because all three extensions merge
// through this package and encode through
// features.MarshalVerificationReport, the report bytes are equivalent across
// runtimes by construction.
package specreport

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/scratch"
)

const (
	// ReportFilename is the reserved report artifact's file name inside the
	// task's command-output directory.
	ReportFilename = features.VerificationReportFilename
	// maxFragmentFiles and maxFragmentBytes bound the merge: fragments are
	// tiny single observations, so a directory beyond these limits is a defect
	// reported loudly rather than an unbounded read.
	maxFragmentFiles = 4096
	maxFragmentBytes = 64 << 10
)

// NewFragmentDirectory mints the per-run fragment directory handed to the
// test processes. The path is FRESH on every run on purpose: a runner with
// its own result cache (go test) records that a test consulted the
// environment variable, so a new value re-executes every bound test — a
// cached verdict can never be served with its fragment silently unwritten.
// Runners without a result cache lose nothing by the fresh path. The directory
// is scratch-owned: when the job is killed before its cleanup runs, the next
// run removes it. Its owner lock file is hidden, so ReadFragments skips it.
func NewFragmentDirectory() (string, func(), error) {
	directory, err := scratch.New("putnami-spec-fragments-")
	if err != nil {
		return "", nil, fmt.Errorf("create spec fragment directory: %w", err)
	}
	return directory.Path(), func() { _ = directory.Remove() }, nil
}

// ReadFragments loads every fragment the test processes wrote, bounded and
// sorted by file name so the merge is deterministic.
func ReadFragments(directory string) ([]spectest.Fragment, []string) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, []string{fmt.Sprintf("spec fragments unreadable: %v", err)}
	}
	if len(entries) > maxFragmentFiles {
		return nil, []string{fmt.Sprintf("spec fragment directory exceeds the bounded limit of %d files", maxFragmentFiles)}
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)

	var warnings []string
	fragments := make([]spectest.Fragment, 0, len(names))
	for _, name := range names {
		path := filepath.Join(directory, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxFragmentBytes {
			warnings = append(warnings, fmt.Sprintf("spec fragment %s dropped: not a bounded regular file", name))
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("spec fragment %s dropped: %v", name, err))
			continue
		}
		var fragment spectest.Fragment
		if err := json.Unmarshal(data, &fragment); err != nil {
			warnings = append(warnings, fmt.Sprintf("spec fragment %s dropped: %v", name, err))
			continue
		}
		fragments = append(fragments, fragment)
	}
	return fragments, warnings
}

// ProjectReport resolves one project's fragments onto the strict wire:
// provenance becomes the declaration file's project-relative path, a
// declaration outside the project is dropped (a report can never claim
// another project's source as proof), and the result is validated before
// anything is published. Fragments whose declaration lives in another project
// are left for that project's own merge and reported through the second
// return value.
//
// The wire admits at most ONE observation per (feature, requirement, check),
// so several tests observing the same check — a legitimate authoring shape —
// are reduced deterministically: a failure beats a pass beats a skip (any
// active contradiction must reach core), and within the winning verdict the
// first provenance in (path, symbol) order is kept. Conflicting verdicts are
// surfaced as a warning; agreeing multiplicity is not, because it is normal.
func ProjectReport(fragments []spectest.Fragment, projectRoot string) (*features.VerificationReport, []spectest.Fragment, []string) {
	var warnings []string
	var foreign []spectest.Fragment
	report := &features.VerificationReport{ProtocolVersion: features.VerificationReportProtocolVersion}

	var order []string
	grouped := make(map[string][]features.VerificationObservation)
	cleanRoot, rootErr := canonicalPath(projectRoot)
	if rootErr != nil {
		warnings = append(warnings, fmt.Sprintf("reporting project root cannot be resolved: %v", rootErr))
		return nil, append(foreign, fragments...), warnings
	}
	for _, fragment := range fragments {
		cleanFile, err := canonicalPath(fragment.File)
		if err != nil {
			foreign = append(foreign, fragment)
			continue
		}
		relative, err := filepath.Rel(cleanRoot, cleanFile)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			foreign = append(foreign, fragment)
			continue
		}
		observation := features.VerificationObservation{
			Feature:     fragment.Feature,
			Requirement: fragment.Requirement,
			Check:       fragment.Check,
			Status:      features.ObservationStatus(fragment.Status),
			Provenance: features.ObservationProvenance{
				Path:   filepath.ToSlash(relative),
				Symbol: fragment.Symbol,
			},
		}
		// The wire admits a verdict or a measurement, never both. A fragment
		// stating both is a producer defect; keeping only the verdict — loudly
		// — is the one repair that never loses a recorded failure, where
		// letting the defective fragment reach the measured reduction could
		// drop that failure or erase an honest measurement from another test.
		switch {
		case fragment.Status != "" && fragment.Measurement != nil:
			warnings = append(warnings, fmt.Sprintf(
				"spec fragment (%s, %s, %s) carries both a verdict and a measurement; the verdict wins",
				fragment.Feature, fragment.Requirement, fragment.Check))
		case fragment.Measurement != nil:
			observation.Measurement = &features.ObservationMeasurement{
				Name:        fragment.Measurement.Name,
				Aggregation: features.VerificationAggregation(fragment.Measurement.Aggregation),
				Value:       fragment.Measurement.Value,
				Unit:        fragment.Measurement.Unit,
			}
			observation.Environment = fragment.Environment
			if fragment.Window != nil {
				observation.Window = &features.ObservedWindow{Start: fragment.Window.Start, End: fragment.Window.End}
			}
		}
		key := strings.Join([]string{observation.Feature, observation.Requirement, observation.Check}, "\x00")
		if _, exists := grouped[key]; !exists {
			order = append(order, key)
		}
		grouped[key] = append(grouped[key], observation)
	}
	for _, key := range order {
		observation, warning := reduceCheckObservations(grouped[key])
		if warning != "" {
			warnings = append(warnings, warning)
		}
		report.Observations = append(report.Observations, observation)
	}
	if len(report.Observations) == 0 {
		return nil, foreign, warnings
	}
	if findings := features.ValidateVerificationReport(report); len(findings) > 0 {
		// A malformed observation must not erase the valid ones: rebuild with
		// only the observations that survive validation individually.
		valid := report.Observations[:0]
		for _, observation := range report.Observations {
			single := &features.VerificationReport{
				ProtocolVersion: features.VerificationReportProtocolVersion,
				Observations:    []features.VerificationObservation{observation},
			}
			if findings := features.ValidateVerificationReport(single); len(findings) > 0 {
				warnings = append(warnings, fmt.Sprintf(
					"spec observation (%s, %s, %s) dropped: fails the verification wire",
					observation.Feature, observation.Requirement, observation.Check))
				continue
			}
			valid = append(valid, observation)
		}
		report.Observations = valid
		if len(report.Observations) == 0 {
			return nil, foreign, warnings
		}
		// Never publish a report the strict reader would refuse whole: core
		// would then resolve every observation in it as missing, silently.
		if findings := features.ValidateVerificationReport(report); len(findings) > 0 {
			warnings = append(warnings, fmt.Sprintf(
				"spec verification report dropped: still fails the wire after salvage (%v)", findings))
			return nil, foreign, warnings
		}
	}
	return report, foreign, warnings
}

// statusPrecedence orders verdicts fail-closed for the per-check reduction: a
// failure always reaches core, a real pass beats a skip.
var statusPrecedence = map[features.ObservationStatus]int{
	features.ObservationFailed:  0,
	features.ObservationPassed:  1,
	features.ObservationSkipped: 2,
}

func reduceCheckObservations(observations []features.VerificationObservation) (features.VerificationObservation, string) {
	if len(observations) == 1 {
		return observations[0], ""
	}
	// A check is authored as either an acceptance or a threshold criterion,
	// never both, so a group mixing verdicts and measurements is a producer
	// defect. The verdict side wins — a recorded failure must reach core —
	// and the mix is surfaced rather than silently repaired.
	var verdicts, measured []features.VerificationObservation
	for _, observation := range observations {
		if observation.Measurement != nil {
			measured = append(measured, observation)
		} else {
			verdicts = append(verdicts, observation)
		}
	}
	if len(verdicts) > 0 && len(measured) > 0 {
		winner, conflict := reduceCheckObservations(verdicts)
		if conflict != "" {
			// The verdict side's own disagreement is the stronger signal; the
			// mix must not swallow it.
			return winner, fmt.Sprintf(
				"spec check (%s, %s, %s) mixes acceptance verdicts and measurements and has conflicting verdicts across its observing tests; the failure wins",
				winner.Feature, winner.Requirement, winner.Check)
		}
		return winner, fmt.Sprintf(
			"spec check (%s, %s, %s) mixes acceptance verdicts and measurements across its observing tests; the verdicts win",
			winner.Feature, winner.Requirement, winner.Check)
	}
	if len(measured) == len(observations) {
		return reduceMeasuredObservations(measured)
	}
	winner := observations[0]
	conflicting := false
	for _, candidate := range observations[1:] {
		if candidate.Status != winner.Status {
			conflicting = conflicting || candidate.Status == features.ObservationFailed || winner.Status == features.ObservationFailed
		}
		winnerRank, winnerKnown := statusPrecedence[winner.Status]
		candidateRank, candidateKnown := statusPrecedence[candidate.Status]
		switch {
		case !candidateKnown:
			continue
		case !winnerKnown, candidateRank < winnerRank:
			winner = candidate
		case candidateRank == winnerRank && provenanceLess(candidate.Provenance, winner.Provenance):
			winner = candidate
		}
	}
	warning := ""
	if conflicting {
		warning = fmt.Sprintf(
			"spec check (%s, %s, %s) has conflicting verdicts across its observing tests; the failure wins",
			winner.Feature, winner.Requirement, winner.Check)
	}
	return winner, warning
}

// reduceMeasuredObservations keeps one measurement per check: the freshest
// observed window wins, then (path, symbol) provenance breaks ties. The merge
// is deliberately criterion-blind — it cannot know which aggregate the
// authored target would judge worse, so divergent values are surfaced as a
// warning instead of silently choosing a favorable one — but recency it CAN
// judge without favoring a value: keeping an older window where a fresher
// observation exists could only fail rolling freshness spuriously, never help
// it. Core recomputes the verdict from whichever observation survives.
func reduceMeasuredObservations(measured []features.VerificationObservation) (features.VerificationObservation, string) {
	winner := measured[0]
	diverging := false
	for _, candidate := range measured[1:] {
		if !sameAggregate(candidate, winner) {
			diverging = true
		}
		if measuredPreferred(candidate, winner) {
			winner = candidate
		}
	}
	warning := ""
	if diverging {
		warning = fmt.Sprintf(
			"spec check (%s, %s, %s) was measured more than once with diverging aggregates; kept %s#%s",
			winner.Feature, winner.Requirement, winner.Check, winner.Provenance.Path, winner.Provenance.Symbol)
	}
	return winner, warning
}

// sameAggregate compares the measured aggregate and environment. Both sides
// always carry a measurement — the partition in reduceCheckObservations sends
// nothing else here. The window is deliberately not compared: two honest
// invocations of one check legitimately cover different spans, and
// measuredPreferred already keeps the freshest.
func sameAggregate(a, b features.VerificationObservation) bool {
	return *a.Measurement == *b.Measurement && a.Environment == b.Environment
}

// measuredPreferred reports whether candidate displaces winner: later observed
// window end first, provenance order on equal ends.
func measuredPreferred(candidate, winner features.VerificationObservation) bool {
	candidateEnd, winnerEnd := windowEnd(candidate), windowEnd(winner)
	if !candidateEnd.Equal(winnerEnd) {
		return candidateEnd.After(winnerEnd)
	}
	return provenanceLess(candidate.Provenance, winner.Provenance)
}

// windowEnd parses the observed window's end instant. An absent or unparsable
// window sorts before every dated one, so it can never displace a real
// observation — and the wire refuses it downstream anyway.
func windowEnd(observation features.VerificationObservation) time.Time {
	if observation.Window == nil {
		return time.Time{}
	}
	end, err := time.Parse(time.RFC3339, observation.Window.End)
	if err != nil {
		return time.Time{}
	}
	return end
}

func provenanceLess(a, b features.ObservationProvenance) bool {
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	return a.Symbol < b.Symbol
}

// canonicalPath resolves both lexical and filesystem aliases before project
// attribution. macOS exposes temporary directories through both /var and
// /private/var, and workspaces may themselves be symlinked; those aliases
// must not discard valid evidence. Resolving the declaration path also makes
// a symlink inside a project that escapes outside it fail closed.
func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

// OwnedBy reports whether one fragment's declaration file lives inside the
// project root — the same physical containment rule ProjectReport applies —
// so a batching adapter can surface an observation no member owns instead of
// letting it silently vanish between members. An unresolvable path is not
// owned: attribution never guesses.
func OwnedBy(fragment spectest.Fragment, projectRoot string) bool {
	root, err := canonicalPath(projectRoot)
	if err != nil {
		return false
	}
	file, err := canonicalPath(fragment.File)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(root, file)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// EmitReport writes one project's merged report into outDir and announces it
// under the reserved artifact id, using the absolute path the CLI normalizes
// to its workspace-relative display form. A nil emitter skips the
// announcement (batch adapters return artifacts through their wire struct
// instead). Returns the absolute path, empty when the project produced no
// observations (the declaration marks the output optional).
func EmitReport(emit *jsonl.Emitter, report *features.VerificationReport, outDir string) (string, error) {
	if report == nil {
		return "", nil
	}
	encoded, err := features.MarshalVerificationReport(report)
	if err != nil {
		return "", fmt.Errorf("encode verification report: %w", err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", fmt.Errorf("prepare verification report output: %w", err)
	}
	destination := filepath.Join(outDir, ReportFilename)
	if err := os.WriteFile(destination, encoded, 0o644); err != nil {
		return "", fmt.Errorf("write verification report: %w", err)
	}
	if emit != nil {
		emit.Artifact(features.VerificationReportArtifactID, "Feature Verification Report", "report", destination)
	}
	return destination, nil
}

// MergeSolo is the single-project path: read, resolve, publish, and surface
// every dropped fragment as a warning diagnostic. Reporting problems never
// change the test verdict — the report is transport, and core resolves a
// missing or partial one as missing evidence, which is the fail-closed
// direction.
func MergeSolo(emit *jsonl.Emitter, directory, projectRoot, outDir string) {
	fragments, warnings := ReadFragments(directory)
	report, foreign, buildWarnings := ProjectReport(fragments, projectRoot)
	warnings = append(warnings, buildWarnings...)
	for _, fragment := range foreign {
		warnings = append(warnings, fmt.Sprintf(
			"spec observation (%s, %s, %s) dropped: declaration %s is outside the reporting project",
			fragment.Feature, fragment.Requirement, fragment.Check, fragment.File))
	}
	if _, err := EmitReport(emit, report, outDir); err != nil {
		warnings = append(warnings, err.Error())
	}
	for _, warning := range warnings {
		emit.Diagnostic("warning", warning, "", 0)
	}
}
