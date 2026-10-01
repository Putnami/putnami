package features

import (
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func ratchetBaseline(projects ...SpecsBaselineProject) *SpecsBaseline {
	return &SpecsBaseline{ProtocolVersion: WorkspaceSpecsBaselineProtocolVersion, Projects: projects}
}

func enforcedProject(id string, requirements ...string) SpecsBaselineProject {
	return SpecsBaselineProject{Project: id, Mode: VerificationModeEnforce, ExecutableRequirements: requirements}
}

// TestSpecsBaselineRoundTripsCanonically pins the committed file's byte
// contract: unsorted authored content re-emits sorted with the published
// schema URL, and the emitted bytes pass their own strict reader.
func TestSpecsBaselineRoundTripsCanonically(t *testing.T) {
	authored := ratchetBaseline(
		enforcedProject("/go/framework/parallel", "go/bounded-parallel-work#ordering"),
		enforcedProject("/go/framework/logger",
			"go/structured-logging#lifecycle", "go/structured-logging#failure-isolation"),
	)
	data, err := MarshalSpecsBaseline(authored)
	if err != nil {
		t.Fatal(err)
	}
	parsed, findings := ParseAndValidateSpecsBaseline(data)
	if parsed == nil || diag.HasErrors(findings) {
		t.Fatalf("canonical baseline fails its own reader: %v\n%s", findings, data)
	}
	if parsed.Schema != SpecsBaselineSchemaURL {
		t.Fatalf("schema = %q", parsed.Schema)
	}
	if parsed.Projects[0].Project != "/go/framework/logger" ||
		parsed.Projects[0].ExecutableRequirements[0] != "go/structured-logging#failure-isolation" {
		t.Fatalf("canonical order lost: %+v", parsed.Projects)
	}

	again, err := MarshalSpecsBaseline(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(data) {
		t.Fatalf("canonical form is not a fixed point:\n%s\n%s", data, again)
	}
}

// TestSpecsBaselineRoundTripsAnEmptyFloorEntry: an enforced project with no
// executable requirement yet is a legal floor entry, and its empty list must
// re-emit as [] — a nil slice would marshal as the explicit null the strict
// reader refuses, making the canonical writer brick its own consumer.
func TestSpecsBaselineRoundTripsAnEmptyFloorEntry(t *testing.T) {
	data, err := MarshalSpecsBaseline(ratchetBaseline(enforcedProject("/go/framework/logger")))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"executableRequirements": []`) {
		t.Fatalf("empty requirement list not encoded as []:\n%s", data)
	}
	parsed, findings := ParseAndValidateSpecsBaseline(data)
	if parsed == nil || diag.HasErrors(findings) {
		t.Fatalf("empty floor entry fails its own reader: %v\n%s", findings, data)
	}
}

// TestSpecsBaselineValidationIsClosed pins the strict shape: only enforce
// entries, canonical feature#requirement identities, no duplicates, no null
// collections, the exact version token.
func TestSpecsBaselineValidationIsClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*SpecsBaseline)
		want   string
	}{
		"report mode": {func(b *SpecsBaseline) { b.Projects[0].Mode = VerificationModeReport },
			"baseline mode must be"},
		"broken identity": {func(b *SpecsBaseline) { b.Projects[0].ExecutableRequirements[0] = "no-feature-part" },
			"not a canonical feature#requirement pair"},
		"duplicate requirement": {func(b *SpecsBaseline) {
			b.Projects[0].ExecutableRequirements = append(b.Projects[0].ExecutableRequirements,
				b.Projects[0].ExecutableRequirements[0])
		}, "is duplicated"},
		"duplicate project": {func(b *SpecsBaseline) { b.Projects = append(b.Projects, b.Projects[0]) },
			"is duplicated"},
		"nil requirements": {func(b *SpecsBaseline) { b.Projects[0].ExecutableRequirements = nil },
			"must be an array"},
		"empty project": {func(b *SpecsBaseline) { b.Projects[0].Project = " " },
			"project is required"},
	} {
		t.Run(name, func(t *testing.T) {
			baseline := ratchetBaseline(enforcedProject("/go/framework/logger", "go/structured-logging#lifecycle"))
			tc.mutate(baseline)
			findings := ValidateSpecsBaseline(baseline)
			if !diag.HasErrors(findings) {
				t.Fatalf("mutation accepted: %+v", baseline)
			}
			if !strings.Contains(diagText(findings), tc.want) {
				t.Fatalf("findings = %v, want %q", findings, tc.want)
			}
		})
	}

	if _, findings := ParseSpecsBaseline([]byte(`{"protocolVersion":2,"projects":[]}`)); !hasCode(findings, ErrorCodeInvalidProtocolVersion) {
		t.Fatalf("forward version accepted: %v", findings)
	}
	if _, findings := ParseSpecsBaseline([]byte(`{"protocolVersion":1,"projects":null}`)); len(findings) == 0 {
		t.Fatal("explicit null accepted")
	}
	if _, findings := ParseSpecsBaseline([]byte(`{"protocolVersion":1,"projects":[],"surprise":true}`)); !hasCode(findings, ErrorCodeUnknownField) {
		t.Fatalf("unknown field accepted: %v", findings)
	}
}

// TestProjectSpecsBaselineRoundTripsCanonically pins the project file's byte
// contract: it names no project, its requirements re-emit sorted with the
// schema URL stamped, an empty list stays [], and the bytes pass their own
// strict reader as a fixed point.
func TestProjectSpecsBaselineRoundTripsCanonically(t *testing.T) {
	data, err := MarshalProjectSpecsBaseline(&ProjectSpecsBaseline{
		ProtocolVersion:        SpecsBaselineProtocolVersion,
		ExecutableRequirements: []string{"go/structured-logging#lifecycle", "go/structured-logging#failure-isolation"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "$schema": "https://putnami.dev/schemas/putnami-specs-baseline.json",
  "protocolVersion": 2,
  "executableRequirements": [
    "go/structured-logging#failure-isolation",
    "go/structured-logging#lifecycle"
  ]
}
`
	if string(data) != want {
		t.Fatalf("canonical project baseline =\n%s\nwant\n%s", data, want)
	}
	if !IsProjectSpecsBaseline(data) {
		t.Fatal("a project baseline is not recognized as one")
	}
	parsed, findings := ParseAndValidateProjectSpecsBaseline(data)
	if parsed == nil || diag.HasErrors(findings) {
		t.Fatalf("canonical project baseline fails its own reader: %v", findings)
	}
	again, err := MarshalProjectSpecsBaseline(parsed)
	if err != nil || string(again) != string(data) {
		t.Fatalf("canonical form is not a fixed point (%v):\n%s", err, again)
	}

	empty, err := MarshalProjectSpecsBaseline(&ProjectSpecsBaseline{ProtocolVersion: SpecsBaselineProtocolVersion})
	if err != nil || !strings.Contains(string(empty), `"executableRequirements": []`) {
		t.Fatalf("empty requirement list not encoded as [] (%v):\n%s", err, empty)
	}
}

// TestProjectSpecsBaselineValidationIsClosed: the project file accepts only
// its own version and shape — a workspace floor document, a project member,
// a broken identity or an explicit null are refused.
func TestProjectSpecsBaselineValidationIsClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		document string
		code     string
	}{
		"workspace version": {`{"protocolVersion":1,"projects":[]}`, ErrorCodeInvalidProtocolVersion},
		"project member":    {`{"protocolVersion":2,"project":"/app","executableRequirements":[]}`, ErrorCodeUnknownField},
		"broken identity":   {`{"protocolVersion":2,"executableRequirements":["no-feature-part"]}`, ErrorCodeInvalidRequirement},
		"null requirements": {`{"protocolVersion":2,"executableRequirements":null}`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			parsed, findings := ParseAndValidateProjectSpecsBaseline([]byte(tc.document))
			if parsed != nil || !diag.HasErrors(findings) {
				t.Fatalf("document accepted: %s", tc.document)
			}
			if tc.code != "" && !hasCode(findings, tc.code) {
				t.Fatalf("findings = %v, want %s", findings, tc.code)
			}
		})
	}
	if IsProjectSpecsBaseline([]byte(`{"protocolVersion":1,"projects":[]}`)) {
		t.Fatal("a workspace floor document is recognized as a project baseline")
	}
}

