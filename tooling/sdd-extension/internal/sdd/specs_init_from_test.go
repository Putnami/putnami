package sdd

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

const (
	specKitFromPath = "app/specs-kit/001-invoice-export"
	specKitFromText = `# Feature Specification: Invoice export

## Requirements *(mandatory)*

### Functional Requirements

{anchor}- **FR-001**: System MUST export the invoices of one calendar month as a CSV
  file with one row per invoice.

## Success Criteria *(mandatory)*

- **SC-001**: An accountant exports a closed month in under 30 seconds.

## Out of Scope

- Exporting to a spreadsheet format other than CSV.
`
	specKitFromManifest = `{
  "protocolVersion": 3,
  "namespace": "billing",
  "features": [{
    "id": "billing/invoicing",
    "type": "feature",
    "name": "Invoicing",
    "outcome": "Customers receive invoices",
    "owner": "billing",
    "target": "coded",
    "spec": {"format": "speckit", "path": "` + specKitFromPath + `"},
    "requirements": [
      {"id": "export-a-month", "stage": "coded", "evidenceKinds": ["attestation"],
       "verification": {"kind": "acceptance", "checks": ["a-month-exports"]}}
    ]
  }]
}`
)

// specKitFromWorkspace is one project in report mode whose feature names a
// Spec Kit spec. anchor is written above FR-001, and text replaces its sentence.
func specKitFromWorkspace(t *testing.T, anchor, text string) *workspace.Workspace {
	t.Helper()
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"speckit-test","includes":["app"]}`)
	writeFixtureFile(t, filepath.Join(root, "app", "putnami.json"), `{"name":"billing-app","type":"application"}`)
	writeFixtureFile(t, filepath.Join(root, "app", featureproto.ManifestFilename), specKitFromManifest)
	markdown := strings.Replace(specKitFromText, "{anchor}", anchor, 1)
	if text != "" {
		markdown = strings.Replace(markdown,
			"System MUST export the invoices of one calendar month as a CSV\n  file with one row per invoice.", text, 1)
	}
	writeFixtureFile(t, filepath.Join(root, filepath.FromSlash(specKitFromPath), "spec.md"), markdown)
	project := appProject("billing-app", "app")
	project.Config = &wsproto.ProjectConfig{Name: "billing-app", Type: "application", Options: map[string]map[string]any{
		"sdd": {"specFormats": []any{"speckit"}},
	}}
	return fixtureWorkspace("speckit-test", root, project)
}

func readFixture(t *testing.T, ws *workspace.Workspace, relative string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(ws.Root, filepath.FromSlash(relative)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSpecsInitFromWritesTheTextTheSourceStates(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "init-from-copies-the-external-spec",
		"init-from-writes-the-text-the-source-states")
	for _, scenario := range []struct {
		name, from, source string
		ws                 func(*testing.T) *workspace.Workspace
		outcomes           []string
		requirements       []featureproto.SpecRequirement
		nonGoals           []string
		decisions          []string
	}{
		{
			name: "openspec", from: "openspec:app/openspec/specs/invoice-export/",
			source: "app/openspec/specs/invoice-export/spec.md",
			ws: func(t *testing.T) *workspace.Workspace {
				ws := openSpecWorkspace(t)
				writeOpenSpec(t, ws.Root, "", "### Requirement: Issue an invoice")
				specPath := filepath.Join(ws.Root, "app", "openspec", "specs", "invoice-export", "spec.md")
				writeFixtureFile(t, specPath, strings.Replace(readFixture(t, ws, "app/openspec/specs/invoice-export/spec.md"),
					"Customers receive invoices.\n",
					"Customers receive invoices, as [the invoice format](../../../doc/adr/0001-invoice-format.md) decides.\n", 1))
				writeFixtureFile(t, filepath.Join(ws.Root, "app", "doc", "adr", "0001-invoice-format.md"), "# ADR 0001\n")
				return ws
			},
			outcomes:     []string{"Customers receive invoices, as [the invoice format](../../../doc/adr/0001-invoice-format.md) decides."},
			requirements: []featureproto.SpecRequirement{{ID: "issue-an-invoice", Text: featureproto.SpecText{"The system SHALL issue an invoice."}}},
			decisions:    []string{"app/doc/adr/0001-invoice-format.md"},
		},
		{
			name: "kiro", from: "kiro:.kiro/specs/invoice-export", source: kiroRequirementsPath,
			ws:       func(t *testing.T) *workspace.Workspace { return kiroWorkspace(t, "report", kiroAnchors) },
			outcomes: []string{"Customers receive invoices."},
			requirements: []featureproto.SpecRequirement{
				{ID: "issue-an-invoice", Text: featureproto.SpecText{"WHEN an order closes THE system SHALL issue an invoice"}},
				{ID: "number-invoices", Text: featureproto.SpecText{"THE system SHALL number invoices"}},
			},
		},
		{
			name: "speckit", from: "speckit:" + specKitFromPath, source: specKitFromPath + "/spec.md",
			ws: func(t *testing.T) *workspace.Workspace {
				return specKitFromWorkspace(t, "<!-- putnami:id export-a-month -->\n", "")
			},
			outcomes: []string{"An accountant exports a closed month in under 30 seconds."},
			requirements: []featureproto.SpecRequirement{{ID: "export-a-month",
				Text: featureproto.SpecText{"System MUST export the invoices of one calendar month as a CSV file with one row per invoice."}}},
			nonGoals: []string{"Exporting to a spreadsheet format other than CSV."},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ws := scenario.ws(t)
			manifest := readFixture(t, ws, "app/"+featureproto.ManifestFilename)
			source := readFixture(t, ws, scenario.source)

			dryRun, err := BuildSpecInitResult(ws, "billing/invoicing", SpecInitOptions{DryRun: true, From: scenario.from})
			if err != nil {
				t.Fatalf("dry run: %v, %+v", err, dryRun.Diagnostics)
			}
			if dryRun.Created || dryRun.Path != "app/specs/invoicing.json" || dryRun.From == nil {
				t.Fatalf("dry run report = %+v", dryRun)
			}
			if _, statErr := os.Stat(filepath.Join(ws.Root, "app", "specs", "invoicing.json")); !os.IsNotExist(statErr) {
				t.Fatal("--dry-run wrote the spec")
			}

			created, err := BuildSpecInitResult(ws, "billing/invoicing", SpecInitOptions{From: scenario.from})
			if err != nil || !created.Created || created.Contents != dryRun.Contents {
				t.Fatalf("create: %v, %+v, want the dry-run bytes", err, created)
			}
			written := readFixture(t, ws, created.Path)
			if written != created.Contents {
				t.Fatalf("written bytes differ from the report:\n%s", written)
			}
			parsed, findings := featureproto.ParseAndValidateSpec([]byte(written))
			if parsed == nil || len(findings) != 0 {
				t.Fatalf("drafted spec is not valid: %+v", findings)
			}
			if canonical, _ := featureproto.MarshalSpec(parsed); string(canonical) != written {
				t.Fatalf("drafted bytes are not canonical:\n%s", written)
			}
			if parsed.Feature != "billing/invoicing" || !reflect.DeepEqual(parsed.Outcomes, scenario.outcomes) ||
				!reflect.DeepEqual(parsed.Requirements, scenario.requirements) ||
				!reflect.DeepEqual(parsed.NonGoals, scenario.nonGoals) || !reflect.DeepEqual(parsed.Decisions, scenario.decisions) {
				t.Fatalf("drafted spec = %+v, want only the source's text", parsed)
			}

			// The source and the manifest stay as they were, so the half-done
			// migration fails specs validate until the author removes both.
			if readFixture(t, ws, "app/"+featureproto.ManifestFilename) != manifest || readFixture(t, ws, scenario.source) != source {
				t.Fatal("specs init --from edited the manifest or the external spec")
			}
			report, _ := BuildSpecValidationResult(ws, Selection{})
			if !hasDiagnosticCode(report.Diagnostics, featureproto.ErrorCodeDuplicateSpec) {
				t.Fatalf("specs validate = %+v, want the duplicate spec", report.Diagnostics)
			}
		})
	}
}

func TestSpecsInitFromRefusesAtTheSourcePosition(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "init-from-copies-the-external-spec",
		"init-from-refuses-at-the-source-position")
	source := specKitFromPath + "/spec.md"
	for _, scenario := range []struct {
		name     string
		ws       func(*testing.T) *workspace.Workspace
		feature  string
		from     string
		wantErr  error
		code     string
		field    string
		mentions string
	}{
		{
			name:    "an unanchored positional requirement",
			ws:      func(t *testing.T) *workspace.Workspace { return specKitFromWorkspace(t, "", "") },
			feature: "billing/invoicing", from: "speckit:" + specKitFromPath, wantErr: protocolcli.ErrInvalidConfig,
			code: featureproto.ErrorCodeSpecRequirementUnanchored, field: source + "#L7", mentions: "fr-001",
		},
		{
			name: "a text over the bound",
			ws: func(t *testing.T) *workspace.Workspace {
				return specKitFromWorkspace(t, "<!-- putnami:id export-a-month -->\n", "System MUST "+strings.Repeat("export ", 80))
			},
			feature: "billing/invoicing", from: "speckit:" + specKitFromPath, wantErr: protocolcli.ErrInvalidConfig,
			code: featureproto.FindingCodeSpecTextTooLong, field: source + "#L8", mentions: "split it",
		},
		{
			name:    "a --from that is not the feature's reference",
			ws:      openSpecWorkspace,
			feature: "billing/invoicing", from: "openspec:app/openspec/specs/other", wantErr: protocolcli.ErrUsage,
			code: SpecCodeInitFromMismatch, field: "app/putnami.features.json#features[0].spec",
			mentions: "openspec:app/openspec/specs/invoice-export",
		},
		{
			name:    "an undeclared feature",
			ws:      openSpecWorkspace,
			feature: "billing/unknown", from: "openspec:app/openspec/specs/invoice-export", wantErr: protocolcli.ErrNotFound,
			code: featureproto.ErrorCodeUnknownFeature, field: "feature", mentions: "billing/unknown",
		},
		{
			name: "a feature with a native spec",
			ws: func(t *testing.T) *workspace.Workspace {
				ws := openSpecWorkspace(t)
				writeFixtureFile(t, filepath.Join(ws.Root, "app", "specs", "invoicing.json"), `{
  "protocolVersion": 1,
  "feature": "billing/invoicing",
  "outcomes": ["Customers receive invoices"],
  "requirements": []
}
`)
				return ws
			},
			feature: "billing/invoicing", from: "openspec:app/openspec/specs/invoice-export", wantErr: protocolcli.ErrUsage,
			code: featureproto.ErrorCodeDuplicateSpec, field: "app/specs/invoicing.json", mentions: "native spec",
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ws := scenario.ws(t)
			report, err := BuildSpecInitResult(ws, scenario.feature, SpecInitOptions{From: scenario.from})
			if !errors.Is(err, scenario.wantErr) {
				t.Fatalf("error = %v, want %v; diagnostics %+v", err, scenario.wantErr, report.Diagnostics)
			}
			if report.Created {
				t.Fatalf("a refused init created %s", report.Path)
			}
			found := false
			for _, finding := range report.Diagnostics {
				if finding.Code == scenario.code && finding.Severity == diag.Error && finding.Field == scenario.field &&
					strings.Contains(finding.Message, scenario.mentions) {
					found = true
				}
			}
			if !found {
				t.Fatalf("diagnostics = %+v, want %s at %s naming %q", report.Diagnostics, scenario.code, scenario.field, scenario.mentions)
			}
		})
	}

	_, err := BuildSpecInitResult(openSpecWorkspace(t), "billing/invoicing", SpecInitOptions{From: "markdown:app/openspec"})
	if !errors.Is(err, protocolcli.ErrUsage) || !strings.Contains(err.Error(), "openspec") || strings.Contains(err.Error(), "markdown") {
		t.Fatalf("an unknown format = %v, want a usage error that lists the formats and does not echo the input", err)
	}
	long := "openspec:" + strings.Repeat("a", specInitFromMaxBytes)
	if _, err := BuildSpecInitResult(openSpecWorkspace(t), "billing/invoicing", SpecInitOptions{From: long}); !errors.Is(err, protocolcli.ErrUsage) ||
		strings.Contains(err.Error(), "aaaa") {
		t.Fatalf("an unbounded --from = %v, want a usage error that does not echo it", err)
	}
	if _, err := BuildSpecInitResult(openSpecWorkspace(t), "billing/invoicing", SpecInitOptions{From: "openspec"}); !errors.Is(err, protocolcli.ErrUsage) {
		t.Fatalf("a --from with no path = %v, want a usage error", err)
	}

	// Without --from, a feature with an external spec is pointed at --from.
	report, err := BuildSpecInitResult(openSpecWorkspace(t), "billing/invoicing", SpecInitOptions{})
	if !errors.Is(err, protocolcli.ErrUsage) ||
		!hasDiagnostic(report.Diagnostics, featureproto.ErrorCodeDuplicateSpec, "--from openspec:app/openspec/specs/invoice-export") {
		t.Fatalf("plain init on an external spec = %v, %+v, want a pointer to --from", err, report.Diagnostics)
	}

	// The same holds when the discovery could not read the reference: a format
	// the project did not opt into still makes the feature specified.
	unread := openSpecWorkspace(t)
	unread.Projects[0].Config.Options["sdd"]["specFormats"] = []any{}
	report, err = BuildSpecInitResult(unread, "billing/invoicing", SpecInitOptions{})
	if !errors.Is(err, protocolcli.ErrUsage) || report.Created {
		t.Fatalf("plain init on an unread external spec = %v, %+v, want a refusal", err, report)
	}
	if _, statErr := os.Stat(filepath.Join(unread.Root, "app", "specs", "invoicing.json")); !os.IsNotExist(statErr) {
		t.Fatal("plain init wrote a skeleton beside an unread external spec")
	}
}

func TestSpecsInitFromKeepsTheSpecsVerifyVerdict(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "init-from-copies-the-external-spec",
		"init-from-keeps-the-specs-verify-verdict")
	ws := openSpecWorkspace(t)
	before, err := BuildSpecVerifyResult(ws, Selection{}, "")
	if err != nil && len(before.Groups) == 0 {
		t.Fatalf("verify the external spec: %v", err)
	}
	if _, err := BuildSpecInitResult(ws, "billing/invoicing", SpecInitOptions{From: "openspec:app/openspec/specs/invoice-export"}); err != nil {
		t.Fatalf("init --from: %v", err)
	}
	// The author finishes the migration: the reference and the external file go.
	manifest := readFixture(t, ws, "app/"+featureproto.ManifestFilename)
	writeFixtureFile(t, filepath.Join(ws.Root, "app", featureproto.ManifestFilename),
		strings.Replace(manifest, `    "spec": {"format": "openspec", "path": "app/openspec/specs/invoice-export"},`+"\n", "", 1))
	if err := os.RemoveAll(filepath.Join(ws.Root, "app", "openspec")); err != nil {
		t.Fatal(err)
	}
	if report, err := BuildSpecValidationResult(ws, Selection{}); err != nil {
		t.Fatalf("the finished migration fails specs validate: %v, %+v", err, report.Diagnostics)
	}
	after, err := BuildSpecVerifyResult(ws, Selection{}, "")
	if err != nil && len(after.Groups) == 0 {
		t.Fatalf("verify the native spec: %v", err)
	}
	if len(before.Groups) != 1 || len(after.Groups) != 1 {
		t.Fatalf("groups = %+v and %+v, want one each", before.Groups, after.Groups)
	}
	if after.Groups[0].Spec != "app/specs/invoicing.json" || before.Groups[0].Spec == after.Groups[0].Spec {
		t.Fatalf("the verdict did not move to the native spec: %s, then %s", before.Groups[0].Spec, after.Groups[0].Spec)
	}
	if !reflect.DeepEqual(before.Groups[0].Requirements, after.Groups[0].Requirements) ||
		before.Groups[0].Counts != after.Groups[0].Counts || before.Groups[0].Blocked != after.Groups[0].Blocked ||
		!reflect.DeepEqual(before.Blocked, after.Blocked) {
		t.Fatalf("the verdict changed:\nbefore %+v\nafter  %+v", before.Groups[0], after.Groups[0])
	}
}
