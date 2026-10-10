package features

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
)

const evaluationInstant = "2026-08-18T10:00:00Z"

// evaluatedAt is the fixed instant every rolling window in this file is judged
// against, so a freshness boundary is a property of the fixture rather than of
// the moment the suite happens to run.
func evaluatedAt(t *testing.T) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, evaluationInstant)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func target(value float64) *float64 { return &value }

func acceptanceRequirement(checks ...string) Requirement {
	return Requirement{
		ID: "lifecycle", Stage: MaturityCoded, EvidenceKinds: []EvidenceKind{EvidenceKindAttestation},
		Verification: &VerificationCriterion{Kind: VerificationKindAcceptance, Checks: checks},
	}
}

func invocationThresholdRequirement() Requirement {
	return Requirement{
		ID: "flush-latency", Stage: MaturityCoded, EvidenceKinds: []EvidenceKind{EvidenceKindAttestation},
		Verification: &VerificationCriterion{
			Kind: VerificationKindThreshold, Checks: []string{"flush-benchmark"},
			Metric: "logger.flush.duration", Aggregation: AggregationP95, Operator: OperatorLte,
			Target: target(25), Unit: "ms", Window: &VerificationWindow{Kind: WindowKindInvocation},
		},
	}
}

func rollingThresholdRequirement() Requirement {
	return Requirement{
		ID: "delivery-slo", Stage: MaturityLiveVerified, EvidenceKinds: []EvidenceKind{EvidenceKindAttestation},
		Verification: &VerificationCriterion{
			Kind: VerificationKindThreshold, Checks: []string{"production-delivery-slo"},
			Metric: "logging.delivery.success", Aggregation: AggregationRatio, Operator: OperatorGte,
			Target: target(0.999), Unit: "ratio",
			Window:      &VerificationWindow{Kind: WindowKindRolling, Seconds: 2592000},
			Environment: "production", MaxAgeSeconds: 3600,
		},
	}
}

func verifiedManifest(requirements ...Requirement) *Manifest {
	feature := Feature{
		ID: "logging/structured-logging", Type: FeatureTypeFeature, Name: "Structured logging",
		Outcome: "Applications emit structured records", Owner: "platform", Target: MaturityCoded,
		Requirements: requirements,
	}
	return &Manifest{ProtocolVersion: ManifestProtocolVersion, Namespace: "logging", Features: []Feature{feature}}
}

func statusObservation(requirement, check string, status ObservationStatus) VerificationObservation {
	return VerificationObservation{
		Feature: "logging/structured-logging", Requirement: requirement, Check: check, Status: status,
		Provenance: ObservationProvenance{Path: "logger_test.go", Symbol: "TestLogger"},
	}
}

func measuredObservation(requirement, check string, measurement ObservationMeasurement, window ObservedWindow, environment string) VerificationObservation {
	return VerificationObservation{
		Feature: "logging/structured-logging", Requirement: requirement, Check: check,
		Measurement: &measurement, Window: &window, Environment: environment,
		Provenance: ObservationProvenance{Path: "internal/slo/delivery.go", Symbol: "ObserveDeliverySLO"},
	}
}

func TestManifestVersionDispatchRefusesTheForwardField(t *testing.T) {
	criterion := `"verification":{"kind":"acceptance","checks":["lifecycle-flush"]}`
	requirement := `{"id":"lifecycle","stage":"coded","evidenceKinds":["attestation"],` + criterion + `}`
	feature := `{"id":"logging/structured-logging","type":"feature","name":"L","outcome":"L","owner":"p","target":"coded","requirements":[` + requirement + `]}`

	current := `{"protocolVersion":2,"namespace":"logging","features":[` + feature + `]}`
	manifest, findings := ParseAndValidateManifest([]byte(current))
	if manifest == nil || diag.HasErrors(findings) {
		t.Fatalf("current manifest rejected the closed criterion: %#v %v", manifest, findings)
	}
	if manifest.Features[0].Requirements[0].Verification == nil {
		t.Fatal("criterion was dropped instead of parsed")
	}

	legacy := `{"protocolVersion":1,"namespace":"logging","features":[` + feature + `]}`
	parsed, legacyFindings := ParseManifest([]byte(legacy))
	if parsed != nil || !hasCode(legacyFindings, ErrorCodeUnknownField) {
		t.Fatalf("legacy manifest accepted the forward field: %#v %v", parsed, legacyFindings)
	}

	// A caller that skips the strict decoder and builds the struct directly is
	// refused by validation for the same reason.
	direct := verifiedManifest(acceptanceRequirement("lifecycle-flush"))
	direct.ProtocolVersion = MinimumManifestProtocolVersion
	if findings := ValidateManifest(direct); !hasCode(findings, ErrorCodeUnknownField) {
		t.Fatalf("legacy struct accepted the forward field: %v", findings)
	}

	// A legacy manifest that declares no criterion stays readable unchanged.
	legacyValid := `{"protocolVersion":1,"namespace":"logging","features":[{"id":"logging/structured-logging","type":"feature","name":"L","outcome":"L","owner":"p","target":"modeled"}]}`
	if parsed, findings := ParseAndValidateManifest([]byte(legacyValid)); parsed == nil || diag.HasErrors(findings) {
		t.Fatalf("legacy manifest without a criterion rejected: %#v %v", parsed, findings)
	}
}

