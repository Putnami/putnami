package features

import (
	"bytes"
	"sort"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func sampleVerificationRecord() *SpecVerificationRecord {
	return &SpecVerificationRecord{
		ProtocolVersion: SpecVerificationRecordProtocolVersion,
		SessionID:       "20260823-120000-abcdef",
		GeneratedAt:     "2026-08-23T12:00:00Z",
		Groups: []SpecVerificationGroup{
			{
				Project:             "/go/framework/logger",
				Feature:             "go/structured-logging",
				Spec:                "go/framework/logger/specs/structured-logging.json",
				Mode:                VerificationModeEnforce,
				ModeSource:          VerificationModeSourceProject,
				AutomaticEvaluation: true,
				Requirements: []SpecRequirementVerdict{
					{Requirement: "record", State: RequirementUnexecutable},
					{Requirement: "lifecycle", State: RequirementVerified, Checks: []SpecCheckVerdict{
						{Check: "flush-visits-every-sink", State: CheckSatisfied, Reason: ReasonCheckPassed,
							Path: "coverage_test.go", Symbol: "TestLoggerFlushVisitsEverySinkAndReturnsFirstError"},
						{Check: "close-visits-every-sink", State: CheckSatisfied, Reason: ReasonCheckPassed,
							Path: "coverage_test.go", Symbol: "TestLoggerCloseVisitsEverySinkAndReturnsFirstError"},
					}},
				},
				Reports: []VerificationReportRef{{
					Task: "/go/framework/logger:test", Project: "/go/framework/logger",
					Path:   ".putnami/out/go/framework/logger/test/putnami-feature-verification.json",
					Digest: "sha256:" + strings.Repeat("ab", 32),
				}},
			},
		},
	}
}

func TestSpecVerificationRecordRoundTripsCanonically(t *testing.T) {
	record := sampleVerificationRecord()
	record.Groups[0].Counts = CountSpecVerdicts(record.Groups[0].Requirements)
	first, err := MarshalSpecVerificationRecord(record)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	parsed, diagnostics := ParseSpecVerificationRecord(first)
	if parsed == nil {
		t.Fatalf("canonical bytes failed the strict reader: %v", diagnostics)
	}
	if findings := ValidateSpecVerificationRecord(parsed); diag.HasErrors(findings) {
		t.Fatalf("canonical record failed validation: %v", findings)
	}
	if parsed.Groups[0].Requirements[0].Requirement != "lifecycle" {
		t.Fatalf("requirements are not sorted: %+v", parsed.Groups[0].Requirements)
	}
	if parsed.Groups[0].Requirements[0].Checks[0].Check != "close-visits-every-sink" {
		t.Fatalf("checks are not sorted: %+v", parsed.Groups[0].Requirements[0].Checks)
	}
	second, err := MarshalSpecVerificationRecord(parsed)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("marshal is not byte-stable (err %v)", err)
	}
}

