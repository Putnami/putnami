package sdd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	supportproto "go.putnami.dev/protocol/support"
	wsproto "go.putnami.dev/protocol/workspace"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// TestSpecsValidateFailsOnlyOnBrokenContract pins the exit-code boundary: an
// invalid document, an unresolvable feature reference, an unsafe decision path,
// and a second spec for one feature fail; every completeness gap stays a
// warning.
func TestSpecsValidateFailsOnlyOnBrokenContract(t *testing.T) {
	ws := specWorkspace(t)
	root := ws.Root
	writeFixtureFile(t, filepath.Join(root, "app", "specs", "invoice.json"), specDocument("billing/invoice-export"))
	writeFixtureFile(t, filepath.Join(root, "specs", "duplicate.json"), specDocument("billing/invoice-export"))
	writeFixtureFile(t, filepath.Join(root, "specs", "unknown.json"), specDocument("billing/never-declared"))
	writeFixtureFile(t, filepath.Join(root, "specs", "unsafe.json"), `{
  "protocolVersion": 1,
  "feature": "billing/refunds",
  "outcomes": ["Customers receive refunds"],
  "requirements": [],
  "decisions": ["../outside/doc/adr/0001-escape.md"]
}`)
	writeFixtureFile(t, filepath.Join(root, "specs", "unknown-field.json"), `{
  "protocolVersion": 1,
  "feature": "billing/refunds",
  "outcomes": ["Customers receive refunds"],
  "requirements": [],
  "status": "approved"
}`)

	_, err := BuildSpecValidationResult(ws, Selection{})
	if !errors.Is(err, protocolcli.ErrInvalidConfig) {
		t.Fatalf("specs validate error = %v, want ErrInvalidConfig", err)
	}
	report, ok := ResultData(err).(SpecValidationReport)
	if !ok {
		t.Fatalf("failure data = %T, want SpecValidationReport", ResultData(err))
	}
	if report.Valid || report.Counts.Errors == 0 {
		t.Fatalf("failure report = %+v", report)
	}
	wantCodes := map[string]bool{
		featureproto.ErrorCodeDuplicateSpec:  false,
		featureproto.ErrorCodeUnknownFeature: false,
		featureproto.ErrorCodePathEscape:     false,
		featureproto.ErrorCodeUnknownField:   false,
	}
	for _, finding := range report.Diagnostics {
		if _, tracked := wantCodes[finding.Code]; tracked && finding.Severity == diag.Error {
			wantCodes[finding.Code] = true
		}
	}
	for code, seen := range wantCodes {
		if !seen {
			t.Errorf("validation did not fail on %s:\n%+v", code, report.Diagnostics)
		}
	}
	// The unsafe link is refused by the protocol, which deliberately keeps the
	// offending path out of the message; resolution must not reintroduce it.
	// Asserting on the diagnostics rather than on rendered output moves the
	// check to where the message is built.
	for _, finding := range report.Diagnostics {
		if strings.Contains(finding.Message, "../outside") {
			t.Errorf("a diagnostic echoed an unsafe decision path: %+v", finding)
		}
	}
	// A document that fails strict parsing never becomes a spec: it is reported
	// and skipped, so it cannot claim a feature or silence a completeness gap.
	if report.Summary.Specs != 4 {
		t.Errorf("summary = %+v, want the unparsable document excluded from the spec count", report.Summary)
	}
}

