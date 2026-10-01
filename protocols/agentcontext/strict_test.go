package agentcontext

import (
	"encoding/json"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// findCode returns true if diags contains a diagnostic with the given code.
func findCode(diags []diag.Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

// validDocument returns a minimal document that validates clean, so a test can
// mutate one field to isolate a single diagnostic.
func validDocument() *Document {
	return &Document{
		ProtocolVersion: ProtocolVersion,
		Identity: Identity{
			ID:   "/go/samples/library",
			Name: "go.putnami.dev/examples/library",
			Path: "go/samples/library",
		},
		Provenance: Provenance{
			WorkspaceRevision: "deadbeef",
			Generator:         Generator{Name: "putnami", Version: "0.1.0"},
			AggregationMethod: AggregationMethodByReference,
		},
	}
}

func TestParseDocument_UnknownField(t *testing.T) {
	d, diags := ParseDocument([]byte(`{"protocolVersion":1,"bogus":true}`))
	if d != nil {
		t.Error("document should be nil on parse error")
	}
	if !findCode(diags, ErrorCodeUnknownField) {
		t.Errorf("want %s, got %v", ErrorCodeUnknownField, diags)
	}
}

func TestParseDocument_InvalidJSON(t *testing.T) {
	_, diags := ParseDocument([]byte("{not json"))
	if !diag.HasErrors(diags) {
		t.Fatal("expected parse error")
	}
	if diags[0].Code != ErrorCodeParseError {
		t.Errorf("Code = %q, want %s", diags[0].Code, ErrorCodeParseError)
	}
}

func TestParseOverrides_UnknownField(t *testing.T) {
	o, diags := ParseOverrides([]byte(`{"protocolVersion":1,"bogus":true}`))
	if o != nil {
		t.Error("overrides file should be nil on parse error")
	}
	if !findCode(diags, ErrorCodeUnknownField) {
		t.Errorf("want %s, got %v", ErrorCodeUnknownField, diags)
	}
}

func TestValidateDocument_NilIsParseError(t *testing.T) {
	if !findCode(ValidateDocument(nil), ErrorCodeParseError) {
		t.Errorf("want %s for nil document", ErrorCodeParseError)
	}
}

func TestValidateDocument_ProtocolVersion(t *testing.T) {
	d := validDocument()
	d.ProtocolVersion = 99
	if !findCode(ValidateDocument(d), ErrorCodeInvalidProtocolVersion) {
		t.Errorf("want %s", ErrorCodeInvalidProtocolVersion)
	}
}

func TestValidateDocument_Identity(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Document)
		code   string
	}{
		{"missingID", func(d *Document) { d.Identity.ID = " " }, ErrorCodeInvalidIdentity},
		{"missingName", func(d *Document) { d.Identity.Name = "" }, ErrorCodeInvalidIdentity},
		{"missingPath", func(d *Document) { d.Identity.Path = "" }, ErrorCodeInvalidIdentity},
		{"duplicateDep", func(d *Document) { d.Identity.Dependencies = []string{"/a", "/a"} }, ErrorCodeDuplicateID},
		{"duplicateDependent", func(d *Document) { d.Identity.Dependents = []string{"/b", "/b"} }, ErrorCodeDuplicateID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validDocument()
			tc.mutate(d)
			if !findCode(ValidateDocument(d), tc.code) {
				t.Errorf("want %s, got %v", tc.code, ValidateDocument(d))
			}
		})
	}
}

