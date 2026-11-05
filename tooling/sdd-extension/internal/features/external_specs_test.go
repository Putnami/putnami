package features

import (
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/conformance"
	"go.putnami.dev/protocol/features/spectest"
	workspaceproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/sdd/extension/internal/gittest"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

const externalManifest = `{"protocolVersion":3,"namespace":"billing","features":[` +
	`{"id":"billing/invoice-export","type":"feature","name":"Export","outcome":"O","owner":"o","target":"modeled",` +
	`"spec":{"format":"openspec","path":"app/openspec/invoice"}}]}`

func minimalLookup(format featureproto.SpecFormat) (featureproto.SpecReader, bool) {
	return conformance.MinimalSpecReader{}, format == featureproto.SpecFormatOpenSpec
}

func externalWorkspace(t *testing.T, mode string, formats ...any) *workspace.Workspace {
	t.Helper()
	root := t.TempDir()
	writeDiscoveryFixture(t, filepath.Join(root, "app", "putnami.features.json"), externalManifest)
	options := map[string]map[string]any{"sdd": {"specFormats": formats}}
	if mode != "" {
		options["sdd"]["verification"] = map[string]any{"specs": mode}
	}
	return workspace.NewWorkspace(root, &workspaceproto.Config{Name: "fixture"}, []*workspace.Project{
		{Name: "app", Path: "app", Config: &workspaceproto.ProjectConfig{Name: "app", Options: options}},
	})
}

func discoverExternal(t *testing.T, ws *workspace.Workspace) SpecDiscoveryResult {
	t.Helper()
	reader, err := NewOSReader(ws.Root)
	if err != nil {
		t.Fatal(err)
	}
	return discoverSpecs(ws, nil, reader, func() (Reader, error) { return reader, nil }, minimalLookup)
}

func findingCodes(findings []diag.Diagnostic) string {
	parts := make([]string, 0, len(findings))
	for _, finding := range findings {
		parts = append(parts, finding.Code+"@"+finding.Field)
	}
	return strings.Join(parts, ",")
}

func TestExternalSpecIsProjectedByItsReader(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "external-specs-share-one-discovery", "an-external-spec-is-projected-by-its-reader")
	ws := externalWorkspace(t, "", "openspec")
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "openspec", "invoice", "spec.md"), "outcome O\nrequirement Export it | Text.\n")
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "openspec", "invoice", "notes.txt"), "requirement Not read | x\n")
	result := discoverExternal(t, ws)
	if len(result.Diagnostics) != 0 || len(result.Specs) != 1 {
		t.Fatalf("specs = %#v, diagnostics = %s", result.Specs, findingCodes(result.Diagnostics))
	}
	spec := result.Specs[0]
	if spec.Path != "app/openspec/invoice" || spec.Project != "app" || spec.Format != featureproto.SpecFormatOpenSpec ||
		spec.Spec.Feature != "billing/invoice-export" || len(spec.Spec.Requirements) != 1 || strings.Join(spec.Files, ",") != "app/openspec/invoice/spec.md" {
		t.Fatalf("spec = %#v", spec)
	}
	if findings := featureproto.ValidateSpecRepository(result.Sources(), []string{"billing/invoice-export"}); len(findings) != 0 {
		t.Fatalf("repository findings = %s", findingCodes(findings))
	}
}

func TestFeatureWithNativeAndExternalSpecsFails(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "external-specs-share-one-discovery", "a-feature-with-native-and-external-specs-fails")
	ws := externalWorkspace(t, "", "openspec")
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "openspec", "invoice", "spec.md"), "outcome O\n")
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "specs", "invoice.json"), specJSON("billing/invoice-export"))
	result := discoverExternal(t, ws)
	findings := featureproto.ValidateSpecRepository(result.Sources(), []string{"billing/invoice-export"})
	if !strings.Contains(findingCodes(findings), featureproto.ErrorCodeDuplicateSpec) {
		t.Fatalf("findings = %s", findingCodes(findings))
	}
}

func TestTwoFeaturesCannotShareASpecPath(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "external-specs-share-one-discovery", "two-features-cannot-share-a-spec-path")
	ws := externalWorkspace(t, "", "openspec")
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "openspec", "invoice", "spec.md"), "outcome O\n")
	second := strings.TrimSuffix(externalManifest, `]}`) + `,{"id":"billing/other","type":"feature","name":"Other","outcome":"O","owner":"o","target":"modeled",` +
		`"spec":{"format":"openspec","path":"app/openspec/invoice"}}]}`
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "putnami.features.json"), second)
	result := discoverExternal(t, ws)
	if got := findingCodes(result.Diagnostics); got != featureproto.ErrorCodeDuplicateSpec+"@app/putnami.features.json#features[1].spec" {
		t.Fatalf("findings = %s", got)
	}
}

