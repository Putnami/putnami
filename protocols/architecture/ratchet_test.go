package architecture

import (
	"go.putnami.dev/protocol/features/spectest"

	"bytes"
	"testing"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestObservedEdgeMustHaveExactDeclaration(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "exact-drift", "an-observed-edge-needs-an-exact-consumer-to-producer-declaration")
	runtime, observability := loadPilot(t)
	graph := BuildGraph(pilotSources(runtime, observability))
	observed := []ObservedEdge{pilotObserved(runtime, observability)}
	findings := CompareObserved(graph, observed)
	if len(findings) != 1 || findings[0].Code != ErrorCodeUndeclaredProjectDependency || !HasBlockingFindings(findings) {
		t.Fatalf("undeclared findings = %#v", findings)
	}

	runtime, observability = activePilot(t)
	graph = BuildGraph(pilotSources(runtime, observability))
	if findings = CompareObserved(graph, observed); len(findings) != 0 {
		t.Fatalf("declared findings = %#v", findings)
	}
}

func TestRatchetAllowsKnownDebtButRejectsNewViolation(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "ratchet", "a-new-violation-fails-while-baselined-debt-stays-a-warning")
	runtime, observability := loadPilot(t)
	current := CompareObserved(BuildGraph(pilotSources(runtime, observability)), []ObservedEdge{pilotObserved(runtime, observability)})
	if len(current) != 1 {
		t.Fatalf("current findings = %#v", current)
	}
	if findings, summary := ApplyRatchet(current, nil, nil, RatchetOptions{}); !HasBlockingFindings(findings) || summary.New != 1 {
		t.Fatalf("new violation was not refused: %#v, %#v", findings, summary)
	}
	baseline := baselineFor(current[0].ID)
	findings, summary := ApplyRatchet(current, baseline, nil, RatchetOptions{})
	if HasBlockingFindings(findings) || len(findings) != 1 || findings[0].Severity != diag.Warning || summary.KnownDebt != 1 {
		t.Fatalf("known debt = %#v, %#v", findings, summary)
	}
}

func TestRatchetDetectsStaleBaselineAndPreventsGrowth(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "ratchet", "a-stale-baseline-entry-and-any-growth-fail")
	baseline := baselineFor("architecture.undeclared_project_dependency:old")
	findings, summary := ApplyRatchet(nil, baseline, nil, RatchetOptions{})
	if !HasBlockingFindings(findings) || summary.StaleBaseline != 1 {
		t.Fatalf("stale baseline = %#v, %#v", findings, summary)
	}

	previous := &Baseline{ProtocolVersion: 1, Findings: []DebtRecord{}}
	findings, summary = ApplyRatchet(nil, baseline, nil, RatchetOptions{
		PreviousBaseline:      previous,
		PreviousBaselineKnown: true,
	})
	if summary.BaselineGrowth != 1 {
		t.Fatalf("baseline growth = %#v, %#v", findings, summary)
	}
}

func TestExpiredAndStaleWaiversFail(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "ratchet", "an-expired-or-stale-waiver-fails")
	runtime, observability := loadPilot(t)
	current := CompareObserved(BuildGraph(pilotSources(runtime, observability)), []ObservedEdge{pilotObserved(runtime, observability)})
	waivers := &WaiverFile{ProtocolVersion: 1, Waivers: []DebtRecord{{
		Finding: current[0].ID, Owner: "observability", Reason: "transition", Scope: "exact edge", Expires: "2026-08-17",
	}}}
	findings, summary := ApplyRatchet(current, nil, waivers, RatchetOptions{Today: architectureTestDate(2026, 8, 18)})
	if !HasBlockingFindings(findings) || summary.ExpiredWaiver != 1 {
		t.Fatalf("expired waiver = %#v, %#v", findings, summary)
	}
	waivers.Waivers[0].Expires = "2026-08-19"
	findings, summary = ApplyRatchet(nil, nil, waivers, RatchetOptions{Today: architectureTestDate(2026, 8, 18)})
	if !HasBlockingFindings(findings) || summary.StaleWaiver != 1 {
		t.Fatalf("stale waiver = %#v, %#v", findings, summary)
	}
}

