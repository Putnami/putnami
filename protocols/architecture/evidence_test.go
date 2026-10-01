package architecture

import (
	"go.putnami.dev/protocol/features/spectest"

	"testing"
)

// Framework evidence: what a build recorded about the contracts a project's
// components enforce, joined to the declarations those contracts claim.
//
// Every test here states which of the two asymmetric rules it exercises, because
// the asymmetry is the design: evidence proves something exists, and its absence
// proves nothing on its own.

// evidenceFor builds one record for the observability domain, which is the only
// consuming domain the cloud-pilot fixtures declare.
func evidenceFor(project, importID string, mode AccessMode, transports ...EvidenceTransport) EvidenceRecord {
	return EvidenceRecord{
		Kind:            EvidenceFrameworkPrimitive,
		ConsumerDomain:  "observability",
		ConsumerProject: project,
		Import:          importID,
		Mode:            mode,
		Transports:      transports,
	}
}

// TestEvidenceWithoutDeclarationAlwaysFails is the direction that needs no
// opt-in: a record proves a component exists, so a record no declaration matches
// is code enforcing a contract nobody reviewed.
func TestEvidenceWithoutDeclarationAlwaysFails(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "framework-evidence",
		"an-implementation-nobody-declared-is-a-finding")
	runtime, observability := activePilot(t)
	graph := BuildGraph(pilotSources(runtime, observability))
	declared := observability.Imports[0]

	stray := findingFor(t, CompareEvidence(graph, []EvidenceRecord{
		evidenceFor("/observability/workloads/telemetry-api", "observability.invented.v1", ModeProjection),
	}), ErrorCodeEvidenceWithoutDeclaration)
	if stray.Evidence == nil || stray.Evidence.ConsumerProject == "" {
		t.Errorf("finding = %+v, want the record that caused it, project included", stray)
	}

	// The same import declared with ANOTHER mode is the drift this catches: a
	// contract copied into code and later changed in the manifest. Matching on
	// the import alone would call that agreement.
	drift := findingFor(t, CompareEvidence(graph, []EvidenceRecord{
		evidenceFor("/observability/workloads/telemetry-api", declared.ID, ModeQuery),
	}), ErrorCodeEvidenceWithoutDeclaration)
	if drift.Evidence == nil || drift.Evidence.Mode != ModeQuery {
		t.Errorf("finding = %+v, want the record whose mode disagrees with the declaration", drift)
	}

	// A record for the declaration as written is agreement, and produces nothing.
	if findings := CompareEvidence(graph, []EvidenceRecord{
		evidenceFor("/observability/workloads/telemetry-api", declared.ID, declared.Mode),
	}); len(findings) != 0 {
		t.Errorf("findings = %+v, want none when the record matches the declaration", findings)
	}
}

// TestDeclaredWithoutEvidenceNeedsAnAdoptedDomain is the asymmetry, stated as a
// test. A domain that emits no evidence is outside evidence coverage entirely —
// demanding an implementation from it would report every honest declaration in a
// repository as a violation. Once a domain implements one import, the rest of
// its ACTIVE imports are expected too.
func TestDeclaredWithoutEvidenceNeedsAnAdoptedDomain(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "framework-evidence",
		"a-declaration-is-only-expected-to-be-implemented-inside-an-adopting-domain")
	runtime, observability := activePilot(t)
	// A second active import, so the domain can implement one and not the other.
	second := observability.Imports[0]
	second.ID = "observability.runtime-region.v1"
	second.Mode = ModeReference
	second.Bootstrap, second.Updates, second.Consistency, second.Deletion, second.LocalModel = nil, nil, nil, nil, nil
	second.Bindings = nil
	second.Facts = []string{"region"}
	observability.Imports = append(observability.Imports, second)
	graph := BuildGraph(pilotSources(runtime, observability))

	if findings := CompareEvidence(graph, nil); len(findings) != 0 {
		t.Fatalf("findings = %+v, want none: a domain that emits no evidence is outside coverage", findings)
	}

	findings := CompareEvidence(graph, []EvidenceRecord{
		evidenceFor("/observability/workloads/telemetry-api", observability.Imports[0].ID, observability.Imports[0].Mode),
	})
	if len(findings) != 1 || findings[0].Code != ErrorCodeDeclaredWithoutEvidence {
		t.Fatalf("findings = %+v, want one %s for the unimplemented sibling", findings, ErrorCodeDeclaredWithoutEvidence)
	}
	expected := findings[0].Evidence
	if expected == nil || expected.Import != "observability.runtime-region.v1" || expected.Mode != ModeReference {
		t.Errorf("finding evidence = %+v, want the record that was expected and not found", expected)
	}
	if expected.ConsumerProject != "" {
		t.Errorf("expected record names project %q; which project should carry it is the domain's decision, not this comparison's",
			expected.ConsumerProject)
	}
}

// TestPlannedDeclarationsAreNotExpectedToBeImplemented pins that only ACTIVE
// contracts are held to evidence. A planned import is a target, and demanding an
// implementation of a target would make declaring one impossible.
func TestPlannedDeclarationsAreNotExpectedToBeImplemented(t *testing.T) {
	runtime, observability := loadPilot(t)
	runtime.Exports[0].Status = StatusActive
	graph := BuildGraph(pilotSources(runtime, observability))
	if observability.Imports[0].Status != StatusPlanned {
		t.Fatalf("fixture import status = %q, want planned", observability.Imports[0].Status)
	}
	findings := CompareEvidence(graph, []EvidenceRecord{
		evidenceFor("/observability/workloads/telemetry-api", "observability.other.v1", ModeReference),
	})
	for _, finding := range findings {
		if finding.Code == ErrorCodeDeclaredWithoutEvidence {
			t.Errorf("a planned import was expected to be implemented: %+v", finding)
		}
	}
}