func TestWorkspaceWithNoReferenceReadsNoExternalFile(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "external-specs-share-one-discovery", "a-workspace-with-no-reference-reads-no-external-file")
	root := t.TempDir()
	writeDiscoveryFixture(t, filepath.Join(root, "specs", "a.json"), specJSON("billing/checkout"))
	writeDiscoveryFixture(t, filepath.Join(root, "openspec", "spec.md"), "outcome O\n")
	ws := workspace.NewWorkspace(root, &workspaceproto.Config{Name: "fixture"}, nil)
	reader, err := NewOSReader(root)
	if err != nil {
		t.Fatal(err)
	}
	opened := false
	result := discoverSpecs(ws, nil, reader, func() (Reader, error) { opened = true; return reader, nil }, minimalLookup)
	if opened || len(result.Specs) != 1 || len(result.Diagnostics) != 0 {
		t.Fatalf("opened = %v, specs = %#v, diagnostics = %s", opened, result.Specs, findingCodes(result.Diagnostics))
	}
}

func TestFormatTheProjectDidNotOptIntoFails(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "a-spec-format-needs-an-opt-in", "a-format-the-project-did-not-opt-into-fails")
	for _, mode := range []string{"off", "report", "enforce"} {
		ws := externalWorkspace(t, mode, "kiro")
		writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "openspec", "invoice", "spec.md"), "outcome O\n")
		result := discoverExternal(t, ws)
		want := featureproto.ErrorCodeSpecFormatNotEnabled + "@app/putnami.features.json#features[0].spec.format"
		if got := findingCodes(result.Diagnostics); got != want || len(result.Specs) != 0 {
			t.Errorf("%s: findings = %s, specs = %d", mode, got, len(result.Specs))
		}
	}
}

func TestFormatWithNoReaderFailsAndSaysSo(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "a-spec-format-needs-an-opt-in", "a-format-with-no-reader-fails-and-says-so")
	ws := externalWorkspace(t, "", "openspec")
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "openspec", "invoice", "spec.md"), "outcome O\n")
	reader, err := NewOSReader(ws.Root)
	if err != nil {
		t.Fatal(err)
	}
	none := func(featureproto.SpecFormat) (featureproto.SpecReader, bool) { return nil, false }
	result := discoverSpecs(ws, nil, reader, func() (Reader, error) { return reader, nil }, none)
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != featureproto.ErrorCodeSpecFormatUnavailable ||
		!strings.Contains(result.Diagnostics[0].Message, "ships no reader") {
		t.Fatalf("findings = %#v", result.Diagnostics)
	}
}

func TestEnforceFailsAnUnanchoredPositionalRequirement(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "enforce-needs-anchors", "enforce-fails-an-unanchored-positional-requirement")
	ws := externalWorkspace(t, "enforce", "openspec")
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "openspec", "invoice", "spec.md"),
		"outcome O\n<!-- putnami:id anchored -->\npositional FR-001 | Kept.\npositional FR-002 | Refused.\n")
	result := discoverExternal(t, ws)
	want := featureproto.ErrorCodeSpecRequirementUnanchored + "@app/openspec/invoice/spec.md#L4"
	if got := findingCodes(result.Diagnostics); got != want {
		t.Fatalf("findings = %s", got)
	}
}

func TestReportProjectsAnUnanchoredPositionalRequirement(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "enforce-needs-anchors", "report-projects-an-unanchored-positional-requirement")
	ws := externalWorkspace(t, "report", "openspec")
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "openspec", "invoice", "spec.md"), "outcome O\npositional FR-002 | Projected.\n")
	result := discoverExternal(t, ws)
	if len(result.Diagnostics) != 0 || len(result.Specs) != 1 || result.Specs[0].Spec.Requirements[0].ID != "fr-002" {
		t.Fatalf("specs = %#v, findings = %s", result.Specs, findingCodes(result.Diagnostics))
	}
}