// TestCompareSpecsBaselineIsShrinkOnly pins the ratchet rule: regressing out
// of enforce or losing a recorded requirement is an error naming the reviewed
// policy change; growth in either direction is legal and only counted.
func TestCompareSpecsBaselineIsShrinkOnly(t *testing.T) {
	baseline := ratchetBaseline(
		enforcedProject("/go/framework/logger", "go/structured-logging#lifecycle", "go/structured-logging#record"),
		enforcedProject("/go/framework/parallel", "go/bounded-parallel-work#ordering"),
	)

	// Identical floors: silence.
	findings, summary := CompareSpecsBaseline(baseline, baseline)
	if len(findings) != 0 || summary.RegressedProjects != 0 || summary.LostRequirements != 0 {
		t.Fatalf("identical floors drew findings %v summary %+v", findings, summary)
	}

	// Growth: a new enforced project and a new requirement — no findings,
	// counted for the nudge.
	grown := ratchetBaseline(
		enforcedProject("/go/framework/logger",
			"go/structured-logging#lifecycle", "go/structured-logging#record", "go/structured-logging#field-precedence"),
		enforcedProject("/go/framework/parallel", "go/bounded-parallel-work#ordering"),
		enforcedProject("/typescript/framework/runtime", "typescript/dependency-injection#singleton-identity"),
	)
	findings, summary = CompareSpecsBaseline(baseline, grown)
	if len(findings) != 0 {
		t.Fatalf("growth drew findings: %v", findings)
	}
	if summary.GrownProjects != 1 || summary.GrownRequirements != 1 {
		t.Fatalf("summary = %+v, want one grown project and one grown requirement", summary)
	}

	// Shrink: parallel regressed out of enforce, logger lost a requirement.
	shrunk := ratchetBaseline(
		enforcedProject("/go/framework/logger", "go/structured-logging#lifecycle"),
	)
	findings, summary = CompareSpecsBaseline(baseline, shrunk)
	if summary.RegressedProjects != 1 || summary.LostRequirements != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	text := diagText(findings)
	if !strings.Contains(text, "regressed out of enforce") ||
		!strings.Contains(text, "lost executable requirement coverage (go/structured-logging#record)") ||
		!strings.Contains(text, SpecsBaselineFilename) {
		t.Fatalf("findings = %v", findings)
	}
	for _, finding := range findings {
		if finding.Severity != diag.Error || finding.Code != ErrorCodeRatchetRegression {
			t.Fatalf("finding = %+v", finding)
		}
	}

	// An absent baseline is initial adoption: nothing to protect, only growth.
	findings, summary = CompareSpecsBaseline(nil, grown)
	if len(findings) != 0 || summary.BaselineProjects != 0 || summary.GrownProjects != 3 {
		t.Fatalf("initial adoption drew %v %+v", findings, summary)
	}
}

func diagText(findings []diag.Diagnostic) string {
	parts := make([]string, 0, len(findings))
	for _, finding := range findings {
		parts = append(parts, finding.Message)
	}
	return strings.Join(parts, "\n")
}