// TestSpecsValidateReportsCompletenessWithoutFailing pins that the three
// authoring gaps are reported as warnings and never change the exit code.
func TestSpecsValidateReportsCompletenessWithoutFailing(t *testing.T) {
	ws := specWorkspace(t)
	root := ws.Root
	writeFixtureFile(t, filepath.Join(root, supportproto.CatalogFilename), `{
  "protocolVersion": 1,
  "entries": [{"id": "billing-app", "kind": "package", "status": "preview"}]
}`)

	report, err := BuildSpecValidationResult(ws, Selection{})
	if err != nil {
		t.Fatalf("specs validate: %v", err)
	}
	if !report.Valid || report.Counts.Errors != 0 {
		t.Fatalf("report = %+v, want a warning-only result", report)
	}
	if !report.Completeness.SupportAssessed {
		t.Fatalf("support was not assessed even though the catalog is readable: %+v", report.Completeness)
	}
	if got := report.Completeness.FeaturesWithoutSpec; len(got) != 2 {
		t.Fatalf("featuresWithoutSpec = %v, want both authored features", got)
	}
	// billing-app is classified and declares a feature; docs-site is neither.
	if !containsString(report.Completeness.ProjectsWithoutSupport, "docs-site") || containsString(report.Completeness.ProjectsWithoutSupport, "billing-app") {
		t.Fatalf("projectsWithoutSupport = %v", report.Completeness.ProjectsWithoutSupport)
	}
	if !containsString(report.Completeness.ProjectsWithoutFeature, "docs-site") || containsString(report.Completeness.ProjectsWithoutFeature, "billing-app") {
		t.Fatalf("projectsWithoutFeature = %v", report.Completeness.ProjectsWithoutFeature)
	}
	codes := map[string]bool{}
	for _, finding := range report.Diagnostics {
		if finding.Severity != diag.Warning {
			t.Fatalf("completeness produced a non-warning finding: %+v", finding)
		}
		codes[finding.Code] = true
	}
	for _, code := range []string{SpecCodeMissingSpec, SpecCodeMissingSupportEntry, SpecCodeMissingFeatureLink} {
		if !codes[code] {
			t.Errorf("missing %s warning in %+v", code, report.Diagnostics)
		}
	}
	if finding, found := findDiagnostic(report.Diagnostics, SpecCodeMissingFeatureLink); !found ||
		!strings.Contains(finding.Message, "publishable project") {
		t.Errorf("missing-feature wording = %+v, want publishable project", finding)
	}
}