func TestValidateDocument_SectionFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Document)
		code   string
	}{
		{"invalidRootKind", func(d *Document) {
			d.CompositionRoots = []CompositionRoot{{Kind: "bogus", Path: "main.go"}}
		}, ErrorCodeInvalidRootKind},
		{"invalidDigest", func(d *Document) {
			d.Capabilities = []ArtifactRef{{Path: "schema/capabilities.json", Digest: "sha256:nothex"}}
		}, ErrorCodeInvalidDigest},
		{"duplicateRef", func(d *Document) {
			d.Contracts = []ArtifactRef{
				{Path: "schema/contracts.json", Digest: goodDigest},
				{Path: "schema/contracts.json", Digest: goodDigest},
			}
		}, ErrorCodeDuplicateRef},
		{"invalidPathAbsolute", func(d *Document) {
			d.Infra = []ArtifactRef{{Path: "/etc/passwd", Digest: goodDigest}}
		}, ErrorCodeInvalidPath},
		{"invalidPathEscape", func(d *Document) {
			d.Migrations = []ArtifactRef{{Path: "../outside/bundle.json", Digest: goodDigest}}
		}, ErrorCodeInvalidPath},
		{"invalidRange", func(d *Document) {
			d.RepresentativeSources = []SourceRange{{Path: "a.go", StartLine: 10, EndLine: 2, Why: SourceReasonMain, Tokens: TokenEstimate{Estimated: 1, Method: TokenMethodBytesDiv4}}}
		}, ErrorCodeInvalidRange},
		{"invalidSourceReason", func(d *Document) {
			d.RepresentativeSources = []SourceRange{{Path: "a.go", StartLine: 1, EndLine: 2, Why: "bogus", Tokens: TokenEstimate{Estimated: 1, Method: TokenMethodBytesDiv4}}}
		}, ErrorCodeInvalidSourceReason},
		{"negativeTokens", func(d *Document) {
			d.RepresentativeSources = []SourceRange{{Path: "a.go", StartLine: 1, EndLine: 2, Why: SourceReasonMain, Tokens: TokenEstimate{Estimated: -1, Method: TokenMethodBytesDiv4}}}
		}, ErrorCodeInvalidTokenEstimate},
		{"invalidTokenMethod", func(d *Document) {
			d.RepresentativeSources = []SourceRange{{Path: "a.go", StartLine: 1, EndLine: 2, Why: SourceReasonMain, Tokens: TokenEstimate{Estimated: 1, Method: "chars"}}}
		}, ErrorCodeInvalidTokenMethod},
		{"invalidDocRelationship", func(d *Document) {
			d.Docs = []DocRef{{Path: "README.md", Relationship: "maybe"}}
		}, ErrorCodeInvalidDocRelationship},
		{"invalidAggregationMethod", func(d *Document) {
			d.Provenance.AggregationMethod = "inline"
		}, ErrorCodeInvalidAggregationMethod},
		{"missingProvenanceRevision", func(d *Document) {
			d.Provenance.WorkspaceRevision = ""
		}, ErrorCodeMissingProvenance},
		{"missingGeneratorName", func(d *Document) {
			d.Provenance.Generator.Name = ""
		}, ErrorCodeMissingProvenance},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validDocument()
			tc.mutate(d)
			if !findCode(ValidateDocument(d), tc.code) {
				t.Errorf("want %s, got %v", tc.code, ValidateDocument(d))
			}
		})
	}
}

const goodDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func TestValidateDocument_TestsSection(t *testing.T) {
	cases := []struct {
		name  string
		tests *TestsSection
		code  string
	}{
		{"invalidPolicy", &TestsSection{Policy: "always", AbsenceReason: AbsenceReasonNoPacks}, ErrorCodeInvalidTestPolicy},
		{"emptyNoReason", &TestsSection{Policy: TestPolicySkip}, ErrorCodeMissingAbsenceReason},
		{"emptyBadReason", &TestsSection{Policy: TestPolicySkip, AbsenceReason: "dunno"}, ErrorCodeInvalidAbsenceReason},
		{"packsWithReason", &TestsSection{Policy: TestPolicyRequire, Packs: []PackRef{{ID: "p"}}, AbsenceReason: AbsenceReasonNoPacks}, ErrorCodeInvalidAbsenceReason},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validDocument()
			d.Tests = tc.tests
			if !findCode(ValidateDocument(d), tc.code) {
				t.Errorf("want %s, got %v", tc.code, ValidateDocument(d))
			}
		})
	}
}

