package features

import (
	"bytes"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

func validProjectionDocument() string {
	return `{
  "$schema": "https://putnami.dev/schemas/putnami-spec-criteria.json",
  "protocolVersion": 1,
  "groups": [
    {
      "feature": "go/structured-logging",
      "spec": "go/framework/logger/specs/structured-logging.json",
      "specRequirements": ["lifecycle", "record"],
      "requirements": [
        {
          "id": "lifecycle",
          "stage": "coded",
          "evidenceKinds": ["attestation"],
          "verification": {
            "kind": "acceptance",
            "checks": ["close-visits-every-sink", "flush-visits-every-sink"]
          }
        },
        {
          "id": "record",
          "stage": "coded",
          "evidenceKinds": ["attestation"]
        }
      ]
    }
  ]
}`
}

func TestParseAndValidateSpecCriteriaProjectionAcceptsTheCanonicalForm(t *testing.T) {
	projection, diagnostics := ParseAndValidateSpecCriteriaProjection([]byte(validProjectionDocument()))
	if projection == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("valid projection rejected: %v", diagnostics)
	}
	if len(projection.Groups) != 1 || projection.Groups[0].Feature != "go/structured-logging" {
		t.Fatalf("unexpected groups: %+v", projection.Groups)
	}
	if projection.Groups[0].Requirements[0].Verification == nil {
		t.Fatal("the executable requirement lost its criterion")
	}
	if projection.Groups[0].Requirements[1].Verification != nil {
		t.Fatal("the unexecutable requirement grew a criterion")
	}
}

func TestParseSpecCriteriaProjectionRequiresTheExactVersionToken(t *testing.T) {
	spectest.Proves(t, "features/feature-contracts", "strict-versioned-documents", "the-projection-reader-refuses-an-inexact-version-an-unknown-field-or-a-null")
	for _, token := range []string{"0", "2", "1.0", "1e0"} {
		document := strings.Replace(validProjectionDocument(), `"protocolVersion": 1`, `"protocolVersion": `+token, 1)
		if projection, _ := ParseSpecCriteriaProjection([]byte(document)); projection != nil {
			t.Errorf("projection with protocolVersion %s was accepted", token)
		}
	}
}

func TestParseSpecCriteriaProjectionRejectsUnknownFieldsAndNulls(t *testing.T) {
	spectest.Proves(t, "features/feature-contracts", "strict-versioned-documents", "the-projection-reader-refuses-an-inexact-version-an-unknown-field-or-a-null")
	unknown := strings.Replace(validProjectionDocument(), `"groups": [`, `"verdict": "verified", "groups": [`, 1)
	if projection, _ := ParseSpecCriteriaProjection([]byte(unknown)); projection != nil {
		t.Error("a projection carrying an undeclared field was accepted")
	}
	null := strings.Replace(validProjectionDocument(), `"specRequirements": ["lifecycle", "record"]`, `"specRequirements": null`, 1)
	if projection, _ := ParseSpecCriteriaProjection([]byte(null)); projection != nil {
		t.Error("a projection carrying an explicit null was accepted")
	}
}

