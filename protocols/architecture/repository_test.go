package architecture

import (
	"go.putnami.dev/protocol/features/spectest"

	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestRepositoryRejectsUnknownDomainAndMissingExport(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "distributed-authority", "a-domain-owns-its-identity-and-its-half-of-a-contract")
	runtime, observability := loadPilot(t)
	observability.Imports[0].From.Domain = "identity"
	diagnostics := ValidateRepository(pilotSources(runtime, observability))
	if !hasDiagnostic(diagnostics, ErrorCodeUnknownDomain) {
		t.Fatalf("unknown-domain diagnostics = %s", diagnosticCodes(diagnostics))
	}

	runtime, observability = loadPilot(t)
	runtime.Exports = nil
	diagnostics = ValidateRepository(pilotSources(runtime, observability))
	if !hasDiagnostic(diagnostics, ErrorCodeUnknownExport) {
		t.Fatalf("missing-export diagnostics = %s", diagnosticCodes(diagnostics))
	}
}

func TestRepositoryRejectsDuplicateAndIncompatibleContracts(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "distributed-authority", "two-domains-cannot-author-incompatible-halves")
	runtime, observability := loadPilot(t)
	runtime.Exports = append(runtime.Exports, runtime.Exports[0])
	if diagnostics := ValidateRepository(pilotSources(runtime, observability)); !hasDiagnostic(diagnostics, ErrorCodeDuplicateExport) {
		t.Fatalf("duplicate-export diagnostics = %s", diagnosticCodes(diagnostics))
	}

	runtime, observability = loadPilot(t)
	runtime.Exports[0].Modes = []AccessMode{ModeQuery}
	if diagnostics := ValidateRepository(pilotSources(runtime, observability)); !hasDiagnostic(diagnostics, ErrorCodeIncompatibleImport) {
		t.Fatalf("incompatible-mode diagnostics = %s", diagnosticCodes(diagnostics))
	}
}

func TestRepositoryChecksExactBindingDirectionAndUniqueness(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "distributed-authority", "binding-direction-and-uniqueness-are-checked-at-the-source")
	runtime, observability := activePilot(t)
	if diagnostics := ValidateRepository(pilotSources(runtime, observability)); diag.HasErrors(diagnostics) {
		t.Fatalf("active pilot diagnostics: %v", diagnostics)
	}
	observability.Imports = append(observability.Imports, observability.Imports[0])
	observability.Imports[1].ID = "observability.second-runtime-context.v1"
	if diagnostics := ValidateRepository(pilotSources(runtime, observability)); !hasDiagnostic(diagnostics, ErrorCodeDuplicateBinding) {
		t.Fatalf("duplicate-binding diagnostics = %s", diagnosticCodes(diagnostics))
	}
}

func pilotSources(runtime, observability *Manifest) []ManifestSource {
	return []ManifestSource{
		{Path: "runtime/putnami.architecture.json", Manifest: runtime},
		{Path: "observability/putnami.architecture.json", Manifest: observability},
	}
}

func activePilot(t *testing.T) (*Manifest, *Manifest) {
	t.Helper()
	runtime, observability := loadPilot(t)
	runtime.Exports[0].Status = StatusActive
	imported := &observability.Imports[0]
	imported.Status = StatusActive
	imported.Bootstrap.Availability = StatusActive
	imported.Updates.Availability = StatusActive
	imported.Bindings = []Binding{{
		Kind:            BindingProjectDependency,
		ConsumerProject: observability.Projects[0],
		ProducerProject: runtime.Projects[0],
	}}
	return runtime, observability
}

// A planned target has not shipped, so it cannot already be bound to a
// concrete project pair — and a transport that is only declared is not
// evidence that anything was observed. Nothing exercised either rule: the
// ErrorCodeInvalidBinding branch in validate.go had no test, and the one
// fixture that happens to contain "planned" is there for a projection error.
func TestPlannedTargetCarriesNoObservedBinding(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "planned-is-not-observed", "a-planned-target-cannot-carry-a-current-project-binding")
	runtime, observability := activePilot(t)

	// The import keeps its binding but reverts to planned: still unshipped,
	// yet claiming a current consumer-to-producer project edge.
	observability.Imports[0].Status = StatusPlanned
	if diagnostics := ValidateRepository(pilotSources(runtime, observability)); !hasDiagnostic(diagnostics, ErrorCodeInvalidBinding) {
		t.Fatalf("planned import with a binding: diagnostics = %s, want %s", diagnosticCodes(diagnostics), ErrorCodeInvalidBinding)
	}

	// Dropping the binding makes the same planned import legitimate, which
	// pins that the diagnostic is about the binding and not about `planned`.
	observability.Imports[0].Bindings = nil
	if diagnostics := ValidateRepository(pilotSources(runtime, observability)); hasDiagnostic(diagnostics, ErrorCodeInvalidBinding) {
		t.Fatalf("planned import without a binding: unexpected %s in %s", ErrorCodeInvalidBinding, diagnosticCodes(diagnostics))
	}
}

// A declared transport is a statement of intent. The v1 snapshot must report
// database, HTTP and event observation as not detected regardless of what the
// manifests declare, so a reader cannot mistake a declaration for evidence.
func TestDeclaredTransportsCreateNoObservedCoverage(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "planned-is-not-observed", "a-declared-transport-creates-no-observed-evidence")
	spectest.Proves(t, "architecture/executable-contracts", "coverage", "database-http-and-event-observation-report-as-not-detected")
	runtime, observability := activePilot(t)
	sources := pilotSources(runtime, observability)
	if diagnostics := ValidateRepository(sources); diag.HasErrors(diagnostics) {
		t.Fatalf("active pilot diagnostics: %v", diagnostics)
	}
	graph := BuildGraph(sources)

	coverage := BuildSnapshot(graph, Observations{}, nil, nil, RatchetOptions{}).Coverage
	for name, got := range map[string]string{
		"database": coverage.Database,
		"http":     coverage.HTTP,
		"events":   coverage.Events,
	} {
		if got != "not-detected" {
			t.Errorf("%s observation = %q, want not-detected; v1 detects no runtime evidence", name, got)
		}
	}
}