func TestIgnoredExternalSpecIsAbsent(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "external-spec-files-key-the-spec-tasks", "an-ignored-external-spec-is-absent")
	ws := externalWorkspace(t, "", "openspec")
	writeDiscoveryFixture(t, filepath.Join(ws.Root, ".gitignore"), "app/openspec/invoice/ignored.md\n")
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "openspec", "invoice", "spec.md"), "outcome O\n")
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "openspec", "invoice", "ignored.md"), "requirement Ignored | x\n")
	if output, err := gittest.Command(ws.Root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	native, err := NewOSReader(ws.Root)
	if err != nil {
		t.Fatal(err)
	}
	result := discoverSpecs(ws, nil, native, func() (Reader, error) { return NewTaskReader(ws.Root) }, minimalLookup)
	if len(result.Specs) != 1 || len(result.Specs[0].Files) != 1 || len(result.Specs[0].Spec.Requirements) != 0 {
		t.Fatalf("specs = %#v, findings = %s", result.Specs, findingCodes(result.Diagnostics))
	}
}

func TestSpecReferenceToOneFileAndToNothing(t *testing.T) {
	ws := externalWorkspace(t, "", "openspec")
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "putnami.features.json"),
		strings.Replace(externalManifest, "app/openspec/invoice", "app/openspec/invoice/spec.md", 1))
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "openspec", "invoice", "spec.md"), "outcome O\n")
	if result := discoverExternal(t, ws); len(result.Specs) != 1 || len(result.Diagnostics) != 0 {
		t.Fatalf("one file: %#v, %s", result.Specs, findingCodes(result.Diagnostics))
	}
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "putnami.features.json"),
		strings.Replace(externalManifest, "app/openspec/invoice", "app/missing.md", 1))
	if got := findingCodes(discoverExternal(t, ws).Diagnostics); got != featureproto.ErrorCodeSpecRead+"@app/putnami.features.json#features[0].spec.path" {
		t.Fatalf("missing file: %s", got)
	}
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "putnami.features.json"),
		strings.Replace(externalManifest, "app/openspec/invoice", "app/missing", 1))
	if got := findingCodes(discoverExternal(t, ws).Diagnostics); got != featureproto.ErrorCodeInvalidSpecReference+"@app/putnami.features.json#features[0].spec.path" {
		t.Fatalf("missing directory: %s", got)
	}
}

func TestOverlappingSpecPathsFail(t *testing.T) {
	ws := externalWorkspace(t, "", "openspec")
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "openspec", "invoice", "spec.md"), "outcome O\n")
	second := strings.TrimSuffix(externalManifest, `]}`) + `,{"id":"billing/other","type":"feature","name":"Other","outcome":"O","owner":"o","target":"modeled",` +
		`"spec":{"format":"openspec","path":"app/openspec/invoice/spec.md"}}]}`
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "putnami.features.json"), second)
	if got := findingCodes(discoverExternal(t, ws).Diagnostics); got != featureproto.ErrorCodeDuplicateSpec+"@app/putnami.features.json#features[1].spec" {
		t.Fatalf("findings = %s", got)
	}
}

func TestScopedRunKeepsItsExternalSpec(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "external-specs-share-one-discovery", "a-scoped-run-keeps-its-external-spec")
	ws := externalWorkspace(t, "", "openspec")
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "openspec", "invoice", "spec.md"), "outcome O\nrequirement A | x\n")
	// A feature the selection does not own names the same path after the owner.
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "zed", "putnami.features.json"),
		strings.ReplaceAll(strings.ReplaceAll(externalManifest, "billing", "other"), "other/invoice-export", "other/late"))
	ws.Projects = append(ws.Projects, &workspace.Project{Name: "zed", Path: "zed", Config: ws.Projects[0].Config})
	scope := NewScope([]string{"app"})
	reader, err := NewOSReader(ws.Root)
	if err != nil {
		t.Fatal(err)
	}
	result := discoverSpecs(ws, scope, reader, func() (Reader, error) { return reader, nil }, minimalLookup)
	if len(result.Selected) != 1 || result.Selected[0].Spec.Feature != "billing/invoice-export" || len(result.Diagnostics) != 0 {
		t.Fatalf("selected = %#v, findings = %s", result.Selected, findingCodes(result.Diagnostics))
	}
}

const specKitManifest = `{"protocolVersion":3,"namespace":"billing","features":[` +
	`{"id":"billing/invoice-export","type":"feature","name":"Export","outcome":"O","owner":"o","target":"modeled",` +
	`"spec":{"format":"speckit","path":"app/specs/001-invoice-export"}},` +
	`{"id":"billing/checkout","type":"feature","name":"Checkout","outcome":"O","owner":"o","target":"modeled"}]}`