func TestValidateDocument_TestsSectionAbsentIsValid(t *testing.T) {
	d := validDocument()
	d.Tests = nil
	if diag.HasErrors(ValidateDocument(d)) {
		t.Errorf("an absent tests section must validate clean: %v", ValidateDocument(d))
	}
}

func TestValidateDocument_TestsSectionEmptyWithReasonIsValid(t *testing.T) {
	d := validDocument()
	d.Tests = &TestsSection{Policy: TestPolicySkip, AbsenceReason: AbsenceReasonNotCollected}
	if diag.HasErrors(ValidateDocument(d)) {
		t.Errorf("an empty tests section with an absence reason must validate clean: %v", ValidateDocument(d))
	}
}

func TestValidateDocument_SampleIsClean(t *testing.T) {
	if diags := ValidateDocument(sampleDocument()); diag.HasErrors(diags) {
		t.Errorf("sampleDocument should validate clean, got %v", diags)
	}
}

// --- Overrides validation ---

func TestValidateOverrides_NilIsParseError(t *testing.T) {
	if !findCode(ValidateOverrides(nil), ErrorCodeParseError) {
		t.Errorf("want %s for nil overrides", ErrorCodeParseError)
	}
}

func TestValidateOverrides_Fields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*OverridesFile)
		code   string
	}{
		{"badVersion", func(o *OverridesFile) { o.ProtocolVersion = 2 }, ErrorCodeInvalidProtocolVersion},
		{"badAddSource", func(o *OverridesFile) {
			o.AddSources = []SourceRange{{Path: "a.go", StartLine: 0, EndLine: 0, Why: SourceReasonOverride, Tokens: TokenEstimate{Method: TokenMethodBytesDiv4}}}
		}, ErrorCodeInvalidRange},
		{"badRemoveSource", func(o *OverridesFile) { o.RemoveSources = []string{"/abs.go"} }, ErrorCodeInvalidPath},
		{"badAddDoc", func(o *OverridesFile) { o.AddDocs = []DocRef{{Path: "d.md", Relationship: "nope"}} }, ErrorCodeInvalidDocRelationship},
		{"badRemoveDoc", func(o *OverridesFile) { o.RemoveDocs = []string{"../escape.md"} }, ErrorCodeInvalidPath},
		{"badSensitive", func(o *OverridesFile) { o.Sensitive = []string{"/secret"} }, ErrorCodeInvalidPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := &OverridesFile{ProtocolVersion: ProtocolVersion}
			tc.mutate(o)
			if !findCode(ValidateOverrides(o), tc.code) {
				t.Errorf("want %s, got %v", tc.code, ValidateOverrides(o))
			}
		})
	}
}

func TestValidateOverrides_SampleIsClean(t *testing.T) {
	if diags := ValidateOverrides(sampleOverrides()); diag.HasErrors(diags) {
		t.Errorf("sampleOverrides should validate clean, got %v", diags)
	}
}

// --- Publish-safety gate ---

func TestPublishSafety_NilIsError(t *testing.T) {
	if !findCode(ValidatePublishSafety(nil, PublishSafetyOptions{}), ErrorCodeParseError) {
		t.Error("nil document must fail the publish-safety gate, never pass vacuously")
	}
}

func TestPublishSafety_SampleWithFlaggedSensitivePasses(t *testing.T) {
	// The sample's infra ref is flagged sensitive; declaring its path sensitive
	// must pass because the flag is set.
	opts := PublishSafetyOptions{SensitivePaths: map[string]bool{
		"go/samples/task-api/infra/requirements.json": true,
	}}
	if diags := ValidatePublishSafety(sampleDocument(), opts); diag.HasErrors(diags) {
		t.Errorf("sampleDocument with a flagged sensitive ref should pass the gate, got %v", diags)
	}
}