func TestCommittedRepositoryContentStaysOnTheLegacyManifestWire(t *testing.T) {
	// Splitting the version constant must not migrate committed manifests
	// wholesale: the repository still reads byte-identical version 1 documents,
	// and a manifest moves to version 2 only when its project starts declaring
	// executable criteria: migrating a manifest is a reviewed decision that
	// lands HERE, not a side effect of touching a file.
	//
	// The first four are the original pilots — the Go pilots and
	// the TypeScript/Python producer-parity fixtures (both in
	// report mode). The rest are the first Go framework rollout wave, each
	// enforced with every spec requirement bound to a protecting test.
	migrated := map[string]bool{
		filepath.Join("go", "framework", "logger", ManifestFilename):          true,
		filepath.Join("go", "framework", "parallel", ManifestFilename):        true,
		filepath.Join("typescript", "framework", "runtime", ManifestFilename): true,
		filepath.Join("python", "samples", "library", ManifestFilename):       true,

		filepath.Join("go", "framework", "app", ManifestFilename):                     true,
		filepath.Join("go", "framework", "cache", ManifestFilename):                   true,
		filepath.Join("go", "framework", "config", ManifestFilename):                  true,
		filepath.Join("go", "framework", "ctxutil", ManifestFilename):                 true,
		filepath.Join("go", "framework", "database", ManifestFilename):                true,
		filepath.Join("go", "framework", "errors", ManifestFilename):                  true,
		filepath.Join("go", "framework", "events", ManifestFilename):                  true,
		filepath.Join("go", "framework", "inject", ManifestFilename):                  true,
		filepath.Join("go", "framework", "migration", "migratecli", ManifestFilename): true,
		filepath.Join("go", "framework", "security", ManifestFilename):                true,
		filepath.Join("go", "framework", "storage", ManifestFilename):                 true,

		filepath.Join("go", "framework", "api", ManifestFilename):       true,
		filepath.Join("go", "framework", "grpc", ManifestFilename):      true,
		filepath.Join("go", "framework", "http", ManifestFilename):      true,
		filepath.Join("go", "framework", "platform", ManifestFilename):  true,
		filepath.Join("go", "framework", "schema", ManifestFilename):    true,
		filepath.Join("go", "framework", "telemetry", ManifestFilename): true,

		filepath.Join("typescript", "framework", "analytics", ManifestFilename):    true,
		filepath.Join("typescript", "framework", "cli-protocol", ManifestFilename): true,
		filepath.Join("typescript", "framework", "database", ManifestFilename):     true,
		filepath.Join("typescript", "framework", "utils", ManifestFilename):        true,

		filepath.Join("go", "framework", "client", ManifestFilename):              true,
		filepath.Join("go", "framework", "keyringstore", ManifestFilename):        true,
		filepath.Join("go", "framework", "migration", ManifestFilename):           true,
		filepath.Join("go", "extension", ManifestFilename):                        true,
		filepath.Join("tooling", "cli", ManifestFilename):                         true,
		filepath.Join("tooling", "cli-documents", ManifestFilename):               true,
		filepath.Join("tooling", "cli-model", ManifestFilename):                   true,
		filepath.Join("tooling", "sdd-extension", ManifestFilename):               true,
		filepath.Join("protocols", "architecture", ManifestFilename):              true,
		filepath.Join("protocols", "clientcontract", ManifestFilename):            true,
		filepath.Join("python", "extension", ManifestFilename):                    true,
		filepath.Join("sites", "telemetry.putnami.dev", ManifestFilename):         true,
		filepath.Join("sites", "putnami.dev", ManifestFilename):                   true,
		filepath.Join("tooling", "clientgen-extension", ManifestFilename):         true,
		filepath.Join("tooling", "extension-sdk", ManifestFilename):               true,
		filepath.Join("tooling", "github-collaboration", ManifestFilename):        true,
		filepath.Join("tooling", "memory-store", ManifestFilename):                true,
		filepath.Join("tooling", "scaffold", ManifestFilename):                    true,
		filepath.Join("typescript", "extension", ManifestFilename):                true,
		filepath.Join("typescript", "framework", "application", ManifestFilename): true,
		filepath.Join("typescript", "framework", "client", ManifestFilename):      true,
		filepath.Join("typescript", "framework", "document", ManifestFilename):    true,
		filepath.Join("typescript", "framework", "events", ManifestFilename):      true,
		filepath.Join("typescript", "framework", "migration", ManifestFilename):   true,
		filepath.Join("typescript", "framework", "storage", ManifestFilename):     true,
		filepath.Join("typescript", "framework", "ui", ManifestFilename):          true,
		filepath.Join("typescript", "framework", "web", ManifestFilename):         true,
		filepath.Join("typescript", "framework", "spectest", ManifestFilename):    true,

		// This extension declares executable acceptance criteria in its v2 manifest.
		filepath.Join("intelligence", "agent-readiness", ManifestFilename): true,
		// The Cloud CLI extension names the acceptance check of its archive
		// publication in the verification block.
		filepath.Join("cloud", "extension", ManifestFilename): true,

		// The two client-matrix samples author their checks in the verification
		// block, which only exists from version 2.
		filepath.Join("go", "samples", "service-to-service", ManifestFilename):            true,
		filepath.Join("typescript", "samples", "10-service-to-service", ManifestFilename): true,
	}
	// Gitignored roots are skipped too. A developer worktree legitimately holds
	// whole checkouts under .context (agent scratch) and .putnami (materialized
	// artifacts); those manifests are not this repository's committed state, and
	// reading them makes the check fail on facts the branch does not own. Same
	// skip set the language extensions' project discovery uses.
	skipped := map[string]bool{
		".cache": true, ".context": true, ".gen": true, ".git": true, ".putnami": true,
		"compiled": true, "dist": true, "node_modules": true, "vendor": true,
	}
	root := filepath.Join("..", "..")
	checked, pilots := 0, 0
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if skipped[entry.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Name() != ManifestFilename {
			return nil
		}
		//nolint:gosec // the walk only reaches this repository's committed manifests
		data, readErr := os.ReadFile(name)
		if readErr != nil {
			return readErr
		}
		relative, relErr := filepath.Rel(root, name)
		if relErr != nil {
			return relErr
		}
		wantVersion := MinimumManifestProtocolVersion
		if migrated[relative] {
			wantVersion = ManifestProtocolVersion
			pilots++
		}
		manifest, findings := ParseAndValidateManifest(data)
		switch {
		case manifest == nil || diag.HasErrors(findings):
			t.Errorf("committed manifest %s no longer validates: %v", name, findings)
		case manifest.ProtocolVersion != wantVersion:
			t.Errorf("committed manifest %s is version %d, want %d", name, manifest.ProtocolVersion, wantVersion)
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no committed manifest was reached; the compatibility check is vacuous")
	}
	if pilots != len(migrated) {
		t.Fatalf("reached %d of the %d sanctioned v2 pilot manifests; the allowlist no longer matches the tree", pilots, len(migrated))
	}
}

func TestVerificationCriterionShapeIsClosed(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(*Requirement)
		wantErr bool
	}{
		{name: "acceptance", mutate: func(Requirement *Requirement) {}},
		{name: "unknown kind", mutate: func(r *Requirement) { r.Verification.Kind = "smoke" }, wantErr: true},
		{name: "no checks", mutate: func(r *Requirement) { r.Verification.Checks = nil }, wantErr: true},
		{name: "duplicate check", mutate: func(r *Requirement) {
			r.Verification.Checks = []string{"flush-visits-every-sink", "flush-visits-every-sink"}
		}, wantErr: true},
		{name: "non-canonical check", mutate: func(r *Requirement) { r.Verification.Checks = []string{"Flush Visits"} }, wantErr: true},
		{name: "unbounded check", mutate: func(r *Requirement) {
			r.Verification.Checks = []string{strings.Repeat("a", checkIDMaxLength+1)}
		}, wantErr: true},
		{name: "acceptance with metric", mutate: func(r *Requirement) { r.Verification.Metric = "logger.flush.duration" }, wantErr: true},
		{name: "acceptance with target", mutate: func(r *Requirement) { r.Verification.Target = target(0) }, wantErr: true},
		{name: "acceptance with window", mutate: func(r *Requirement) {
			r.Verification.Window = &VerificationWindow{Kind: WindowKindInvocation}
		}, wantErr: true},
		{name: "acceptance with environment", mutate: func(r *Requirement) { r.Verification.Environment = "production" }, wantErr: true},
		{name: "without attestation", mutate: func(r *Requirement) {
			r.EvidenceKinds = []EvidenceKind{EvidenceKindCapability}
		}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			requirement := acceptanceRequirement("flush-visits-every-sink")
			test.mutate(&requirement)
			findings := ValidateManifest(verifiedManifest(requirement))
			if got := hasCode(findings, ErrorCodeInvalidVerification); got != test.wantErr {
				t.Fatalf("invalid verification = %v, want %v; findings: %v", got, test.wantErr, findings)
			}
		})
	}
}

