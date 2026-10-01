package sdd

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

// docsWorkspace is a workspace whose root README links into a scope doc tree,
// which links into the project at tooling/lib, whose README links back.
func docsWorkspace(t *testing.T) string {
	t.Helper()
	root := decisionsWorkspace(t)
	writeFixtureFile(t, filepath.Join(root, "README.md"), "# W\n\nSee [the guide](tooling/doc/guide.md#set-up).\n")
	writeFixtureFile(t, filepath.Join(root, "tooling", "putnami.json"), "{}")
	writeFixtureFile(t, filepath.Join(root, "tooling", "doc", "guide.md"), "# Guide\n\n## Set up\n\n[lib](../lib/README.md#usage)\n")
	writeFixtureFile(t, filepath.Join(root, "tooling", "lib", "putnami.json"), "{}")
	writeFixtureFile(t, filepath.Join(root, "tooling", "lib", "README.md"), "# Lib\n\n## Usage\n\n[the guide](../doc/guide.md)\n")
	return root
}

// TestDocsLinksPassesAWorkspaceWithWorkingLinks keeps the gate from being
// vacuously red.
func TestDocsLinksPassesAWorkspaceWithWorkingLinks(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "every-document-has-working-links", "working-links-pass")
	root := docsWorkspace(t)
	report, findings, err := BuildDocsLinksResult(fixtureWorkspace("w", root, appProject("lib", "tooling/lib")), nil)
	if err != nil {
		t.Fatalf("working links failed the task: %v (%+v)", err, report.Findings)
	}
	if report.Documents != 3 || len(report.Findings) != 0 || len(findings) != 0 {
		t.Fatalf("report = %+v, want three documents and no finding", report)
	}
}

// TestDocsLinksFailsNamingTheBrokenLink is the acceptance line for
// a document no project owns.
func TestDocsLinksFailsNamingTheBrokenLink(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "every-document-has-working-links", "a-broken-link-fails-naming-it")
	root := docsWorkspace(t)
	writeFixtureFile(t, filepath.Join(root, "tooling", "doc", "guide.md"), "# Guide\n\n[lib](../lib/README.md)\n")
	report, findings, err := BuildDocsLinksResult(fixtureWorkspace("w", root, appProject("lib", "tooling/lib")), nil)
	if err == nil {
		t.Fatal("a broken anchor passed the task")
	}
	if !errors.Is(err, protocolcli.ErrInvalidConfig) || ResultData(err) == nil {
		t.Fatalf("err = %v, want an invalid-configuration verdict carrying the report", err)
	}
	if len(findings) != 1 || len(report.Findings) != 1 {
		t.Fatalf("report = %+v, want one finding", report)
	}
	finding := report.Findings[0]
	if finding.File != "README.md" || finding.Line != 3 || finding.Target != "tooling/doc/guide.md#set-up" ||
		!strings.Contains(finding.Message, "names no heading of tooling/doc/guide.md") {
		t.Fatalf("finding = %+v", finding)
	}
}

// TestDocsLinksChecksALinkIntoAnotherProject asserts the gate reads project
// documents too: a change to one project that breaks a link another project's
// README holds fails here, although no lint task of the other project runs.
func TestDocsLinksChecksALinkIntoAnotherProject(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "every-document-has-working-links", "a-link-into-another-project-is-checked")
	root := docsWorkspace(t)
	writeFixtureFile(t, filepath.Join(root, "tooling", "lib", "README.md"), "# Lib\n\n## Usage\n\n[the spec](../app/specs/app.json)\n")
	report, _, err := BuildDocsLinksResult(fixtureWorkspace("w", root, appProject("lib", "tooling/lib")), nil)
	if err == nil || len(report.Findings) != 1 || report.Findings[0].File != "tooling/lib/README.md" {
		t.Fatalf("report = %+v, err = %v, want the project README's broken link", report, err)
	}
}