func TestPublishSafety_UnflaggedSensitiveFailsClosed(t *testing.T) {
	d := validDocument()
	d.Capabilities = []ArtifactRef{{Path: "secret/keyring.json", Digest: goodDigest}} // not flagged
	opts := PublishSafetyOptions{SensitivePaths: map[string]bool{"secret/keyring.json": true}}
	if !findCode(ValidatePublishSafety(d, opts), ErrorCodeUnredactedSensitive) {
		t.Errorf("a reference to a sensitive path without the flag must fail with %s", ErrorCodeUnredactedSensitive)
	}
}

func TestPublishSafety_FlaggedSensitivePasses(t *testing.T) {
	d := validDocument()
	d.Capabilities = []ArtifactRef{{Path: "secret/keyring.json", Digest: goodDigest, Sensitive: true}}
	opts := PublishSafetyOptions{SensitivePaths: map[string]bool{"secret/keyring.json": true}}
	if findCode(ValidatePublishSafety(d, opts), ErrorCodeUnredactedSensitive) {
		t.Errorf("a flagged sensitive reference must pass: %v", ValidatePublishSafety(d, opts))
	}
}

func TestPublishSafety_SensitiveDocAndSourceEnforced(t *testing.T) {
	d := validDocument()
	d.Docs = []DocRef{{Path: "docs/internal.md", Relationship: DocRelationshipUnchecked}}
	d.RepresentativeSources = []SourceRange{{Path: "internal/keys.go", StartLine: 1, EndLine: 5, Why: SourceReasonMain, Tokens: TokenEstimate{Estimated: 10, Method: TokenMethodBytesDiv4}}}
	opts := PublishSafetyOptions{SensitivePaths: map[string]bool{
		"docs/internal.md": true,
		"internal/keys.go": true,
	}}
	diags := ValidatePublishSafety(d, opts)
	if got := len(diag.Errors(diags)); got != 2 {
		t.Errorf("expected both the sensitive doc and source to fail closed, got %d errors: %v", got, diags)
	}
}

func TestPublishSafety_EmbeddedContentRejected(t *testing.T) {
	d := validDocument()
	// Smuggle a large blob into a free-form provenance string.
	d.CompositionRoots = []CompositionRoot{{Kind: RootKindApplicationMain, Path: "main.go", Provenance: strings.Repeat("x", defaultMaxStringBytes+1)}}
	if !findCode(ValidatePublishSafety(d, PublishSafetyOptions{}), ErrorCodeEmbeddedContent) {
		t.Errorf("an oversized string must fail with %s", ErrorCodeEmbeddedContent)
	}
}

func TestPublishSafety_CustomThreshold(t *testing.T) {
	d := validDocument()
	d.Identity.Name = strings.Repeat("y", 50)
	if diags := ValidatePublishSafety(d, PublishSafetyOptions{MaxStringBytes: 10}); !findCode(diags, ErrorCodeEmbeddedContent) {
		t.Errorf("a custom threshold must be honored, got %v", diags)
	}
}

func TestPublishSafety_PathHygieneDefenseInDepth(t *testing.T) {
	d := validDocument()
	d.Contracts = []ArtifactRef{{Path: "../escape.json", Digest: goodDigest}}
	if !findCode(ValidatePublishSafety(d, PublishSafetyOptions{}), ErrorCodeInvalidPath) {
		t.Errorf("the gate must independently reject a non-workspace-relative path with %s", ErrorCodeInvalidPath)
	}
}

func TestPublishSafety_DuplicateRefDefenseInDepth(t *testing.T) {
	d := validDocument()
	d.Contracts = []ArtifactRef{
		{Path: "schema/contracts.json", Digest: goodDigest},
		{Path: "schema/contracts.json", Digest: goodDigest},
	}
	if !findCode(ValidatePublishSafety(d, PublishSafetyOptions{}), ErrorCodeDuplicateRef) {
		t.Errorf("the gate must independently reject a duplicate ref with %s", ErrorCodeDuplicateRef)
	}
}

