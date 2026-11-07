package specs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

const specKitSpec = `# Feature Specification: Invoice export

**Feature Branch**: ` + "`001-invoice-export`" + `

## Requirements *(mandatory)*

### Functional Requirements

<!-- putnami:id export-a-month -->
- **FR-001**: System MUST export the invoices of one calendar month as a CSV file.
- **FR-002**: System MUST refuse a month that has not closed.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An accountant exports a closed month in under 30 seconds.
`

const openSpecSpec = `# ci-cd Specification

## Purpose

Run the integration tests fast.

## Requirements

### Requirement: Parallel Test Execution

The CI system SHALL execute integration tests in parallel.

#### Scenario: Pull Request

- **WHEN** a pull request is opened
- **THEN** the CI system SHALL spawn parallel jobs
`

const kiroSpec = `# Requirements Document: CLI Application

## Introduction

The CLI lets developers reach the platform from their terminal.

## Requirements

### Requirement 1: Authentication

**User Story:** As a developer, I want to authenticate via CLI, so that I can access my account.

#### Acceptance Criteria

1. WHEN running ` + "`sia login`" + ` THEN the CLI SHALL open a browser for authentication
`

const gherkinSpec = `Feature: Export audit
  Scenario: An export leaves an audit entry
    When the accountant exports March
    Then the audit log names the accountant
`

func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func byFormat(found []Spec) map[string]Spec {
	out := map[string]Spec{}
	for _, s := range found {
		out[s.Format] = s
	}
	return out
}

func TestCollect_ReportsASpecKitRequirement(t *testing.T) {
	spectest.Proves(t, "tooling/observe", "external-specs", "observe-reports-a-spec-kit-requirement")
	root := writeRepo(t, map[string]string{
		"packages/billing/package.json":                     "{}",
		"packages/billing/specs/001-invoice-export/spec.md": specKitSpec,
		"packages/billing/specs/001-invoice-export/plan.md": "# Plan\n",
	})
	owner := func(path string) (string, bool) {
		if strings.HasPrefix(path, "packages/billing/") {
			return "billing", true
		}
		return "", false
	}
	found, diags := Collect(Request{Root: root, Owner: owner})
	if len(found) != 1 || len(diags) != 0 {
		t.Fatalf("found %+v, diagnostics %v", found, diags)
	}
	spec := found[0]
	if spec.Format != "speckit" || spec.Path != "packages/billing/specs/001-invoice-export" ||
		spec.File != "packages/billing/specs/001-invoice-export/spec.md" || spec.Component != "billing" || spec.Changed {
		t.Fatalf("spec = %+v", spec)
	}
	if len(spec.Requirements) != 2 || spec.Requirements[0].ID != "export-a-month" || spec.Requirements[1].ID != "fr-002" ||
		!strings.Contains(spec.Requirements[1].Text, "refuse a month") || spec.Requirements[0].Line == 0 {
		t.Fatalf("requirements = %+v", spec.Requirements)
	}
}

func TestCollect_ReportsEachExternalFormat(t *testing.T) {
	spectest.Proves(t, "tooling/observe", "external-specs", "observe-reports-each-external-format")
	root := writeRepo(t, map[string]string{
		"specs/001-invoice-export/spec.md":                            specKitSpec,
		"openspec/specs/ci-cd/spec.md":                                openSpecSpec,
		"openspec/changes/next/specs/ci-cd/spec.md":                   openSpecSpec,
		"openspec/changes/archive/2026-01-01-old/specs/ci-cd/spec.md": openSpecSpec,
		"packages/api/openspec/changes/next/specs/ci-cd/spec.md":      openSpecSpec,
		".kiro/specs/cli/requirements.md":                             kiroSpec,
		"features/audit.feature":                                      gherkinSpec,
		"node_modules/dep/features/x.feature":                         gherkinSpec,
		"docs/spec.md":                                                "# Not a spec\n",
	})
	found, diags := Collect(Request{Root: root})
	formats := byFormat(found)
	want := map[string]string{
		"speckit":  "specs/001-invoice-export",
		"openspec": "openspec/specs/ci-cd",
		"kiro":     ".kiro/specs/cli",
		"gherkin":  "features/audit.feature",
	}
	for format, path := range want {
		if formats[format].Path != path || len(formats[format].Requirements) == 0 {
			t.Errorf("%s: %+v, want a spec at %s with requirements", format, formats[format], path)
		}
	}
	// A change in flight or archived under openspec/changes is not a current
	// spec: observe neither reports it nor warns about it.
	if len(found) != 4 {
		t.Errorf("found %d specs, want 4: %+v", len(found), found)
	}
	if len(diags) != 0 {
		t.Errorf("diagnostics = %v", diags)
	}
}

func TestCollect_MarksAChangedSpec(t *testing.T) {
	spectest.Proves(t, "tooling/observe", "external-specs", "observe-marks-a-changed-spec")
	root := writeRepo(t, map[string]string{
		"specs/001-invoice-export/spec.md": specKitSpec,
		"specs/002-refunds/spec.md":        strings.ReplaceAll(specKitSpec, "Invoice export", "Refunds"),
	})
	found, _ := Collect(Request{Root: root, Changed: []string{"specs/002-refunds/spec.md", "src/main.go"}})
	if len(found) != 2 || found[0].Changed || !found[1].Changed || found[1].Path != "specs/002-refunds" {
		t.Fatalf("found = %+v", found)
	}
}

func TestCollect_AnUnreadableSpecIsADiagnostic(t *testing.T) {
	spectest.Proves(t, "tooling/observe", "external-specs", "an-unreadable-spec-is-a-diagnostic")
	root := writeRepo(t, map[string]string{
		"specs/001-invoice-export/spec.md": specKitSpec,
		"specs/002-broken/spec.md":         "# Feature Specification: Broken\n\n<!-- never closed\n",
	})
	found, diags := Collect(Request{Root: root})
	if len(found) != 1 || found[0].Path != "specs/001-invoice-export" {
		t.Fatalf("found = %+v", found)
	}
	if len(diags) != 1 || diags[0].Code != CodeUnreadable || diags[0].Severity != diag.Warning || diags[0].Field != "specs/002-broken" {
		t.Fatalf("diagnostics = %v", diags)
	}
}