func TestThresholdCriterionRequiresACompleteObjective(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(*Requirement)
		wantErr bool
	}{
		{name: "invocation objective", mutate: func(*Requirement) {}},
		{name: "zero target", mutate: func(r *Requirement) { r.Verification.Target = target(0) }},
		{name: "negative target", mutate: func(r *Requirement) { r.Verification.Target = target(-3.5) }},
		{name: "absent target", mutate: func(r *Requirement) { r.Verification.Target = nil }, wantErr: true},
		{name: "NaN target", mutate: func(r *Requirement) { r.Verification.Target = target(math.NaN()) }, wantErr: true},
		{name: "positive infinite target", mutate: func(r *Requirement) {
			r.Verification.Target = target(math.Inf(1))
		}, wantErr: true},
		{name: "negative infinite target", mutate: func(r *Requirement) {
			r.Verification.Target = target(math.Inf(-1))
		}, wantErr: true},
		{name: "two checks", mutate: func(r *Requirement) {
			r.Verification.Checks = []string{"flush-benchmark", "flush-load-test"}
		}, wantErr: true},
		{name: "unknown aggregation", mutate: func(r *Requirement) { r.Verification.Aggregation = "p999" }, wantErr: true},
		{name: "strict operator", mutate: func(r *Requirement) { r.Verification.Operator = "lt" }, wantErr: true},
		{name: "no metric", mutate: func(r *Requirement) { r.Verification.Metric = "" }, wantErr: true},
		{name: "unbounded metric", mutate: func(r *Requirement) {
			r.Verification.Metric = strings.Repeat("a", semanticCodeMaxLength+1)
		}, wantErr: true},
		{name: "no unit", mutate: func(r *Requirement) { r.Verification.Unit = "" }, wantErr: true},
		{name: "no window", mutate: func(r *Requirement) { r.Verification.Window = nil }, wantErr: true},
		{name: "unknown window kind", mutate: func(r *Requirement) { r.Verification.Window.Kind = "daily" }, wantErr: true},
		{name: "invocation window with duration", mutate: func(r *Requirement) { r.Verification.Window.Seconds = 60 }, wantErr: true},
		{name: "invocation window with freshness", mutate: func(r *Requirement) { r.Verification.MaxAgeSeconds = 60 }, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			requirement := invocationThresholdRequirement()
			test.mutate(&requirement)
			findings := ValidateManifest(verifiedManifest(requirement))
			if got := hasCode(findings, ErrorCodeInvalidVerification); got != test.wantErr {
				t.Fatalf("invalid verification = %v, want %v; findings: %v", got, test.wantErr, findings)
			}
		})
	}
}