func TestSpecVerificationRecordRejectsBrokenDocuments(t *testing.T) {
	record := sampleVerificationRecord()
	encoded, err := MarshalSpecVerificationRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if parsed, _ := ParseSpecVerificationRecord(bytes.Replace(encoded,
		[]byte(`"protocolVersion": 1`), []byte(`"protocolVersion": 2`), 1)); parsed != nil {
		t.Error("a forward-versioned record was accepted")
	}
	if parsed, _ := ParseSpecVerificationRecord(bytes.Replace(encoded,
		[]byte(`"sessionId"`), []byte(`"verdict"`), 1)); parsed != nil {
		t.Error("a record carrying an undeclared field was accepted")
	}
	for name, mutate := range map[string]func(*SpecVerificationRecord){
		"missing groups":     func(r *SpecVerificationRecord) { r.Groups = nil },
		"bad generatedAt":    func(r *SpecVerificationRecord) { r.GeneratedAt = "yesterday" },
		"missing project":    func(r *SpecVerificationRecord) { r.Groups[0].Project = "" },
		"unknown mode":       func(r *SpecVerificationRecord) { r.Groups[0].Mode = "audit" },
		"duplicate group":    func(r *SpecVerificationRecord) { r.Groups = append(r.Groups, r.Groups[0]) },
		"wrong wire version": func(r *SpecVerificationRecord) { r.ProtocolVersion = 0 },
	} {
		broken := sampleVerificationRecord()
		mutate(broken)
		if findings := ValidateSpecVerificationRecord(broken); !diag.HasErrors(findings) {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestCountSpecVerdictsCoversEveryState(t *testing.T) {
	counts := CountSpecVerdicts([]SpecRequirementVerdict{
		{State: RequirementVerified}, {State: RequirementVerified},
		{State: RequirementUnmapped},
		{State: RequirementUnexecutable},
		{State: RequirementMissing},
		{State: RequirementStale},
		{State: RequirementContradicted},
		{State: RequirementUnobserved},
	})
	want := SpecVerificationCounts{SpecRequirements: 8, Executable: 6, Verified: 2,
		Unmapped: 1, Unexecutable: 1, Missing: 1, Stale: 1, Contradicted: 1, Unobserved: 1}
	if counts != want {
		t.Fatalf("counts = %+v, want %+v", counts, want)
	}
	// Every declared state has a bucket. A state nobody counts would silently
	// vanish from every roll-up the moment it is introduced.
	total := CountSpecVerdicts(verdictOfEveryState())
	if total.Verified+total.Unmapped+total.Unexecutable+total.Missing+
		total.Stale+total.Contradicted+total.Unobserved != total.SpecRequirements {
		t.Fatalf("some declared state has no bucket: %+v", total)
	}
}

func verdictOfEveryState() []SpecRequirementVerdict {
	states := make([]RequirementVerificationState, 0, len(ValidRequirementVerificationStates))
	for state := range ValidRequirementVerificationStates {
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i] < states[j] })
	verdicts := make([]SpecRequirementVerdict, 0, len(states))
	for _, state := range states {
		verdicts = append(verdicts, SpecRequirementVerdict{State: state})
	}
	return verdicts
}

// TestUnobservedIsTheOneNonVerifiedStateThatDoesNotBlock pins the protocol
// boundary: a requirement whose checks NO source could report is
// reported and warned, never sanctioned, while every state that rests on an
// observation that did arrive keeps blocking. The record's strict reader also
// has to accept it, or the gate would write a document its own audit surface
// refuses.
func TestUnobservedIsTheOneNonVerifiedStateThatDoesNotBlock(t *testing.T) {
	mixed := []SpecRequirementVerdict{{State: RequirementVerified}, {State: RequirementUnobserved}}
	if GroupBlocks(VerificationModeEnforce, mixed, nil) {
		t.Error("enforce sanctioned a requirement no source could observe")
	}
	// It is still not verified: a surface that reports "all verified" from the
	// absence of blocking would be wrong.
	if counts := CountSpecVerdicts(mixed); counts.Verified != 1 || counts.Unobserved != 1 {
		t.Errorf("counts = %+v, want one verified and one unobserved", counts)
	}
	// An error finding over the group's own inputs still blocks it.
	broken := []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidVerification, "x", "unreadable")}
	if !GroupBlocks(VerificationModeEnforce, mixed, broken) {
		t.Error("an unreadable input stopped blocking because the group was unobserved")
	}

	record := &SpecVerificationRecord{
		ProtocolVersion: SpecVerificationRecordProtocolVersion,
		GeneratedAt:     "2026-09-09T12:00:00Z",
		Groups: []SpecVerificationGroup{{
			Project: "/workload", Feature: "gate/workload", Mode: VerificationModeEnforce,
			Requirements: mixed,
			Reports: []VerificationReportRef{{
				Task: "/library:test~test", Project: "/library",
				Path: "cache/blob/report", Digest: "sha256:00", Restored: true,
			}},
		}},
	}
	if findings := ValidateSpecVerificationRecord(record); diag.HasErrors(findings) {
		t.Fatalf("the strict reader refused a record carrying the unobserved state: %+v", findings)
	}
	data, err := MarshalSpecVerificationRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	parsed, findings := ParseSpecVerificationRecord(data)
	if parsed == nil || diag.HasErrors(findings) {
		t.Fatalf("round trip failed: %+v", findings)
	}
	if !parsed.Groups[0].Reports[0].Restored {
		t.Error("the restored marker did not survive the wire")
	}
}