func TestValidateSpecCriteriaProjectionRejectsBrokenGroups(t *testing.T) {
	target := 25.0
	cases := map[string]SpecCriteriaProjection{
		"missing groups": {ProtocolVersion: 1},
		"duplicate feature": {ProtocolVersion: 1, Groups: []SpecCriteriaGroup{
			{Feature: "go/x", Spec: "a/specs/x.json", SpecRequirements: []string{"r"}},
			{Feature: "go/x", Spec: "b/specs/x.json", SpecRequirements: []string{"r"}},
		}},
		"invalid feature id": {ProtocolVersion: 1, Groups: []SpecCriteriaGroup{
			{Feature: "NotAFeature", Spec: "a/specs/x.json", SpecRequirements: []string{"r"}},
		}},
		"escaping spec path": {ProtocolVersion: 1, Groups: []SpecCriteriaGroup{
			{Feature: "go/x", Spec: "../specs/x.json", SpecRequirements: []string{"r"}},
		}},
		"non-spec path": {ProtocolVersion: 1, Groups: []SpecCriteriaGroup{
			{Feature: "go/x", Spec: "a/putnami.features.json", SpecRequirements: []string{"r"}},
		}},
		"empty textual identities": {ProtocolVersion: 1, Groups: []SpecCriteriaGroup{
			{Feature: "go/x", Spec: "a/specs/x.json", SpecRequirements: []string{}},
		}},
		"duplicated textual identity": {ProtocolVersion: 1, Groups: []SpecCriteriaGroup{
			{Feature: "go/x", Spec: "a/specs/x.json", SpecRequirements: []string{"r", "r"}},
		}},
		"requirement outside the join": {ProtocolVersion: 1, Groups: []SpecCriteriaGroup{
			{Feature: "go/x", Spec: "a/specs/x.json", SpecRequirements: []string{"r"},
				Requirements: []Requirement{{ID: "other", Stage: MaturityCoded, EvidenceKinds: []EvidenceKind{EvidenceKindAttestation}}}},
		}},
		"criterion refusing attestation": {ProtocolVersion: 1, Groups: []SpecCriteriaGroup{
			{Feature: "go/x", Spec: "a/specs/x.json", SpecRequirements: []string{"r"},
				Requirements: []Requirement{{ID: "r", Stage: MaturityCoded, EvidenceKinds: []EvidenceKind{EvidenceKindArtifact},
					Verification: &VerificationCriterion{Kind: VerificationKindAcceptance, Checks: []string{"c"}}}}},
		}},
		"threshold with two checks": {ProtocolVersion: 1, Groups: []SpecCriteriaGroup{
			{Feature: "go/x", Spec: "a/specs/x.json", SpecRequirements: []string{"r"},
				Requirements: []Requirement{{ID: "r", Stage: MaturityCoded, EvidenceKinds: []EvidenceKind{EvidenceKindAttestation},
					Verification: &VerificationCriterion{Kind: VerificationKindThreshold, Checks: []string{"a", "b"},
						Metric: "m.x", Aggregation: AggregationP95, Operator: OperatorLte, Target: &target, Unit: "ms",
						Window: &VerificationWindow{Kind: WindowKindInvocation}}}}}},
		},
	}
	for name, projection := range cases {
		if diagnostics := ValidateSpecCriteriaProjection(&projection); !diag.HasErrors(diagnostics) {
			t.Errorf("%s: projection was accepted", name)
		}
	}
}

func TestMarshalSpecCriteriaProjectionIsCanonicalAndByteStable(t *testing.T) {
	spectest.Proves(t, "features/feature-contracts", "canonical-bytes-across-languages", "go-writes-the-criteria-projection-canonically")
	unsorted := &SpecCriteriaProjection{
		Schema:          "https://putnami.dev/schemas/putnami-spec-criteria.json",
		ProtocolVersion: 1,
		Groups: []SpecCriteriaGroup{
			{
				Feature:          "go/structured-logging",
				Spec:             "go/framework/logger/specs/structured-logging.json",
				SpecRequirements: []string{"record", "lifecycle"},
				Requirements: []Requirement{
					{ID: "record", Stage: MaturityCoded, EvidenceKinds: []EvidenceKind{EvidenceKindAttestation}},
					{ID: "lifecycle", Stage: MaturityCoded, EvidenceKinds: []EvidenceKind{EvidenceKindAttestation},
						Verification: &VerificationCriterion{Kind: VerificationKindAcceptance,
							Checks: []string{"flush-visits-every-sink", "close-visits-every-sink"}}},
				},
			},
			{Feature: "go/bounded-parallel-work", Spec: "go/framework/parallel/specs/bounded-parallel-work.json",
				SpecRequirements: []string{"ordering"}},
		},
	}
	first, err := MarshalSpecCriteriaProjection(unsorted)
	if err != nil {
		t.Fatalf("marshal projection: %v", err)
	}
	if unsorted.Groups[0].Feature != "go/structured-logging" {
		t.Fatal("canonicalization mutated the caller's projection")
	}
	parsed, diagnostics := ParseAndValidateSpecCriteriaProjection(first)
	if parsed == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("canonical bytes failed the strict reader: %v", diagnostics)
	}
	if parsed.Groups[0].Feature != "go/bounded-parallel-work" {
		t.Fatalf("groups are not sorted by feature: %+v", parsed.Groups)
	}
	if got := parsed.Groups[1].SpecRequirements; got[0] != "lifecycle" || got[1] != "record" {
		t.Fatalf("textual identities are not sorted: %v", got)
	}
	if checks := parsed.Groups[1].Requirements[0].Verification.Checks; checks[0] != "close-visits-every-sink" {
		t.Fatalf("criterion checks are not sorted: %v", checks)
	}
	second, err := MarshalSpecCriteriaProjection(parsed)
	if err != nil {
		t.Fatalf("re-marshal projection: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("marshal is not byte-stable:\n%s\n---\n%s", first, second)
	}
}