func TestRollingObjectiveRequiresLiveVerifiedAuthority(t *testing.T) {
	manifest := verifiedManifest(rollingThresholdRequirement())
	manifest.Features[0].Target = MaturityLiveVerified
	manifest.Features[0].Requirements = append(manifest.Features[0].Requirements,
		Requirement{ID: "implementation", Stage: MaturityCoded, EvidenceKinds: []EvidenceKind{EvidenceKindCapability}},
		Requirement{ID: "integration", Stage: MaturityWired, EvidenceKinds: []EvidenceKind{EvidenceKindArtifact}},
		Requirement{ID: "enabled", Stage: MaturityDefaultOn, EvidenceKinds: []EvidenceKind{EvidenceKindCapability}},
	)
	if findings := ValidateManifest(manifest); diag.HasErrors(findings) {
		t.Fatalf("valid rolling objective rejected: %v", findings)
	}

	for _, test := range []struct {
		name   string
		mutate func(*Manifest)
	}{
		{name: "no environment", mutate: func(m *Manifest) { rollingCriterion(t, m).Environment = "" }},
		{name: "no freshness bound", mutate: func(m *Manifest) { rollingCriterion(t, m).MaxAgeSeconds = 0 }},
		{name: "negative freshness bound", mutate: func(m *Manifest) { rollingCriterion(t, m).MaxAgeSeconds = -1 }},
		{name: "zero duration", mutate: func(m *Manifest) { rollingCriterion(t, m).Window.Seconds = 0 }},
		{name: "negative duration", mutate: func(m *Manifest) { rollingCriterion(t, m).Window.Seconds = -1 }},
		{name: "unbounded duration", mutate: func(m *Manifest) {
			rollingCriterion(t, m).Window.Seconds = rollingWindowMaxSeconds + 1
		}},
		{name: "below live-verified", mutate: func(m *Manifest) {
			for i := range m.Features[0].Requirements {
				if m.Features[0].Requirements[i].ID == "delivery-slo" {
					m.Features[0].Requirements[i].Stage = MaturityCoded
				}
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutated := CanonicalManifest(manifest)
			test.mutate(mutated)
			if findings := ValidateManifest(mutated); !hasCode(findings, ErrorCodeInvalidVerification) {
				t.Fatalf("invalid rolling objective accepted: %v", findings)
			}
		})
	}
}

func rollingCriterion(t *testing.T, manifest *Manifest) *VerificationCriterion {
	t.Helper()
	for i := range manifest.Features[0].Requirements {
		if manifest.Features[0].Requirements[i].ID == "delivery-slo" {
			return manifest.Features[0].Requirements[i].Verification
		}
	}
	t.Fatal("rolling requirement is missing")
	return nil
}

func TestVerificationReportShapeIsClosedAndBounded(t *testing.T) {
	valid := VerificationObservation{
		Feature: "logging/structured-logging", Requirement: "lifecycle", Check: "flush-visits-every-sink",
		Status: ObservationPassed, Provenance: ObservationProvenance{Path: "logger_test.go"},
	}
	if findings := ValidateVerificationReport(&VerificationReport{ProtocolVersion: 1, Observations: []VerificationObservation{valid}}); diag.HasErrors(findings) {
		t.Fatalf("valid acceptance observation rejected: %v", findings)
	}

	for _, test := range []struct {
		name   string
		mutate func(*VerificationObservation)
		want   string
	}{
		{name: "no verdict", mutate: func(o *VerificationObservation) { o.Status = "" }, want: ErrorCodeInvalidObservation},
		{name: "unknown status", mutate: func(o *VerificationObservation) { o.Status = "flaky" }, want: ErrorCodeInvalidObservation},
		{name: "status with window", mutate: func(o *VerificationObservation) {
			o.Window = &ObservedWindow{Start: evaluationInstant, End: evaluationInstant}
		}, want: ErrorCodeInvalidObservation},
		{name: "status with environment", mutate: func(o *VerificationObservation) { o.Environment = "production" }, want: ErrorCodeInvalidObservation},
		{name: "both verdicts", mutate: func(o *VerificationObservation) {
			o.Measurement = &ObservationMeasurement{Name: "logger.flush.duration", Aggregation: AggregationP95, Value: 12, Unit: "ms"}
			o.Window = &ObservedWindow{Start: evaluationInstant, End: evaluationInstant}
		}, want: ErrorCodeInvalidObservation},
		{name: "non-canonical feature", mutate: func(o *VerificationObservation) { o.Feature = "Logging" }, want: ErrorCodeUnknownFeature},
		{name: "non-canonical requirement", mutate: func(o *VerificationObservation) { o.Requirement = "Life Cycle" }, want: ErrorCodeUnknownRequirement},
		{name: "non-canonical check", mutate: func(o *VerificationObservation) { o.Check = "Flush Visits" }, want: ErrorCodeInvalidObservation},
		{name: "escaping provenance", mutate: func(o *VerificationObservation) { o.Provenance.Path = "../secrets/logger_test.go" }, want: ErrorCodePathEscape},
		{name: "absolute provenance", mutate: func(o *VerificationObservation) { o.Provenance.Path = "/Users/alice/logger_test.go" }, want: ErrorCodeInvalidPath},
	} {
		t.Run(test.name, func(t *testing.T) {
			observation := valid
			test.mutate(&observation)
			findings := ValidateVerificationReport(&VerificationReport{ProtocolVersion: 1, Observations: []VerificationObservation{observation}})
			if !hasCode(findings, test.want) {
				t.Fatalf("invalid observation accepted: %v", findings)
			}
			if joined := strings.Join(signatures(findings), "\n"); strings.Contains(joined, "/Users/alice") || strings.Contains(joined, "secrets") {
				t.Fatalf("diagnostics leaked a raw path: %s", joined)
			}
		})
	}

	duplicate := []VerificationObservation{valid, valid}
	if findings := ValidateVerificationReport(&VerificationReport{ProtocolVersion: 1, Observations: duplicate}); !hasCode(findings, ErrorCodeDuplicateObservation) {
		t.Fatalf("duplicate observation accepted: %v", findings)
	}

	oversized := make([]VerificationObservation, VerificationReportMaxObservations+1)
	for i := range oversized {
		oversized[i] = valid
		oversized[i].Check = "check-" + strings.Repeat("a", i%16+1)
	}
	if findings := ValidateVerificationReport(&VerificationReport{ProtocolVersion: 1, Observations: oversized}); !hasCode(findings, ErrorCodeInvalidObservation) {
		t.Fatal("unbounded report accepted")
	}
}

func TestMeasuredObservationBoundaries(t *testing.T) {
	measurement := ObservationMeasurement{Name: "logging.delivery.success", Aggregation: AggregationRatio, Value: 0.9994, Unit: "ratio"}
	window := ObservedWindow{Start: "2026-07-19T10:00:00Z", End: evaluationInstant}
	valid := measuredObservation("delivery-slo", "production-delivery-slo", measurement, window, "production")
	if findings := ValidateVerificationReport(&VerificationReport{ProtocolVersion: 1, Observations: []VerificationObservation{valid}}); diag.HasErrors(findings) {
		t.Fatalf("valid measured observation rejected: %v", findings)
	}

	for _, test := range []struct {
		name   string
		mutate func(*VerificationObservation)
	}{
		{name: "no window", mutate: func(o *VerificationObservation) { o.Window = nil }},
		{name: "unparsable window", mutate: func(o *VerificationObservation) { o.Window.Start = "2026-07-19" }},
		{name: "inverted window", mutate: func(o *VerificationObservation) {
			o.Window.Start, o.Window.End = o.Window.End, o.Window.Start
		}},
		{name: "unknown aggregation", mutate: func(o *VerificationObservation) { o.Measurement.Aggregation = "p999" }},
		{name: "non-finite value", mutate: func(o *VerificationObservation) { o.Measurement.Value = math.Inf(1) }},
		{name: "NaN value", mutate: func(o *VerificationObservation) { o.Measurement.Value = math.NaN() }},
		{name: "non-semantic metric", mutate: func(o *VerificationObservation) { o.Measurement.Name = "Delivery Success" }},
		{name: "non-semantic unit", mutate: func(o *VerificationObservation) { o.Measurement.Unit = "Ratio" }},
		{name: "non-semantic environment", mutate: func(o *VerificationObservation) { o.Environment = "tenant Customer-42" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			observation := valid
			copiedMeasurement, copiedWindow := measurement, window
			observation.Measurement, observation.Window = &copiedMeasurement, &copiedWindow
			test.mutate(&observation)
			findings := ValidateVerificationReport(&VerificationReport{ProtocolVersion: 1, Observations: []VerificationObservation{observation}})
			if !hasCode(findings, ErrorCodeInvalidObservation) {
				t.Fatalf("invalid measured observation accepted: %v", findings)
			}
			if strings.Contains(strings.Join(signatures(findings), "\n"), "Customer-42") {
				t.Fatal("diagnostics leaked tenant data")
			}
		})
	}
}

func TestVerificationCanonicalizationIsDeterministicAndNonMutating(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("fixtures", "equivalence", "go-typescript-verification.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	report, findings := ParseAndValidateVerificationReport(data)
	if report == nil || diag.HasErrors(findings) {
		t.Fatalf("golden: %v", findings)
	}
	slices.Reverse(report.Observations)
	before := append([]VerificationObservation(nil), report.Observations...)
	first, err := MarshalVerificationReport(report)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, data) {
		t.Fatalf("canonical bytes differ\n%s", first)
	}
	if !reflect.DeepEqual(before, report.Observations) {
		t.Fatal("canonicalization mutated caller observation order")
	}
	for range 100 {
		got, err := MarshalVerificationReport(report)
		if err != nil || !bytes.Equal(got, first) {
			t.Fatalf("unstable serialization: %v", err)
		}
	}

	// A criterion's expected checks are an unordered set, so canonicalization
	// sorts them without touching the caller's authored slice.
	manifest := verifiedManifest(acceptanceRequirement("flush-visits-every-sink", "close-visits-every-sink"))
	authored := append([]string(nil), manifest.Features[0].Requirements[0].Verification.Checks...)
	canonical := CanonicalManifest(manifest)
	if got := canonical.Features[0].Requirements[0].Verification.Checks; !slices.Equal(got, []string{"close-visits-every-sink", "flush-visits-every-sink"}) {
		t.Fatalf("checks are not canonically ordered: %v", got)
	}
	if !slices.Equal(manifest.Features[0].Requirements[0].Verification.Checks, authored) {
		t.Fatal("canonicalization mutated the caller-owned criterion")
	}
	canonical.Features[0].Requirements[0].Verification.Target = target(1)
	if manifest.Features[0].Requirements[0].Verification.Target != nil {
		t.Fatal("canonicalization shared the caller-owned criterion pointer")
	}
}

func TestVerificationReportFieldSetMatchesThePublishedSchema(t *testing.T) {
	wantReport := []string{"$schema", "protocolVersion", "observations"}
	if got := jsonFieldNames(t, VerificationReport{}); !slices.Equal(got, wantReport) {
		t.Fatalf("report field set drifted: %v", got)
	}
	wantObservation := []string{"feature", "requirement", "check", "status", "measurement", "window", "environment", "provenance"}
	if got := jsonFieldNames(t, VerificationObservation{}); !slices.Equal(got, wantObservation) {
		t.Fatalf("observation field set drifted: %v", got)
	}
	// The payload deliberately cannot name its own authority or objective.
	for _, forbidden := range []string{"issuer", "project", "source", "binding", "target", "verdict", "outcome", "stage"} {
		if slices.Contains(wantObservation, forbidden) {
			t.Fatalf("an observation must not carry %q", forbidden)
		}
	}
	if got := jsonFieldNames(t, ObservationMeasurement{}); !slices.Equal(got, []string{"name", "aggregation", "value", "unit"}) {
		t.Fatalf("measurement field set drifted: %v", got)
	}
	if got := jsonFieldNames(t, ObservationProvenance{}); !slices.Equal(got, []string{"path", "symbol"}) {
		t.Fatalf("observation provenance field set drifted: %v", got)
	}
	if got := jsonFieldNames(t, VerificationCriterion{}); !slices.Equal(got, []string{
		"kind", "checks", "metric", "aggregation", "operator", "target", "unit", "window", "environment", "maxAgeSeconds",
	}) {
		t.Fatalf("criterion field set drifted: %v", got)
	}

	assertSchemaProperties(t, filepath.Join("schemas", "putnami-feature-verification.json"), VerificationReportSchemaURL, wantReport)
	assertDefProperties(t, filepath.Join("schemas", "putnami-feature-verification.json"), "observation", wantObservation)
	assertDefProperties(t, filepath.Join("schemas", "putnami-features.json"), "verification", jsonFieldNames(t, VerificationCriterion{}))
}

func assertSchemaProperties(t *testing.T, filename, wantID string, want []string) {
	t.Helper()
	//nolint:gosec // the test supplies its own committed schema path
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		ID         string                     `json:"$id"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.ID != wantID {
		t.Fatalf("schema $id %q does not match the published URL", schema.ID)
	}
	assertSameNames(t, schema.Properties, want)
}

func assertDefProperties(t *testing.T, filename, definition string, want []string) {
	t.Helper()
	//nolint:gosec // the test supplies its own committed schema path
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	def, found := schema.Defs[definition]
	if !found {
		t.Fatalf("schema %s has no %q definition", filename, definition)
	}
	assertSameNames(t, def.Properties, want)
}

func assertSameNames(t *testing.T, properties map[string]json.RawMessage, want []string) {
	t.Helper()
	published := make([]string, 0, len(properties))
	for name := range properties {
		published = append(published, name)
	}
	slices.Sort(published)
	expected := append([]string(nil), want...)
	slices.Sort(expected)
	if !slices.Equal(published, expected) {
		t.Fatalf("schema properties diverged from the Go wire type: %v, want %v", published, expected)
	}
}

func TestEvaluationJoinsOnRequirementIdentity(t *testing.T) {
	requirements := []Requirement{
		acceptanceRequirement("flush-visits-every-sink"),
		{ID: "sink-wiring", Stage: MaturityWired, EvidenceKinds: []EvidenceKind{EvidenceKindCapability}},
	}
	spec := []SpecRequirement{
		{ID: "lifecycle", Text: "Flushing a logger visits every configured sink."},
		{ID: "sink-wiring", Text: "A configured sink is wired before the first record."},
		{ID: "retention", Text: "A record stays retrievable for the configured retention period."},
	}
	observations := []VerificationObservation{statusObservation("lifecycle", "flush-visits-every-sink", ObservationPassed)}

	results := EvaluateSpecRequirements("logging/structured-logging", spec, requirements, observations, evaluatedAt(t))
	want := map[string]RequirementVerificationState{
		"lifecycle":   RequirementVerified,
		"sink-wiring": RequirementUnexecutable,
		"retention":   RequirementUnmapped,
	}
	if len(results) != len(want) {
		t.Fatalf("evaluated %d requirements", len(results))
	}
	for _, result := range results {
		if result.State != want[result.Requirement] {
			t.Errorf("%s = %q, want %q", result.Requirement, result.State, want[result.Requirement])
		}
	}
	if !slices.IsSortedFunc(results, func(a, b SpecRequirementEvaluation) int { return strings.Compare(a.Requirement, b.Requirement) }) {
		t.Fatal("results are not deterministically ordered")
	}

	// An authored requirement without a textual counterpart is allowed and is
	// never invented into a spec statement.
	if len(results) != 3 || results[0].Requirement != "lifecycle" {
		t.Fatalf("unexpected join: %+v", results)
	}

	shuffled := EvaluateSpecRequirements("logging/structured-logging", []SpecRequirement{spec[2], spec[0], spec[1]}, requirements, observations, evaluatedAt(t))
	if !reflect.DeepEqual(results, shuffled) {
		t.Fatal("authored order changed the evaluation")
	}
}

func TestAcceptanceEvaluationRequiresEveryDeclaredCheck(t *testing.T) {
	requirement := acceptanceRequirement("close-visits-every-sink", "flush-visits-every-sink")
	for _, test := range []struct {
		name         string
		observations []VerificationObservation
		want         RequirementVerificationState
		wantReason   string
	}{
		{
			name: "every check passes",
			observations: []VerificationObservation{
				statusObservation("lifecycle", "close-visits-every-sink", ObservationPassed),
				statusObservation("lifecycle", "flush-visits-every-sink", ObservationPassed),
			},
			want: RequirementVerified, wantReason: ReasonCheckPassed,
		},
		{
			name:         "one passing check cannot hide a missing one",
			observations: []VerificationObservation{statusObservation("lifecycle", "close-visits-every-sink", ObservationPassed)},
			want:         RequirementMissing, wantReason: ReasonCheckNotObserved,
		},
		{
			name: "a skipped check is missing",
			observations: []VerificationObservation{
				statusObservation("lifecycle", "close-visits-every-sink", ObservationPassed),
				statusObservation("lifecycle", "flush-visits-every-sink", ObservationSkipped),
			},
			want: RequirementMissing, wantReason: ReasonCheckSkipped,
		},
		{
			name: "any active contradiction wins",
			observations: []VerificationObservation{
				statusObservation("lifecycle", "close-visits-every-sink", ObservationFailed),
			},
			want: RequirementContradicted, wantReason: ReasonCheckFailed,
		},
		{
			name: "a duplicated check never resolves itself",
			observations: []VerificationObservation{
				statusObservation("lifecycle", "close-visits-every-sink", ObservationPassed),
				statusObservation("lifecycle", "close-visits-every-sink", ObservationFailed),
				statusObservation("lifecycle", "flush-visits-every-sink", ObservationPassed),
			},
			want: RequirementMissing, wantReason: ReasonDuplicateObservation,
		},
		{
			name:         "no observation at all",
			observations: nil,
			want:         RequirementMissing, wantReason: ReasonCheckNotObserved,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			evaluation := EvaluateRequirement("logging/structured-logging", requirement, test.observations, evaluatedAt(t))
			if evaluation.State != test.want {
				t.Fatalf("state = %q, want %q; checks: %+v", evaluation.State, test.want, evaluation.Checks)
			}
			reasons := make([]string, 0, len(evaluation.Checks))
			for _, check := range evaluation.Checks {
				reasons = append(reasons, check.Reason)
			}
			if !slices.Contains(reasons, test.wantReason) {
				t.Fatalf("reasons = %v, want %q", reasons, test.wantReason)
			}
			if !slices.IsSorted([]string{evaluation.Checks[0].Check, evaluation.Checks[len(evaluation.Checks)-1].Check}) {
				t.Fatal("checks are not deterministically ordered")
			}
		})
	}

	// An observation for a check no criterion declares is reported but never
	// grants support, and the declared checks still decide the state.
	observations := []VerificationObservation{
		statusObservation("lifecycle", "close-visits-every-sink", ObservationPassed),
		statusObservation("lifecycle", "flush-visits-every-sink", ObservationPassed),
		statusObservation("lifecycle", "undeclared-side-effect", ObservationPassed),
	}
	evaluation := EvaluateRequirement("logging/structured-logging", requirement, observations, evaluatedAt(t))
	if evaluation.State != RequirementVerified {
		t.Fatalf("undeclared observation changed the state: %q", evaluation.State)
	}
	if len(evaluation.Unexpected) != 1 || evaluation.Unexpected[0].Reason != ReasonUndeclaredCheck {
		t.Fatalf("undeclared observation was not reported: %+v", evaluation.Unexpected)
	}
	if _, granted := evaluation.Unexpected[0].EvidenceOutcome(); granted {
		t.Fatal("an undeclared observation granted evidence")
	}

	// Observations belonging to another feature or requirement never leak in.
	foreign := statusObservation("lifecycle", "close-visits-every-sink", ObservationPassed)
	foreign.Feature = "logging/flush-performance"
	if got := EvaluateRequirement("logging/structured-logging", requirement, []VerificationObservation{foreign}, evaluatedAt(t)); got.State != RequirementMissing {
		t.Fatalf("a foreign observation was joined: %q", got.State)
	}
}

func TestThresholdEvaluationRecomputesTheAuthoredObjective(t *testing.T) {
	requirement := invocationThresholdRequirement()
	window := ObservedWindow{Start: "2026-08-18T09:59:00Z", End: evaluationInstant}
	measured := func(value float64) []VerificationObservation {
		return []VerificationObservation{measuredObservation("flush-latency", "flush-benchmark",
			ObservationMeasurement{Name: "logger.flush.duration", Aggregation: AggregationP95, Value: value, Unit: "ms"}, window, "")}
	}

	for _, test := range []struct {
		name     string
		operator VerificationOperator
		target   float64
		value    float64
		want     RequirementVerificationState
	}{
		{name: "below the ceiling", operator: OperatorLte, target: 25, value: 18.5, want: RequirementVerified},
		{name: "exactly at the ceiling", operator: OperatorLte, target: 25, value: 25, want: RequirementVerified},
		{name: "above the ceiling", operator: OperatorLte, target: 25, value: 25.000001, want: RequirementContradicted},
		{name: "exactly at the floor", operator: OperatorGte, target: 0.999, value: 0.999, want: RequirementVerified},
		{name: "below the floor", operator: OperatorGte, target: 0.999, value: 0.9989, want: RequirementContradicted},
		{name: "exact equality", operator: OperatorEq, target: 0, value: 0, want: RequirementVerified},
		{name: "near equality is not equality", operator: OperatorEq, target: 0, value: 1e-12, want: RequirementContradicted},
		{name: "negative objective met", operator: OperatorLte, target: -3, value: -4, want: RequirementVerified},
	} {
		t.Run(test.name, func(t *testing.T) {
			criterion := *requirement.Verification
			criterion.Operator, criterion.Target = test.operator, target(test.target)
			scoped := requirement
			scoped.Verification = &criterion
			evaluation := EvaluateRequirement("logging/structured-logging", scoped, measured(test.value), evaluatedAt(t))
			if evaluation.State != test.want {
				t.Fatalf("state = %q, want %q", evaluation.State, test.want)
			}
		})
	}

	// A producer may report that a benchmark did not run, but never that a
	// numeric objective passed: only the observed value can settle that.
	selfDeclared := []VerificationObservation{statusObservation("flush-latency", "flush-benchmark", ObservationPassed)}
	evaluation := EvaluateRequirement("logging/structured-logging", requirement, selfDeclared, evaluatedAt(t))
	if evaluation.State != RequirementMissing || evaluation.Checks[0].Reason != ReasonMeasurementMissing {
		t.Fatalf("a self-declared objective verdict was accepted: %q %+v", evaluation.State, evaluation.Checks)
	}
	skipped := []VerificationObservation{statusObservation("flush-latency", "flush-benchmark", ObservationSkipped)}
	if got := EvaluateRequirement("logging/structured-logging", requirement, skipped, evaluatedAt(t)); got.Checks[0].Reason != ReasonCheckSkipped {
		t.Fatalf("a skipped benchmark was not reported as skipped: %+v", got.Checks)
	}

	for _, test := range []struct {
		name       string
		mutate     func(*ObservationMeasurement)
		wantReason string
	}{
		{name: "another metric", mutate: func(m *ObservationMeasurement) { m.Name = "logger.write.duration" }, wantReason: ReasonMetricMismatch},
		{name: "another aggregation", mutate: func(m *ObservationMeasurement) { m.Aggregation = AggregationAvg }, wantReason: ReasonAggregationMismatch},
		{name: "another unit", mutate: func(m *ObservationMeasurement) { m.Unit = "s" }, wantReason: ReasonUnitMismatch},
	} {
		t.Run(test.name, func(t *testing.T) {
			observations := measured(1)
			test.mutate(observations[0].Measurement)
			evaluation := EvaluateRequirement("logging/structured-logging", requirement, observations, evaluatedAt(t))
			if evaluation.State != RequirementMissing || evaluation.Checks[0].Reason != test.wantReason {
				t.Fatalf("state = %q reason = %q, want missing/%s", evaluation.State, evaluation.Checks[0].Reason, test.wantReason)
			}
			if _, granted := evaluation.Checks[0].EvidenceOutcome(); granted {
				t.Fatal("a mismatched measurement granted evidence")
			}
		})
	}
}

func TestRollingEvaluationEnforcesWindowEnvironmentAndFreshness(t *testing.T) {
	requirement := rollingThresholdRequirement()
	measurement := ObservationMeasurement{Name: "logging.delivery.success", Aggregation: AggregationRatio, Value: 0.9994, Unit: "ratio"}
	observe := func(start, end, environment string) []VerificationObservation {
		return []VerificationObservation{measuredObservation("delivery-slo", "production-delivery-slo", measurement,
			ObservedWindow{Start: start, End: end}, environment)}
	}
	now := evaluatedAt(t)

	for _, test := range []struct {
		name       string
		start      string
		end        string
		env        string
		instant    time.Time
		want       RequirementVerificationState
		wantReason string
	}{
		{
			name: "current production window", start: "2026-07-19T10:00:00Z", end: evaluationInstant,
			env: "production", instant: now, want: RequirementVerified, wantReason: ReasonObjectiveMet,
		},
		{
			name: "exactly at the freshness bound", start: "2026-07-19T09:00:00Z", end: "2026-08-18T09:00:00Z",
			env: "production", instant: now, want: RequirementVerified, wantReason: ReasonObjectiveMet,
		},
		{
			name: "one second past the freshness bound", start: "2026-07-19T08:59:59Z", end: "2026-08-18T08:59:59Z",
			env: "production", instant: now, want: RequirementStale, wantReason: ReasonObservationExpired,
		},
		{
			name: "window shorter than declared", start: "2026-08-11T10:00:00Z", end: evaluationInstant,
			env: "production", instant: now, want: RequirementStale, wantReason: ReasonWindowTooShort,
		},
		{
			name: "another environment", start: "2026-07-19T10:00:00Z", end: evaluationInstant,
			env: "staging", instant: now, want: RequirementStale, wantReason: ReasonEnvironmentMismatch,
		},
		{
			name: "observed after the evaluation instant", start: "2026-07-19T10:00:00Z", end: "2026-08-18T10:00:01Z",
			env: "production", instant: now, want: RequirementStale, wantReason: ReasonObservationAhead,
		},
		{
			name: "no evaluation clock", start: "2026-07-19T10:00:00Z", end: evaluationInstant,
			env: "production", instant: time.Time{}, want: RequirementStale, wantReason: ReasonEvaluationInstantMissing,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			evaluation := EvaluateRequirement("logging/structured-logging", requirement, observe(test.start, test.end, test.env), test.instant)
			if evaluation.State != test.want || evaluation.Checks[0].Reason != test.wantReason {
				t.Fatalf("state = %q reason = %q, want %q/%s", evaluation.State, evaluation.Checks[0].Reason, test.want, test.wantReason)
			}
		})
	}

	// A current window whose objective is missed is a contradiction, not stale.
	missed := observe("2026-07-19T10:00:00Z", evaluationInstant, "production")
	missed[0].Measurement = &ObservationMeasurement{Name: measurement.Name, Aggregation: measurement.Aggregation, Value: 0.99, Unit: measurement.Unit}
	if got := EvaluateRequirement("logging/structured-logging", requirement, missed, now); got.State != RequirementContradicted {
		t.Fatalf("a missed current objective = %q", got.State)
	}

	// An unparsable window cannot be shown to cover the declared duration.
	unusable := observe("2026-07-19", evaluationInstant, "production")
	if got := EvaluateRequirement("logging/structured-logging", requirement, unusable, now); got.State != RequirementStale || got.Checks[0].Reason != ReasonWindowUnusable {
		t.Fatalf("unusable window = %q/%q", got.State, got.Checks[0].Reason)
	}
}

func TestAnUnauthoredObjectiveNeverProducesAVerdict(t *testing.T) {
	// Manifest validation refuses a threshold without a target. A caller that
	// builds one directly must still get nothing rather than a verdict against
	// an objective nobody authored.
	requirement := invocationThresholdRequirement()
	requirement.Verification.Target = nil
	observations := []VerificationObservation{measuredObservation("flush-latency", "flush-benchmark",
		ObservationMeasurement{Name: "logger.flush.duration", Aggregation: AggregationP95, Value: 1, Unit: "ms"},
		ObservedWindow{Start: "2026-08-18T09:59:00Z", End: evaluationInstant}, "")}

	evaluation := EvaluateRequirement("logging/structured-logging", requirement, observations, evaluatedAt(t))
	if evaluation.State != RequirementMissing || evaluation.Checks[0].Reason != ReasonObjectiveUnauthored {
		t.Fatalf("state = %q reason = %q", evaluation.State, evaluation.Checks[0].Reason)
	}
	if _, granted := evaluation.Checks[0].EvidenceOutcome(); granted {
		t.Fatal("an unauthored objective granted evidence")
	}
}

func TestCheckStatesMapOntoTheDurableEvidenceVocabulary(t *testing.T) {
	for _, test := range []struct {
		state       CheckState
		wantOutcome EvidenceOutcome
		wantGranted bool
	}{
		{state: CheckSatisfied, wantOutcome: EvidenceOutcomeSupports, wantGranted: true},
		{state: CheckViolated, wantOutcome: EvidenceOutcomeContradicts, wantGranted: true},
		{state: CheckMissing},
		{state: CheckStale},
		{state: CheckUnclassified},
	} {
		outcome, granted := CheckEvaluation{State: test.state}.EvidenceOutcome()
		if granted != test.wantGranted || outcome != test.wantOutcome {
			t.Errorf("%q = (%q, %v)", test.state, outcome, granted)
		}
	}

	// An adapter records an automated claim, never a human product promise.
	acceptance := VerificationCriterion{Kind: VerificationKindAcceptance}
	threshold := VerificationCriterion{Kind: VerificationKindThreshold}
	if acceptance.AttestationClaim() != AttestationClaimAcceptance || threshold.AttestationClaim() != AttestationClaimObjectiveMet {
		t.Fatal("automated attestation claims drifted")
	}
	for _, claim := range []AttestationClaim{AttestationClaimAcceptance, AttestationClaimObjectiveMet} {
		if productPromiseClaims[claim] {
			t.Fatalf("automated claim %q requires human authority", claim)
		}
		record := basicEvidence()
		record.Subject = EvidenceSubject{Kind: EvidenceKindAttestation, Attestation: &AttestationSubject{Claim: claim}}
		if findings := ValidateEvidenceDocument(&EvidenceDocument{ProtocolVersion: EvidenceProtocolVersion, Evidence: []EvidenceRecord{record}}); diag.HasErrors(findings) {
			t.Fatalf("automated attestation %q rejected: %v", claim, findings)
		}
	}
}

func TestVerificationDiagnosticCodesAreReserved(t *testing.T) {
	for _, code := range []string{ErrorCodeInvalidVerification, ErrorCodeInvalidObservation, ErrorCodeDuplicateObservation} {
		if !ValidDiagnosticCodes[code] {
			t.Errorf("diagnostic code %q is not in the reserved automation vocabulary", code)
		}
		if !strings.HasPrefix(code, "features.") {
			t.Errorf("diagnostic code %q leaves the features vocabulary", code)
		}
	}
}