// TestEvidenceCoverageNeverClaimsRuntimeObservation pins the honesty of the
// report. The tier is framework-evidence at most — a build recorded what a
// component was configured with — and a transport category nothing declared
// stays not-detected.
func TestEvidenceCoverageNeverClaimsRuntimeObservation(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "framework-evidence",
		"evidence-coverage-reports-the-framework-tier-and-never-more")
	empty := EvidenceCoverage(nil)
	if empty.DomainAccess != CoverageNotDetected || empty.HTTP != CoverageNotDetected || empty.Events != CoverageNotDetected {
		t.Fatalf("coverage = %+v, want everything not-detected without evidence", empty)
	}

	withAPI := EvidenceCoverage([]EvidenceRecord{
		evidenceFor("/a", "observability.x.v1", ModeQuery,
			EvidenceTransport{Role: "transport", Kind: TransportAPI, Availability: StatusActive}),
	})
	if withAPI.DomainAccess != CoverageFrameworkEvidence || withAPI.HTTP != CoverageFrameworkEvidence {
		t.Errorf("coverage = %+v, want the framework tier for domain access and HTTP", withAPI)
	}
	if withAPI.Events != CoverageNotDetected || withAPI.Database != CoverageNotDetected {
		t.Errorf("coverage = %+v, want categories nothing declared to stay not-detected", withAPI)
	}

	// A reference declares no carrier at all, so it moves the domain-access tier
	// and NOTHING else. Reporting HTTP coverage for it would claim an observation
	// nobody made.
	withReference := EvidenceCoverage([]EvidenceRecord{
		evidenceFor("/a", "observability.x.v1", ModeReference),
	})
	if withReference.DomainAccess != CoverageFrameworkEvidence {
		t.Errorf("coverage = %+v, want the framework tier once any implementation is recorded", withReference)
	}
	if withReference.HTTP != CoverageNotDetected || withReference.Events != CoverageNotDetected {
		t.Errorf("coverage = %+v, want no transport claim from a carrier-free contract", withReference)
	}
}

// TestEvidenceFindingsAreRatchetableAndDeterministic pins that an evidence
// finding is an ordinary finding: it carries a stable semantic ID a baseline or
// waiver can name, and the snapshot it lands in is order-independent.
func TestEvidenceFindingsAreRatchetableAndDeterministic(t *testing.T) {
	runtime, observability := activePilot(t)
	graph := BuildGraph(pilotSources(runtime, observability))
	stray := evidenceFor("/observability/workloads/telemetry-api", "observability.invented.v1", ModeProjection)
	other := evidenceFor("/observability/workloads/telemetry-api", "observability.also-invented.v1", ModeQuery)

	forward := BuildSnapshot(graph, Observations{Evidence: []EvidenceRecord{stray, other}}, nil, nil, RatchetOptions{})
	backward := BuildSnapshot(graph, Observations{Evidence: []EvidenceRecord{other, stray}}, nil, nil, RatchetOptions{})
	left, err := MarshalSnapshot(forward)
	if err != nil {
		t.Fatal(err)
	}
	right, err := MarshalSnapshot(backward)
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(right) {
		t.Fatalf("evidence order is observable in the snapshot:\n%s\n%s", left, right)
	}

	// A clean run with the declared binding observed and the declared import
	// implemented, plus one stray record: exactly one finding, and a baseline
	// entry naming its stable ID accepts it like any other classified violation.
	declared := observability.Imports[0]
	clean := Observations{
		Edges: []ObservedEdge{{
			Kind:            BindingProjectDependency,
			ProducerDomain:  "runtime",
			ConsumerDomain:  "observability",
			ProducerProject: runtime.Projects[0],
			ConsumerProject: observability.Projects[0],
		}},
		Evidence: []EvidenceRecord{
			evidenceFor(observability.Projects[0], declared.ID, declared.Mode),
			stray,
		},
	}
	if findings := BuildSnapshot(graph, clean, nil, nil, RatchetOptions{}).Findings; len(findings) != 1 {
		t.Fatalf("findings = %+v, want only the stray record", findings)
	}

	id := StableEvidenceFindingID(ErrorCodeEvidenceWithoutDeclaration, stray)
	baselined := BuildSnapshot(graph, clean, &Baseline{
		ProtocolVersion: ProtocolVersion,
		Findings: []DebtRecord{{
			Finding: id, Owner: "observability", Reason: "adoption debt", Scope: "one import", RemoveWhen: "the contract is declared",
		}},
	}, nil, RatchetOptions{})
	if len(baselined.Findings) != 1 || baselined.Findings[0].Disposition != DispositionKnownDebt {
		t.Fatalf("findings = %+v, want the evidence finding accepted as known debt", baselined.Findings)
	}
	if HasBlockingFindings(baselined.Findings) {
		t.Error("a baselined evidence finding still blocks; it must warn like every other classified violation")
	}
}

// findingFor asserts exactly one finding of code is present and returns it.
// Other codes may legitimately appear beside it — a domain that implements
// anything is held to its other active declarations too — so this narrows to the
// one the test is about instead of pinning the whole set.
func findingFor(t *testing.T, findings []Finding, code string) Finding {
	t.Helper()
	var matched []Finding
	for _, finding := range findings {
		if finding.Code == code {
			matched = append(matched, finding)
		}
	}
	if len(matched) != 1 {
		t.Fatalf("findings = %+v, want exactly one %s", findings, code)
	}
	return matched[0]
}