// TestDocsLinksHonorsAProjectOptOut asserts the lint flag that turns the
// check off for a project turns this gate off for it too, and that a
// project's own value overrides the workspace's.
func TestDocsLinksHonorsAProjectOptOut(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "every-document-has-working-links", "a-project-opt-out-is-honored")
	root := docsWorkspace(t)
	writeFixtureFile(t, filepath.Join(root, "go", "extension", "putnami.extension.json"), `{"name": "@putnami/go"}`)
	writeFixtureFile(t, filepath.Join(root, "tooling", "lib", "README.md"), "# Lib\n\n## Usage\n\n[gone](gone.md)\n")
	writeFixtureFile(t, filepath.Join(root, "tooling", "lib", "putnami.json"), `{"options": {"lint": {"docs-links": false}}}`)
	lib := appProject("lib", "tooling/lib")
	lib.Extensions = []string{"/go/extension"}
	ws := fixtureWorkspace("w", root, lib)
	report, _, err := BuildDocsLinksResult(ws, nil)
	if err != nil || report.Documents != 2 {
		t.Fatalf("report = %+v, err = %v, want the opted-out project skipped", report, err)
	}

	// The project's own value wins over the workspace's, under the key of its
	// extension's lint.
	writeFixtureFile(t, filepath.Join(root, "tooling", "lib", "putnami.json"), `{"options": {"@putnami/go:lint": {"docs-links": true}}}`)
	report, _, err = BuildDocsLinksResult(ws, map[string]map[string]any{"lint": {"docs-links": false}})
	if err == nil || len(report.Findings) != 1 || report.Findings[0].File != "tooling/lib/README.md" {
		t.Fatalf("report = %+v, err = %v, want the project's own true to win over the workspace's false", report, err)
	}

	// A block keyed by the extension's name, which the project reaches
	// through the path it wrote.
	writeFixtureFile(t, filepath.Join(root, "tooling", "lib", "putnami.json"), `{"options": {"@putnami/go": {"docs-links": false}}}`)
	report, _, err = BuildDocsLinksResult(ws, nil)
	if err != nil || report.Documents != 2 {
		t.Fatalf("report = %+v, err = %v, want an extension-keyed opt-out honored", report, err)
	}

	// The CLI reads a block keyed by the extension's path for file inputs
	// only, never for parameters, and neither does this gate.
	writeFixtureFile(t, filepath.Join(root, "tooling", "lib", "putnami.json"), `{"options": {"/go/extension": {"docs-links": false}}}`)
	report, _, err = BuildDocsLinksResult(ws, nil)
	if err == nil || len(report.Findings) != 1 {
		t.Fatalf("report = %+v, err = %v, want a path-keyed block ignored", report, err)
	}

	// The CLI reads no `*` block from a project's options, and neither does
	// this gate.
	writeFixtureFile(t, filepath.Join(root, "tooling", "lib", "putnami.json"), `{"options": {"*": {"docs-links": false}}}`)
	report, _, err = BuildDocsLinksResult(ws, nil)
	if err == nil || len(report.Findings) != 1 {
		t.Fatalf("report = %+v, err = %v, want a project `*` block ignored", report, err)
	}

	// A workspace block keyed by one extension reaches only that extension's
	// projects.
	writeFixtureFile(t, filepath.Join(root, "tooling", "lib", "putnami.json"), "{}")
	goOff := map[string]map[string]any{"@putnami/go": {"docs-links": false}}
	if report, _, err = BuildDocsLinksResult(ws, goOff); err != nil || report.Documents != 2 {
		t.Fatalf("report = %+v, err = %v, want the Go project skipped by a Go-keyed block", report, err)
	}
	lib.Extensions = []string{"@putnami/typescript"}
	report, _, err = BuildDocsLinksResult(fixtureWorkspace("w", root, lib), goOff)
	if err == nil || len(report.Findings) != 1 || report.Findings[0].File != "tooling/lib/README.md" {
		t.Fatalf("report = %+v, err = %v, want a TypeScript project checked despite a Go-keyed block", report, err)
	}
}

// TestDocsLinksChecksEveryDocumentOnce asserts no document falls between the
// projects: a nested Go module that is not a project belongs to the project
// around it, and a root project's scopes are its own.
func TestDocsLinksChecksEveryDocumentOnce(t *testing.T) {
	root := docsWorkspace(t)
	writeFixtureFile(t, filepath.Join(root, "tooling", "lib", "tool", "go.mod"), "module tool\n")
	writeFixtureFile(t, filepath.Join(root, "tooling", "lib", "tool", "README.md"), "[gone](gone.md)\n")
	report, _, err := BuildDocsLinksResult(fixtureWorkspace("w", root, appProject("lib", "tooling/lib")), nil)
	if err == nil || len(report.Findings) != 1 || report.Findings[0].File != "tooling/lib/tool/README.md" {
		t.Fatalf("report = %+v, err = %v, want the nested module's broken link", report, err)
	}

	writeFixtureFile(t, filepath.Join(root, "tooling", "lib", "tool", "README.md"), "# Tool\n")
	writeFixtureFile(t, filepath.Join(root, "tooling", "doc", "guide.md"), "# Guide\n\n## Set up\n\n[gone](gone.md)\n")
	ws := fixtureWorkspace("w", root, appProject("root", "."), appProject("lib", "tooling/lib"))
	report, _, err = BuildDocsLinksResult(ws, nil)
	if err == nil || len(report.Findings) != 1 || report.Findings[0].File != "tooling/doc/guide.md" {
		t.Fatalf("report = %+v, err = %v, want the scope's broken link under a root project", report, err)
	}

	writeFixtureFile(t, filepath.Join(root, "putnami.json"), `{"options": {"lint": {"docs-links": false}}}`)
	report, _, err = BuildDocsLinksResult(ws, nil)
	if err != nil || report.Documents != 2 {
		t.Fatalf("report = %+v, err = %v, want a root project's opt-out to cover the scope", report, err)
	}
}

// TestDocsLinksReadsAProjectAsItsLintDoes asserts a project under a doc
// directory is read by the per-project rule: its CHANGELOG.md is not a
// document, as it is not one to its lint-docs task.
func TestDocsLinksReadsAProjectAsItsLintDoes(t *testing.T) {
	root := docsWorkspace(t)
	writeFixtureFile(t, filepath.Join(root, "site", "doc", "handbook", "putnami.json"), "{}")
	writeFixtureFile(t, filepath.Join(root, "site", "doc", "handbook", "README.md"), "# Handbook\n")
	writeFixtureFile(t, filepath.Join(root, "site", "doc", "handbook", "CHANGELOG.md"), "[gone](gone.md)\n")
	ws := fixtureWorkspace("w", root, appProject("lib", "tooling/lib"), appProject("handbook", "site/doc/handbook"))
	report, _, err := BuildDocsLinksResult(ws, nil)
	if err != nil || report.Documents != 4 {
		t.Fatalf("report = %+v, err = %v, want four documents and no finding", report, err)
	}
}