// TestRecordRejectsAStateNobodyDeclared is the other half: the vocabulary is
// closed, so a document carrying an invented state is refused rather than
// silently ignored by a renderer's switch.
func TestRecordRejectsAStateNobodyDeclared(t *testing.T) {
	record := &SpecVerificationRecord{
		ProtocolVersion: SpecVerificationRecordProtocolVersion,
		GeneratedAt:     "2026-09-09T12:00:00Z",
		Groups: []SpecVerificationGroup{{
			Project: "/workload", Mode: VerificationModeEnforce,
			Requirements: []SpecRequirementVerdict{{Requirement: "x", State: "probably-fine"}},
		}},
	}
	findings := ValidateSpecVerificationRecord(record)
	if !diag.HasErrors(findings) {
		t.Fatal("the strict reader accepted a state nobody declared")
	}
}

// TestGroupBlocksIsTheOneBlockingRule pins the sanction predicate every
// surface shares: only enforce can block, every non-verified state blocks it,
// and an error finding over the group's own inputs blocks it too — a gate
// whose inputs were unreadable must not read as green.
func TestGroupBlocksIsTheOneBlockingRule(t *testing.T) {
	verified := []SpecRequirementVerdict{{State: RequirementVerified}}
	for _, state := range []RequirementVerificationState{
		RequirementUnmapped, RequirementUnexecutable, RequirementMissing, RequirementStale, RequirementContradicted,
	} {
		unresolved := []SpecRequirementVerdict{{State: RequirementVerified}, {State: state}}
		if !GroupBlocks(VerificationModeEnforce, unresolved, nil) {
			t.Errorf("enforce did not block on %s", state)
		}
		if GroupBlocks(VerificationModeReport, unresolved, nil) || GroupBlocks(VerificationModeOff, unresolved, nil) {
			t.Errorf("%s blocked outside enforce", state)
		}
	}
	if GroupBlocks(VerificationModeEnforce, verified, nil) {
		t.Error("enforce blocked a fully verified group")
	}
	broken := []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidVerification, "x", "unreadable")}
	if !GroupBlocks(VerificationModeEnforce, verified, broken) {
		t.Error("enforce did not block on an error finding")
	}
	if GroupBlocks(VerificationModeEnforce, verified,
		[]diag.Diagnostic{diag.Warningf(ErrorCodeInvalidObservation, "x", "note")}) {
		t.Error("a warning finding blocked a verified group")
	}
}

// TestEmptySpecVerificationRecordRoundTripsReadable pins the fail-safe shape
// of a session that contained tests but no spec-owning projection: the
// persisted record has zero groups, and it must still round-trip through its
// own strict reader — canonicalization turning the empty groups array into
// JSON null once made core write records its own consumers refused.
func TestEmptySpecVerificationRecordRoundTripsReadable(t *testing.T) {
	record := &SpecVerificationRecord{
		ProtocolVersion: SpecVerificationRecordProtocolVersion,
		SessionID:       "20260818-100000-abcdef",
		GeneratedAt:     evaluationInstant,
		Groups:          []SpecVerificationGroup{},
	}
	data, err := MarshalSpecVerificationRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"groups": null`)) {
		t.Fatalf("empty groups marshaled as null:\n%s", data)
	}
	parsed, findings := ParseSpecVerificationRecord(data)
	if parsed == nil || len(findings) > 0 {
		t.Fatalf("record failed its own reader: %v", findings)
	}
	if diagnostics := ValidateSpecVerificationRecord(parsed); diag.HasErrors(diagnostics) {
		t.Fatalf("record failed its own validator: %v", diagnostics)
	}
}