// projectionGolden is the canonical form of validProjectionDocument. These are
// the bytes of the first projection version, which a group without a target
// keeps.
const projectionGolden = `{
  "$schema": "https://putnami.dev/schemas/putnami-spec-criteria.json",
  "protocolVersion": 1,
  "groups": [
    {
      "feature": "go/structured-logging",
      "spec": "go/framework/logger/specs/structured-logging.json",
      "specRequirements": [
        "lifecycle",
        "record"
      ],
      "requirements": [
        {
          "id": "lifecycle",
          "stage": "coded",
          "evidenceKinds": [
            "attestation"
          ],
          "verification": {
            "kind": "acceptance",
            "checks": [
              "close-visits-every-sink",
              "flush-visits-every-sink"
            ]
          }
        },
        {
          "id": "record",
          "stage": "coded",
          "evidenceKinds": [
            "attestation"
          ]
        }
      ]
    }
  ]
}
`

// TestSpecCriteriaGroupCarriesTheFeatureTarget pins the additive member: it
// round-trips, written after the spec path; a group without it keeps the exact
// bytes of the first projection version and still reads; and a stage outside
// the maturity ladder is refused.
func TestSpecCriteriaGroupCarriesTheFeatureTarget(t *testing.T) {
	spectest.Proves(t, "features/feature-contracts", "groups-name-the-feature-target", "a-criteria-group-round-trips-its-feature-target")
	spectest.Proves(t, "features/feature-contracts", "groups-name-the-feature-target", "a-group-without-a-target-keeps-its-bytes-and-reads")
	spectest.Proves(t, "features/feature-contracts", "groups-name-the-feature-target", "a-target-outside-the-maturity-ladder-is-refused")

	native, diagnostics := ParseAndValidateSpecCriteriaProjection([]byte(validProjectionDocument()))
	if native == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("a projection without a target did not read: %v", diagnostics)
	}
	if native.Groups[0].Target != "" {
		t.Fatalf("a group without the member read target %q", native.Groups[0].Target)
	}
	encoded, err := MarshalSpecCriteriaProjection(native)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != projectionGolden {
		t.Fatalf("a group without a target changed its bytes:\n%s", encoded)
	}

	native.Groups[0].Target = MaturityWired
	targeted, err := MarshalSpecCriteriaProjection(native)
	if err != nil {
		t.Fatal(err)
	}
	specLine := `      "spec": "go/framework/logger/specs/structured-logging.json",` + "\n"
	want := strings.Replace(projectionGolden, specLine, specLine+`      "target": "wired",`+"\n", 1)
	if string(targeted) != want {
		t.Fatalf("the target is not written right after the spec path:\n%s", targeted)
	}
	parsed, diagnostics := ParseAndValidateSpecCriteriaProjection(targeted)
	if parsed == nil || diag.HasErrors(diagnostics) || parsed.Groups[0].Target != MaturityWired {
		t.Fatalf("the target did not round-trip: %v", diagnostics)
	}

	for _, stage := range []string{"shipped", "Coded", "coded "} {
		broken := strings.Replace(want, `"target": "wired"`, `"target": "`+stage+`"`, 1)
		if _, diagnostics := ParseAndValidateSpecCriteriaProjection([]byte(broken)); !hasCode(diagnostics, ErrorCodeInvalidStage) {
			t.Errorf("target %q was accepted: %v", stage, diagnostics)
		}
	}
}