func TestExpiringWaiverFailsClosedWithoutEvaluationDate(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "ratchet", "an-expiring-waiver-fails-closed-without-an-evaluation-date")
	runtime, observability := loadPilot(t)
	current := CompareObserved(BuildGraph(pilotSources(runtime, observability)), []ObservedEdge{pilotObserved(runtime, observability)})
	waivers := &WaiverFile{ProtocolVersion: 1, Waivers: []DebtRecord{{
		Finding: current[0].ID, Owner: "observability", Reason: "transition", Scope: "exact edge", Expires: "1999-01-01",
	}}}

	findings, summary := ApplyRatchet(current, nil, waivers, RatchetOptions{})
	if !HasBlockingFindings(findings) || len(findings) != 1 || findings[0].Disposition != DispositionNew || summary.New != 1 || summary.Waived != 0 {
		t.Fatalf("waiver without evaluation date failed open: findings=%#v summary=%#v", findings, summary)
	}
	if !bytes.Contains([]byte(findings[0].Message), []byte("evaluation date is unavailable")) {
		t.Fatalf("finding does not explain why the waiver was refused: %q", findings[0].Message)
	}
}

func TestDebtCannotBeBothBaselinedAndWaived(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "ratchet", "debt-cannot-be-both-baselined-and-waived")
	const finding = "architecture.undeclared_project_dependency:exact-edge"
	baseline := baselineFor(finding)
	waivers := &WaiverFile{ProtocolVersion: 1, Waivers: []DebtRecord{{
		Finding: finding, Owner: "observability", Reason: "temporary", Scope: "exact edge", Expires: "2026-08-19",
	}}}
	diagnostics := ValidateDebtFiles(baseline, waivers)
	if len(diagnostics) != 1 || diagnostics[0].Code != ErrorCodeDuplicateDebtRecord || diagnostics[0].Field != "waivers[0].finding" {
		t.Fatalf("cross-file diagnostics = %+v", diagnostics)
	}
}

func TestSnapshotIsDeterministicAcrossSourceAndObservationOrder(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "single-interpretation", "canonicalization-makes-the-snapshot-order-independent")
	runtime, observability := activePilot(t)
	forward := pilotSources(runtime, observability)
	reverse := []ManifestSource{forward[1], forward[0]}
	observed := pilotObserved(runtime, observability)
	left := BuildSnapshot(BuildGraph(forward), Observations{Edges: []ObservedEdge{observed, observed}}, nil, nil, RatchetOptions{})
	right := BuildSnapshot(BuildGraph(reverse), Observations{Edges: []ObservedEdge{observed}}, nil, nil, RatchetOptions{})
	leftBytes, err := MarshalSnapshot(left)
	if err != nil {
		t.Fatal(err)
	}
	rightBytes, err := MarshalSnapshot(right)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(leftBytes, rightBytes) {
		t.Fatalf("snapshot order changed bytes:\n%s\n---\n%s", leftBytes, rightBytes)
	}
}

func pilotObserved(runtime, observability *Manifest) ObservedEdge {
	return ObservedEdge{
		Kind:            BindingProjectDependency,
		ProducerDomain:  runtime.Domain,
		ConsumerDomain:  observability.Domain,
		ProducerProject: runtime.Projects[0],
		ConsumerProject: observability.Projects[0],
	}
}

func baselineFor(finding string) *Baseline {
	return &Baseline{ProtocolVersion: 1, Findings: []DebtRecord{{
		Finding:    finding,
		Owner:      "observability",
		Reason:     "Known adoption debt.",
		Scope:      "The exact finding ID only.",
		RemoveWhen: "The declared projection replaces the legacy dependency.",
	}}}
}

func architectureTestDate(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 12, 0, 0, 0, time.UTC)
}