func TestPublishSafety_CleanDocumentPasses(t *testing.T) {
	d := validDocument()
	if diags := ValidatePublishSafety(d, PublishSafetyOptions{}); diag.HasErrors(diags) {
		t.Errorf("a clean minimal document must pass the gate, got %v", diags)
	}
}

// --- Path hygiene ---

func TestIsWorkspaceRelative(t *testing.T) {
	ok := []string{"a.go", "go/samples/x/main.go", "schema/config.json", "a/b..c/d.go"}
	for _, p := range ok {
		if !isWorkspaceRelative(p) {
			t.Errorf("isWorkspaceRelative(%q) = false, want true", p)
		}
	}
	bad := []string{"", "  ", "/abs/path", "../escape", "a/../b", "a/..", "..", `windows\path`, "C:\\x"}
	for _, p := range bad {
		if isWorkspaceRelative(p) {
			t.Errorf("isWorkspaceRelative(%q) = true, want false", p)
		}
	}
}

// --- Review-hardening regressions ---

// TestValidateDocument_IdentityPathEscapeRejected pins that identity.path gets
// full path hygiene: consumers use it as the project directory, so an escaping
// value must fail both the normal validation and the publish-safety gate.
func TestValidateDocument_IdentityPathEscapeRejected(t *testing.T) {
	d := validDocument()
	d.Identity.Path = "../outside"
	if !findCode(ValidateDocument(d), ErrorCodeInvalidPath) {
		t.Errorf("identity.path %q must fail ValidateDocument with %s", d.Identity.Path, ErrorCodeInvalidPath)
	}
	if !findCode(ValidatePublishSafety(d, PublishSafetyOptions{}), ErrorCodeInvalidPath) {
		t.Errorf("identity.path %q must fail ValidatePublishSafety with %s", d.Identity.Path, ErrorCodeInvalidPath)
	}
}

// TestValidatePath_NonCanonicalSpellingRejected pins that equivalent-but-
// different path spellings are hard errors, which is what makes the exact-match
// sensitive-path lookup in the publish-safety gate sound: "./secret/x" cannot
// slip past a sensitive set keyed "secret/x" because it never validates.
func TestValidatePath_NonCanonicalSpellingRejected(t *testing.T) {
	d := validDocument()
	d.Capabilities = []ArtifactRef{{Path: "./secret/keyring.json", Digest: goodDigest}} // not flagged
	opts := PublishSafetyOptions{SensitivePaths: map[string]bool{"secret/keyring.json": true}}
	if !findCode(ValidatePublishSafety(d, opts), ErrorCodeInvalidPath) {
		t.Errorf("a non-canonical spelling of a sensitive path must fail the gate with %s, not bypass the lookup", ErrorCodeInvalidPath)
	}
	for _, p := range []string{"./x", "a//b", "a/./b", "a/b/"} {
		if isWorkspaceRelative(p) {
			t.Errorf("isWorkspaceRelative(%q) = true, want false (non-canonical)", p)
		}
	}
}

// TestPublishSafety_KindEmbeddedContentRejected pins that ArtifactRef.Kind — an
// unrestricted string — is covered by the embedded-content sweep.
func TestPublishSafety_KindEmbeddedContentRejected(t *testing.T) {
	d := validDocument()
	d.Capabilities = []ArtifactRef{{Path: "schema/capabilities.json", Digest: goodDigest, Kind: strings.Repeat("x", defaultMaxStringBytes+1)}}
	if !findCode(ValidatePublishSafety(d, PublishSafetyOptions{}), ErrorCodeEmbeddedContent) {
		t.Errorf("an oversized ArtifactRef.Kind must fail the gate with %s", ErrorCodeEmbeddedContent)
	}
}