const specKitAnchored = "## Functional Requirements\n\n<!-- putnami:id export-it -->\n- **FR-001**: System MUST export.\n\n## Success Criteria\n\n- **SC-001**: Done.\n"

// specKitWorkspace holds one project whose specs/ carries a native spec and a
// Spec Kit feature directory side by side, read by the reader that ships.
func specKitWorkspace(t *testing.T, mode, spec string) *workspace.Workspace {
	t.Helper()
	root := t.TempDir()
	writeDiscoveryFixture(t, filepath.Join(root, "app", "putnami.features.json"), specKitManifest)
	writeDiscoveryFixture(t, filepath.Join(root, "app", "specs", "checkout.json"), specJSON("billing/checkout"))
	writeDiscoveryFixture(t, filepath.Join(root, "app", "specs", "001-invoice-export", "spec.md"), spec)
	options := map[string]map[string]any{"sdd": {"specFormats": []any{"speckit"}}}
	if mode != "" {
		options["sdd"]["verification"] = map[string]any{"specs": mode}
	}
	return workspace.NewWorkspace(root, &workspaceproto.Config{Name: "fixture"}, []*workspace.Project{
		{Name: "app", Path: "app", Config: &workspaceproto.ProjectConfig{Name: "app", Options: options}},
	})
}

func discoverSpecKit(t *testing.T, ws *workspace.Workspace) SpecDiscoveryResult {
	t.Helper()
	reader, err := NewOSReader(ws.Root)
	if err != nil {
		t.Fatal(err)
	}
	return discoverSpecs(ws, nil, reader, func() (Reader, error) { return reader, nil }, featureproto.SpecReaderFor)
}

func TestNativeAndSpecKitSpecsCoexistWithoutAFinding(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "external-specs-share-one-discovery", "native-and-speckit-specs-coexist-without-a-finding")
	ws := specKitWorkspace(t, "", specKitAnchored)
	// JSON below a Spec Kit directory stays out of native discovery, which reads
	// only the direct children of specs/.
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "specs", "001-invoice-export", "contracts", "api.json"), `{"not":"a spec"}`)
	writeDiscoveryFixture(t, filepath.Join(ws.Root, "app", "specs", "001-invoice-export", "stray.json"), specJSON("billing/invoice-export"))
	result := discoverSpecKit(t, ws)
	if len(result.Diagnostics) != 0 || len(result.Specs) != 2 {
		t.Fatalf("specs = %d, diagnostics = %s", len(result.Specs), findingCodes(result.Diagnostics))
	}
	if findings := featureproto.ValidateSpecRepository(result.Sources(), []string{"billing/checkout", "billing/invoice-export"}); len(findings) != 0 {
		t.Fatalf("repository findings = %s", findingCodes(findings))
	}
}

func TestEnforceFailsAnInsertedSpecKitRequirementWithoutAnAnchor(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "enforce-needs-anchors", "enforce-fails-an-inserted-speckit-requirement-without-an-anchor")
	if result := discoverSpecKit(t, specKitWorkspace(t, "enforce", specKitAnchored)); len(result.Diagnostics) != 0 {
		t.Fatalf("anchored diagnostics = %s", findingCodes(result.Diagnostics))
	}
	inserted := strings.Replace(specKitAnchored, "<!-- putnami:id export-it -->", "- **FR-002**: System MUST log.\n<!-- putnami:id export-it -->", 1)
	want := featureproto.ErrorCodeSpecRequirementUnanchored + "@app/specs/001-invoice-export/spec.md#L3"
	if got := findingCodes(discoverSpecKit(t, specKitWorkspace(t, "enforce", inserted)).Diagnostics); got != want {
		t.Fatalf("findings = %s", got)
	}
}

func TestEnforceFailsANeedsClarificationMarker(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "enforce-needs-anchors", "enforce-fails-a-needs-clarification-marker")
	spec := strings.Replace(specKitAnchored, "export.", "export [NEEDS CLARIFICATION: where?].", 1)
	want := featureproto.FindingCodeSpecNeedsClarification + "@app/specs/001-invoice-export/spec.md#L4"
	enforce := discoverSpecKit(t, specKitWorkspace(t, "enforce", spec)).Diagnostics
	if findingCodes(enforce) != want || !diag.HasErrors(enforce) {
		t.Fatalf("enforce = %v", enforce)
	}
	report := discoverSpecKit(t, specKitWorkspace(t, "report", spec)).Diagnostics
	if findingCodes(report) != want || diag.HasErrors(report) {
		t.Fatalf("report = %v", report)
	}
}