// TestSpecsValidateMatchesSupportKindAndID pins the identity boundary used by
// completeness: a publishable protocol project requires a protocol entry. An
// equal package id is a different subject and leaves a warning-only gap.
func TestSpecsValidateMatchesSupportKindAndID(t *testing.T) {
	for _, test := range []struct {
		name        string
		kind        supportproto.SubjectKind
		wantMissing bool
	}{
		{name: "protocol entry satisfies protocol project", kind: supportproto.SubjectKindProtocol},
		{name: "package entry with same id does not satisfy protocol project", kind: supportproto.SubjectKindPackage, wantMissing: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"specs-test","includes":["wire"]}`)
			writeFixtureFile(t, filepath.Join(root, "wire", "putnami.json"), `{
  "name": "go.putnami.dev/protocol/example",
  "type": "library",
  "tags": ["protocol"],
  "publish": ["go"]
}`)
			writeFixtureFile(t, filepath.Join(root, supportproto.CatalogFilename), `{
  "protocolVersion": 1,
  "entries": [{"id": "go.putnami.dev/protocol/example", "kind": "`+string(test.kind)+`", "status": "preview"}]
}`)
			ws := fixtureWorkspace("specs-test", root, &workspace.Project{
				ID: "/wire", Name: "go.putnami.dev/protocol/example", SourceName: "go.putnami.dev/protocol/example",
				Type: "library", Path: "wire", Tags: []string{"protocol"}, Publish: []string{"go"},
				Config: &wsproto.ProjectConfig{
					Name: "go.putnami.dev/protocol/example", Type: "library",
					Tags: []string{"protocol"}, Publish: []string{"go"},
				},
			})

			report, err := BuildSpecValidationResult(ws, Selection{})
			if err != nil {
				t.Fatalf("specs validate: %v", err)
			}
			if !report.Valid || report.Counts.Errors != 0 {
				t.Fatalf("support gap must remain warning-only: %+v", report)
			}
			missing := containsString(report.Completeness.ProjectsWithoutSupport, "go.putnami.dev/protocol/example")
			if missing != test.wantMissing {
				t.Fatalf("projectsWithoutSupport = %v, wantMissing %v", report.Completeness.ProjectsWithoutSupport, test.wantMissing)
			}
			if got := hasDiagnosticCode(report.Diagnostics, SpecCodeMissingSupportEntry); got != test.wantMissing {
				t.Fatalf("missing-support warning = %v, want %v; diagnostics: %+v", got, test.wantMissing, report.Diagnostics)
			}
		})
	}
}

// Completeness must see every project that is actually public, not only the
// ones using the top-level `publish` array. TypeScript packages publish through
// `options.publish.npm` and extensions through `options.publish.archives`, and
// a catalog-classified package may declare no channel at all — keying the check
// on the array alone silently skipped 17 classified subjects, including the
// entire TypeScript framework family. A deployable workload publishing a docker
// image is not a package and must stay out.
func TestSpecCompletenessAssessesEveryPublicSubjectAndSkipsDeployables(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"),
		`{"name":"specs-test","includes":["pkg","ext","sdk","site"]}`)
	// npm package, extension archive, catalog-classified with no channel, and a
	// docker-only site. Only the first three are public packages.
	writeFixtureFile(t, filepath.Join(root, "pkg", "putnami.json"),
		`{"name":"@scope/pkg","type":"library","options":{"publish":{"npm":true}}}`)
	writeFixtureFile(t, filepath.Join(root, "ext", "putnami.json"),
		`{"name":"@scope/ext","type":"library","options":{"publish":{"archives":true}}}`)
	writeFixtureFile(t, filepath.Join(root, "sdk", "putnami.json"),
		`{"name":"@scope/sdk","type":"library"}`)
	writeFixtureFile(t, filepath.Join(root, "site", "putnami.json"),
		`{"name":"site","type":"application","options":{"publish":{"docker":true,"archives":false}}}`)
	writeFixtureFile(t, filepath.Join(root, supportproto.CatalogFilename), `{
  "protocolVersion": 1,
  "entries": [{"id": "@scope/sdk", "kind": "package", "status": "stable"}]
}`)
	// The four options.publish blocks above are read from putnami.json by the
	// loader; the wire delivers them as the project's parsed config.
	publishOption := func(channel string, enabled bool, extra ...string) *wsproto.ProjectConfig {
		options := map[string]any{channel: enabled}
		for index := 0; index+1 < len(extra); index += 2 {
			options[extra[index]] = extra[index+1] == "true"
		}
		return &wsproto.ProjectConfig{Options: map[string]map[string]any{"publish": options}}
	}
	ws := fixtureWorkspace("specs-test", root,
		&workspace.Project{ID: "/pkg", Name: "@scope/pkg", Type: "library", Path: "pkg", Config: publishOption("npm", true)},
		&workspace.Project{ID: "/ext", Name: "@scope/ext", Type: "library", Path: "ext", Config: publishOption("archives", true)},
		&workspace.Project{ID: "/sdk", Name: "@scope/sdk", Type: "library", Path: "sdk", Config: &wsproto.ProjectConfig{Name: "@scope/sdk", Type: "library"}},
		&workspace.Project{ID: "/site", Name: "site", Type: "application", Path: "site", Config: publishOption("docker", true, "archives", "false")})

	report, err := BuildSpecValidationResult(ws, Selection{})
	if err != nil {
		t.Fatalf("specs validate: %v", err)
	}
	if report.Summary.PublishableProjects != 3 {
		t.Fatalf("PublishableProjects = %d, want 3 (npm, archives, classified)", report.Summary.PublishableProjects)
	}
	for _, name := range []string{"@scope/pkg", "@scope/ext", "@scope/sdk"} {
		if !containsString(report.Completeness.ProjectsWithoutFeature, name) {
			t.Errorf("%s was not assessed: %v", name, report.Completeness.ProjectsWithoutFeature)
		}
	}
	if containsString(report.Completeness.ProjectsWithoutFeature, "site") ||
		containsString(report.Completeness.ProjectsWithoutSupport, "site") {
		t.Errorf("docker-only deployable must not be assessed as a package: %+v", report.Completeness)
	}
	// @scope/sdk is classified, so only the two unclassified ones may warn.
	if got := report.Completeness.ProjectsWithoutSupport; len(got) != 2 {
		t.Errorf("ProjectsWithoutSupport = %v, want the two unclassified packages", got)
	}
}

// featureAuthority closes the completeness gap for a project that deliberately
// has no feature of its own, without becoming a way to silence the check: an
// `owner` naming no authored feature must report louder than the missing link
// it claims to explain.
func TestSpecCompletenessHonorsFeatureAuthorityButNotAnUnresolvableClaim(t *testing.T) {
	for _, test := range []struct {
		name        string
		authority   string
		wantLinked  bool
		wantUnknown bool
	}{
		{name: "no declaration", authority: ""},
		{
			name:       "deliberately none",
			authority:  `,"featureAuthority":{"none":"a wire contract is not a user outcome"}`,
			wantLinked: true,
		},
		{
			name:       "owned by another project's feature",
			authority:  `,"featureAuthority":{"owner":"demo/outcome"}`,
			wantLinked: true,
		},
		{
			name:        "owner names no authored feature",
			authority:   `,"featureAuthority":{"owner":"demo/does-not-exist"}`,
			wantUnknown: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"),
				`{"name":"specs-test","includes":["owner","wire"]}`)
			// The owning project declares the feature; the wire project does not.
			writeFixtureFile(t, filepath.Join(root, "owner", "putnami.json"),
				`{"name":"@scope/owner","type":"library","publish":["npm"]}`)
			writeFixtureFile(t, filepath.Join(root, "owner", "putnami.features.json"), `{
  "protocolVersion": 1,
  "namespace": "demo",
  "features": [{"id":"demo/outcome","type":"feature","name":"Outcome","outcome":"A user gets an outcome","owner":"demo","target":"modeled"}]
}`)
			writeFixtureFile(t, filepath.Join(root, "owner", "specs", "outcome.json"), `{
  "protocolVersion": 1,
  "feature": "demo/outcome",
  "outcomes": ["A user gets an outcome"],
  "nonGoals": [],
  "requirements": [],
  "decisions": []
}`)
			writeFixtureFile(t, filepath.Join(root, "wire", "putnami.json"),
				`{"name":"@scope/wire","type":"library","publish":["npm"]`+test.authority+`}`)
			wireConfig := &wsproto.ProjectConfig{Name: "@scope/wire", Type: "library", Publish: []string{"npm"}}
			switch {
			case strings.Contains(test.authority, `"none"`):
				wireConfig.FeatureAuthority = &wsproto.ProjectFeatureAuthority{None: "a wire contract is not a user outcome"}
			case strings.Contains(test.authority, "demo/does-not-exist"):
				wireConfig.FeatureAuthority = &wsproto.ProjectFeatureAuthority{Owner: "demo/does-not-exist"}
			case strings.Contains(test.authority, "demo/outcome"):
				wireConfig.FeatureAuthority = &wsproto.ProjectFeatureAuthority{Owner: "demo/outcome"}
			}
			ws := fixtureWorkspace("specs-test", root,
				&workspace.Project{
					ID: "/owner", Name: "@scope/owner", Type: "library", Path: "owner", Publish: []string{"npm"},
					Config: &wsproto.ProjectConfig{Name: "@scope/owner", Type: "library", Publish: []string{"npm"}},
				},
				&workspace.Project{
					ID: "/wire", Name: "@scope/wire", Type: "library", Path: "wire", Publish: []string{"npm"},
					Config: wireConfig,
				})

			report, err := BuildSpecValidationResult(ws, Selection{})
			if err != nil {
				t.Fatalf("specs validate: %v", err)
			}
			if !report.Valid || report.Counts.Errors != 0 {
				t.Fatalf("completeness must stay warning-only: %+v", report)
			}
			gap := containsString(report.Completeness.ProjectsWithoutFeature, "@scope/wire")
			if gap == test.wantLinked {
				t.Fatalf("ProjectsWithoutFeature = %v, wantLinked %v", report.Completeness.ProjectsWithoutFeature, test.wantLinked)
			}
			if got := hasDiagnosticCode(report.Diagnostics, SpecCodeUnresolvedFeatureAuthority); got != test.wantUnknown {
				t.Fatalf("unresolved-authority warning = %v, want %v; diagnostics: %+v", got, test.wantUnknown, report.Diagnostics)
			}
		})
	}
}

// TestSpecCompletenessSkipsSupportWhenTheCatalogIsUnreadable pins that a broken
// authority disables the check instead of reporting every project as
// unclassified — a finding that would be an artifact of the reader.
func TestSpecCompletenessSkipsSupportWhenTheCatalogIsUnreadable(t *testing.T) {
	ws := specWorkspace(t)
	root := ws.Root
	writeFixtureFile(t, filepath.Join(root, supportproto.CatalogFilename), `{"protocolVersion": 2, "entries": []}`)

	report, err := BuildSpecValidationResult(ws, Selection{})
	if err != nil {
		t.Fatalf("specs validate: %v", err)
	}
	if report.Completeness.SupportAssessed || len(report.Completeness.ProjectsWithoutSupport) != 0 {
		t.Fatalf("completeness = %+v, want the support check skipped", report.Completeness)
	}
	if !hasDiagnosticCode(report.Diagnostics, SpecCodeUnreadableSupportCatalog) {
		t.Fatalf("diagnostics = %+v, want an explicit unreadable-catalog warning", report.Diagnostics)
	}
	if finding, _ := findDiagnostic(report.Diagnostics, SpecCodeUnreadableSupportCatalog); !strings.Contains(finding.Message, "project support completeness") {
		t.Errorf("unreadable-catalog wording = %q, want project support completeness", finding.Message)
	}
	if !report.Valid {
		t.Fatalf("an unreadable support catalog must not fail spec validation: %+v", report)
	}
}

// TestSpecsInitCreatesCanonicalSkeletonAndNeverOverwrites walks the whole
// non-destructive contract: dry-run writes nothing, create writes canonical
// bytes next to the declaration, and neither an existing file nor an existing
// spec for the feature can be replaced.
func TestSpecsInitCreatesCanonicalSkeletonAndNeverOverwrites(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "predictable-spec-scaffolding",
		"init-uses-the-feature-leaf-with-dry-run-parity")
	ws := specWorkspace(t)
	root := ws.Root
	target := filepath.Join(root, "app", "specs", "invoice-export.json")

	dryRun, err := BuildSpecInitResult(ws, "billing/invoice-export", true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dryRun.Created || dryRun.Contents == "" || dryRun.Path != "app/specs/invoice-export.json" {
		t.Fatalf("dry run report = %+v", dryRun)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("--dry-run wrote %s", target)
	}

	created, err := BuildSpecInitResult(ws, "billing/invoice-export", false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !created.Created || created.Contents != dryRun.Contents {
		t.Fatalf("create report = %+v, want the dry-run bytes", created)
	}
	written, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("read created spec: %v", readErr)
	}
	if string(written) != created.Contents {
		t.Fatalf("written bytes differ from the reported document:\n%s", written)
	}

	// The skeleton is canonical, valid, and claims nothing the declaration did
	// not state: no requirement text, no non-goal, no decision link.
	parsed, findings := featureproto.ParseAndValidateSpec(written)
	if parsed == nil || len(findings) != 0 {
		t.Fatalf("generated spec is not valid: %+v", findings)
	}
	if len(parsed.Requirements) != 0 || len(parsed.NonGoals) != 0 || len(parsed.Decisions) != 0 {
		t.Fatalf("generated spec invented content: %+v", parsed)
	}
	if len(parsed.Outcomes) != 1 || parsed.Outcomes[0] != "Customers can export issued invoices" {
		t.Fatalf("generated outcomes = %v, want the declared outcome", parsed.Outcomes)
	}
	canonical, marshalErr := featureproto.MarshalSpec(parsed)
	if marshalErr != nil || string(canonical) != string(written) {
		t.Fatalf("generated bytes are not canonical: %v\n%s", marshalErr, written)
	}

	// A second init for the same feature refuses rather than replacing it.
	duplicate, err := BuildSpecInitResult(ws, "billing/invoice-export", false)
	if !errors.Is(err, protocolcli.ErrUsage) {
		t.Fatalf("duplicate init error = %v, want ErrUsage", err)
	}
	if duplicate.Created || !hasDiagnosticCode(duplicate.Diagnostics, featureproto.ErrorCodeDuplicateSpec) {
		t.Fatalf("duplicate init report = %+v", duplicate)
	}

	// A file already sitting at the target path is never overwritten, even when
	// no valid spec claims the feature yet.
	writeFixtureFile(t, filepath.Join(root, "app", "specs", "refunds.json"), "hand-written notes, not JSON\n")
	blocked, err := BuildSpecInitResult(ws, "billing/refunds", false)
	if !errors.Is(err, protocolcli.ErrUsage) {
		t.Fatalf("no-overwrite error = %v, want ErrUsage", err)
	}
	if blocked.Created {
		t.Fatalf("init overwrote user content: %+v", blocked)
	}
	preserved, readErr := os.ReadFile(filepath.Join(root, "app", "specs", "refunds.json"))
	if readErr != nil || string(preserved) != "hand-written notes, not JSON\n" {
		t.Fatalf("user content changed: %q (%v)", preserved, readErr)
	}
}

// Two declared features may share a leaf because identity lives in the complete
// feature ID. The default path is deliberately not widened in that case: the
// second init refuses the occupied leaf and preserves the first document.
func TestSpecsInitRefusesALeafCollision(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "predictable-spec-scaffolding",
		"init-refuses-a-leaf-collision-without-overwriting")
	ws := specWorkspace(t)
	root := ws.Root
	manifestPath := filepath.Join(root, "app", featureproto.ManifestFilename)
	writeFixtureFile(t, manifestPath, `{
  "protocolVersion": 1,
  "namespace": "billing",
  "features": [{
    "id": "billing/refunds",
    "type": "feature",
    "name": "Refunds",
    "outcome": "Customers receive refunds",
    "owner": "billing",
    "target": "modeled"
  }, {
    "id": "billing/archive/refunds",
    "type": "feature",
    "name": "Archived refunds",
    "outcome": "Customers can inspect archived refunds",
    "owner": "billing",
    "target": "modeled"
  }]
}`)

	created, err := BuildSpecInitResult(ws, "billing/archive/refunds", false)
	if err != nil {
		t.Fatalf("create first leaf: %v", err)
	}
	if !created.Created || created.Feature != "billing/archive/refunds" || created.Path != "app/specs/refunds.json" {
		t.Fatalf("first init report = %+v", created)
	}

	blocked, err := BuildSpecInitResult(ws, "billing/refunds", false)
	if !errors.Is(err, protocolcli.ErrUsage) {
		t.Fatalf("leaf collision error = %v, want ErrUsage", err)
	}
	if blocked.Created || blocked.Path != created.Path || !hasDiagnosticCode(blocked.Diagnostics, featureproto.ErrorCodeInvalidPath) {
		t.Fatalf("leaf collision report = %+v", blocked)
	}
	written, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(created.Path)))
	if readErr != nil {
		t.Fatalf("read first leaf document: %v", readErr)
	}
	parsed, findings := featureproto.ParseAndValidateSpec(written)
	if parsed == nil || len(findings) != 0 || parsed.Feature != "billing/archive/refunds" {
		t.Fatalf("occupied leaf changed: spec=%+v findings=%+v", parsed, findings)
	}
}

// A syntactically contained target is still unsafe when an existing parent is
// a symlink. specs init must leave both the workspace and the link target
// untouched instead of following app/specs outside the workspace.
func TestSpecsInitRefusesASymlinkedParent(t *testing.T) {
	ws := specWorkspace(t)
	root := ws.Root
	outside := t.TempDir()
	link := filepath.Join(root, "app", featureproto.SpecDirectory)
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}

	report, err := BuildSpecInitResult(ws, "billing/refunds", false)
	if err == nil {
		t.Fatal("specs init followed a symlinked parent")
	}
	if report.Created {
		t.Fatalf("specs init reported an outside write: %+v", report)
	}
	outsideTarget := filepath.Join(outside, "refunds.json")
	if _, statErr := os.Stat(outsideTarget); !os.IsNotExist(statErr) {
		t.Fatalf("specs init wrote outside the workspace: %v", statErr)
	}
	if info, statErr := os.Lstat(link); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the parent symlink changed: info=%v err=%v", info, statErr)
	}
}

func TestSpecsInitRefusesAnUnknownFeature(t *testing.T) {
	ws := specWorkspace(t)
	root := ws.Root
	report, err := BuildSpecInitResult(ws, "billing/never-declared", false)
	if !errors.Is(err, protocolcli.ErrNotFound) {
		t.Fatalf("unknown feature error = %v, want ErrNotFound", err)
	}
	if report.Created || report.Path != "" {
		t.Fatalf("unknown feature report = %+v", report)
	}
	if !hasDiagnosticCode(report.Diagnostics, featureproto.ErrorCodeUnknownFeature) {
		t.Fatalf("diagnostics = %+v", report.Diagnostics)
	}
	entries, _ := os.ReadDir(filepath.Join(root, "app"))
	for _, entry := range entries {
		if entry.Name() == featureproto.SpecDirectory {
			t.Fatalf("a refused init created %s/", featureproto.SpecDirectory)
		}
	}
}

// TestSpecInspectAnswersAnAuthoredFeatureWithNoSpec keeps the two "missing"
// cases distinguishable: an undeclared feature is not found at all, while a
// declared one without a spec still returns its declaration.
func TestSpecInspectAnswersAnAuthoredFeatureWithNoSpec(t *testing.T) {
	ws := specWorkspace(t)
	report, err := BuildSpecContextResult(ws, "billing/invoice-export")
	if !errors.Is(err, protocolcli.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if report.Spec != nil || len(report.Declarations) != 1 {
		t.Fatalf("report = %+v, want the declaration without a spec", report)
	}
	if !hasDiagnosticCode(report.Diagnostics, SpecCodeMissingSpec) {
		t.Fatalf("diagnostics = %+v", report.Diagnostics)
	}

	undeclared, err := BuildSpecContextResult(ws, "billing/never-declared")
	if !errors.Is(err, protocolcli.ErrNotFound) {
		t.Fatalf("undeclared error = %v, want ErrNotFound", err)
	}
	if len(undeclared.Declarations) != 0 || !hasDiagnosticCode(undeclared.Diagnostics, featureproto.ErrorCodeUnknownFeature) {
		t.Fatalf("undeclared report = %+v", undeclared)
	}
}

func TestSpecContextReportsAnUnresolvedDecisionRecordWithoutEchoingIt(t *testing.T) {
	ws := specWorkspace(t)
	root := ws.Root
	writeFixtureFile(t, filepath.Join(root, "app", "specs", "invoice.json"), `{
  "protocolVersion": 1,
  "feature": "billing/invoice-export",
  "outcomes": ["Customers export an issued invoice"],
  "requirements": [],
  "decisions": ["app/doc/adr/0009-deleted.md"]
}`)
	report, err := BuildSpecContextResult(ws, "billing/invoice-export")
	if err != nil {
		t.Fatalf("specs inspect: %v", err)
	}
	if len(report.Decisions) != 1 || report.Decisions[0].Exists {
		t.Fatalf("decisions = %+v, want the link reported as absent", report.Decisions)
	}
	finding, found := findDiagnostic(report.Diagnostics, SpecCodeUnresolvedDecision)
	if !found {
		t.Fatalf("diagnostics = %+v, want an unresolved-decision warning", report.Diagnostics)
	}
	if finding.Severity != diag.Warning {
		t.Fatalf("an absent record must not fail the run: %+v", finding)
	}
	if finding.Field != "app/specs/invoice.json#decisions[0]" {
		t.Fatalf("field = %q, want the exact link position", finding.Field)
	}
	if strings.Contains(finding.Message, "0009-deleted") {
		t.Fatalf("message echoed the linked path: %q", finding.Message)
	}
}

func TestSpecContextReportsAnEscapingDecisionLeafSymlinkAsUnresolved(t *testing.T) {
	ws := specWorkspace(t)
	root := ws.Root
	outsideRecord := filepath.Join(t.TempDir(), "outside.md")
	writeFixtureFile(t, outsideRecord, "# outside")
	link := filepath.Join(root, "app", "doc", "adr", "0008-outside-link.md")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideRecord, link); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	writeFixtureFile(t, filepath.Join(root, "app", "specs", "invoice.json"), `{
  "protocolVersion": 1,
  "feature": "billing/invoice-export",
  "outcomes": ["Customers export an issued invoice"],
  "requirements": [],
  "decisions": ["app/doc/adr/0008-outside-link.md"]
}`)

	report, err := BuildSpecContextResult(ws, "billing/invoice-export")
	if err != nil {
		t.Fatalf("specs inspect: %v", err)
	}
	if len(report.Decisions) != 1 || report.Decisions[0].Exists {
		t.Fatalf("decisions = %+v, want the escaping leaf link reported as absent", report.Decisions)
	}
	if !hasDiagnosticCode(report.Diagnostics, SpecCodeUnresolvedDecision) {
		t.Fatalf("diagnostics = %+v, want an unresolved-decision warning", report.Diagnostics)
	}
}

// TestSpecSelectorsRejectUnboundedInputWithoutEchoingIt keeps an unbounded or
// control-character selector out of both the message and the report.
func TestSpecSelectorsRejectUnboundedInputWithoutEchoingIt(t *testing.T) {
	ws := specWorkspace(t)
	selector := strings.Repeat("a", specSelectorMaxBytes+1)
	for _, run := range []struct {
		name string
		err  error
	}{
		{"inspect", secondValue(BuildSpecContextResult(ws, selector))},
		{"init", secondValue(BuildSpecInitResult(ws, selector, true))},
	} {
		if !errors.Is(run.err, protocolcli.ErrUsage) {
			t.Errorf("%s error = %v, want ErrUsage", run.name, run.err)
		}
		if run.err != nil && strings.Contains(run.err.Error(), selector) {
			t.Errorf("%s echoed the rejected selector", run.name)
		}
	}
}

// TestSpecCatalogDegradesAroundBrokenDocuments keeps discovery usable: a
// document that cannot be read or does not resolve must not hide the healthy
// specs, on the CLI or through list_specs.
func TestSpecCatalogDegradesAroundBrokenDocuments(t *testing.T) {
	ws := specWorkspace(t)
	root := ws.Root
	writeFixtureFile(t, filepath.Join(root, "app", "specs", "healthy.json"), specDocument("billing/invoice-export"))
	writeFixtureFile(t, filepath.Join(root, "specs", "unresolvable.json"), specDocument("billing/never-declared"))
	writeFixtureFile(t, filepath.Join(root, "specs", "unparsable.json"),
		`{"protocolVersion":1,"feature":"billing/refunds","outcomes":["x"],"requirements":[],"status":"approved"}`)

	report, err := BuildSpecCatalogResult(ws, Selection{})
	if err != nil {
		t.Fatalf("a broken document must not fail discovery: %v", err)
	}
	if len(report.Specs) != 2 {
		t.Fatalf("specs = %+v, want the healthy and the parsed-but-unresolvable rows", report.Specs)
	}
	byFeature := map[string]SpecSummary{}
	for _, summary := range report.Specs {
		byFeature[summary.Feature] = summary
	}
	if !byFeature["billing/invoice-export"].Valid {
		t.Errorf("the healthy spec is reported invalid: %+v", byFeature["billing/invoice-export"])
	}
	if byFeature["billing/never-declared"].Valid {
		t.Errorf("an unresolvable spec is reported valid: %+v", byFeature["billing/never-declared"])
	}
	if !hasDiagnosticCode(report.Diagnostics, featureproto.ErrorCodeUnknownField) ||
		!hasDiagnosticCode(report.Diagnostics, featureproto.ErrorCodeUnknownFeature) {
		t.Fatalf("diagnostics = %+v, want both broken documents named", report.Diagnostics)
	}
	// Inspection does not degrade: the caller named one exact feature, and a
	// feature no declaration mints is an answer of "no", not a partial result.
	if _, err := BuildSpecContextResult(ws, "billing/never-declared"); !errors.Is(err, protocolcli.ErrNotFound) {
		t.Fatalf("inspecting an unresolvable feature = %v, want ErrNotFound", err)
	}
}

// TestSpecCatalogBoundsOutcomesPerRow keeps the discovery surface compact: a
// long spec contributes a truncated list plus its honest total.
func TestSpecCatalogBoundsOutcomesPerRow(t *testing.T) {
	ws := specWorkspace(t)
	root := ws.Root
	outcomes := make([]string, 0, specListOutcomeLimit+3)
	for index := 0; index < specListOutcomeLimit+3; index++ {
		outcomes = append(outcomes, `"Outcome `+string(rune('a'+index))+`"`)
	}
	writeFixtureFile(t, filepath.Join(root, "app", "specs", "invoice.json"), `{
  "protocolVersion": 1,
  "feature": "billing/invoice-export",
  "outcomes": [`+strings.Join(outcomes, ",")+`],
  "requirements": []
}`)
	report, err := BuildSpecCatalogResult(ws, Selection{})
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	summary := report.Specs[0]
	if len(summary.Outcomes) != specListOutcomeLimit || summary.OutcomeCount != specListOutcomeLimit+3 {
		t.Fatalf("summary = %+v, want a bounded list with an honest total", summary)
	}
}

func specWorkspace(t *testing.T) *workspace.Workspace {
	t.Helper()
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"specs-test","includes":["app","docs"]}`)
	writeFixtureFile(t, filepath.Join(root, "app", "putnami.json"), `{"name":"billing-app","type":"application","publish":["npm"]}`)
	writeFixtureFile(t, filepath.Join(root, "docs", "putnami.json"), `{"name":"docs-site","type":"application","publish":["npm"]}`)
	writeFixtureFile(t, filepath.Join(root, "app", featureproto.ManifestFilename), `{
  "protocolVersion": 1,
  "namespace": "billing",
  "features": [{
    "id": "billing/invoice-export",
    "type": "feature",
    "name": "Invoice export",
    "outcome": "Customers can export issued invoices",
    "owner": "billing",
    "target": "modeled"
  }, {
    "id": "billing/refunds",
    "type": "feature",
    "name": "Refunds",
    "outcome": "Customers receive refunds",
    "owner": "billing",
    "target": "modeled"
  }]
}`)
	// The two putnami.json files above are what workspace.Load read. An extension
	// is handed this membership on the wire instead.
	return fixtureWorkspace("specs-test", root,
		&workspace.Project{
			ID: "/app", Name: "billing-app", SourceName: "billing-app", Type: "application", Path: "app",
			Publish: []string{"npm"},
			Config:  &wsproto.ProjectConfig{Name: "billing-app", Type: "application", Publish: []string{"npm"}},
		},
		&workspace.Project{
			ID: "/docs", Name: "docs-site", SourceName: "docs-site", Type: "application", Path: "docs",
			Publish: []string{"npm"},
			Config:  &wsproto.ProjectConfig{Name: "docs-site", Type: "application", Publish: []string{"npm"}},
		})
}

func specDocument(feature string) string {
	return `{
  "protocolVersion": 1,
  "feature": "` + feature + `",
  "outcomes": ["An intended outcome"],
  "requirements": []
}`
}

func hasDiagnosticCode(diagnostics []diag.Diagnostic, code string) bool {
	_, found := findDiagnostic(diagnostics, code)
	return found
}

func findDiagnostic(diagnostics []diag.Diagnostic, code string) (diag.Diagnostic, bool) {
	for _, finding := range diagnostics {
		if finding.Code == code {
			return finding, true
		}
	}
	return diag.Diagnostic{}, false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// secondValue keeps the selector table readable: every builder returns
// (report, error) and only the error is asserted there.
func secondValue[T any](_ T, err error) error { return err }