// TestParseDocument_TrailingDataRejected pins that the strict decoder requires
// exactly one JSON value: a valid document followed by trailing JSON fails.
func TestParseDocument_TrailingDataRejected(t *testing.T) {
	data, err := json.Marshal(validDocument())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, diags := ParseDocument(append(data, []byte("{}")...)); !findCode(diags, ErrorCodeParseError) {
		t.Errorf("trailing JSON after the document must fail with %s", ErrorCodeParseError)
	}
	if _, diags := ParseDocument(data); diag.HasErrors(diags) {
		t.Errorf("the same document without a trailer must parse clean: %v", diags)
	}
}

// TestValidateTests_PackMissingIDRejected pins that pack entries are validated:
// "packs":[{}] must not validate despite the section being non-empty.
func TestValidateTests_PackMissingIDRejected(t *testing.T) {
	d := validDocument()
	d.Tests = &TestsSection{Policy: TestPolicyAuto, Packs: []PackRef{{}}}
	if !findCode(ValidateDocument(d), ErrorCodeInvalidPack) {
		t.Errorf("a pack entry without an id must fail with %s", ErrorCodeInvalidPack)
	}
}

// --- Taxonomy + enum maps ---

// TestDiagnosticTaxonomy_Membership asserts every constant is registered in
// ValidDiagnosticCodes so tooling can trust the map is exhaustive.
func TestDiagnosticTaxonomy_Membership(t *testing.T) {
	required := []string{
		ErrorCodeParseError,
		ErrorCodeUnknownField,
		ErrorCodeInvalidProtocolVersion,
		ErrorCodeInvalidIdentity,
		ErrorCodeDuplicateID,
		ErrorCodeInvalidRootKind,
		ErrorCodeInvalidSourceReason,
		ErrorCodeInvalidDocRelationship,
		ErrorCodeInvalidTestPolicy,
		ErrorCodeInvalidAbsenceReason,
		ErrorCodeMissingAbsenceReason,
		ErrorCodeInvalidPack,
		ErrorCodeInvalidTokenMethod,
		ErrorCodeInvalidAggregationMethod,
		ErrorCodeInvalidDigest,
		ErrorCodeInvalidRange,
		ErrorCodeInvalidTokenEstimate,
		ErrorCodeInvalidPath,
		ErrorCodeDuplicateRef,
		ErrorCodeMissingProvenance,
		ErrorCodeUnredactedSensitive,
		ErrorCodeEmbeddedContent,
	}
	for _, code := range required {
		if !ValidDiagnosticCodes[code] {
			t.Errorf("ValidDiagnosticCodes missing %q", code)
		}
	}
	if len(ValidDiagnosticCodes) != len(required) {
		t.Errorf("ValidDiagnosticCodes has %d entries, want %d", len(ValidDiagnosticCodes), len(required))
	}
}

// TestTestPolicy_MirrorsDatabaseTestMode pins the three policy strings to the
// database TestMode values they mirror; if database renames one, this fails so
// the two contracts never silently diverge.
func TestTestPolicy_MirrorsDatabaseTestMode(t *testing.T) {
	if TestPolicyAuto != "auto" || TestPolicyRequire != "require" || TestPolicySkip != "skip" {
		t.Errorf("TestPolicy values must mirror database TestMode strings auto/require/skip, got %q/%q/%q",
			TestPolicyAuto, TestPolicyRequire, TestPolicySkip)
	}
}

// TestConstants pins the canonical filenames and paths tooling depends on.
func TestConstants(t *testing.T) {
	if DocumentFilename != "agent-context.json" {
		t.Errorf("DocumentFilename = %q", DocumentFilename)
	}
	if DocumentEmitDir != ".gen" {
		t.Errorf("DocumentEmitDir = %q, want .gen (ephemeral, never promoted)", DocumentEmitDir)
	}
	if OverridesPath != "schema/agent-context.overrides.json" {
		t.Errorf("OverridesPath = %q", OverridesPath)
	}
}
