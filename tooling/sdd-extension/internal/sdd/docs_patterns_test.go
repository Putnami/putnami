package sdd

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

// docsPatterns is the options block of a workspace that lists its documents.
func docsPatterns(patterns ...any) map[string]map[string]any {
	return map[string]map[string]any{
		"evidence": {"documents": []any{map[string]any{"type": "docs", "patterns": patterns}}},
	}
}

// TestConfiguredDocsPatternsSelectTheDocuments pins that a workspace's docs
// entries are the whole selection: the check reads the files they select, a
// file they leave out is not read even when it breaks a link, and the lint
// docs-links option no longer skips a project.
func TestConfiguredDocsPatternsSelectTheDocuments(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "configured-docs-patterns-select-the-documents", "the-patterns-select-the-documents")
	root := docsWorkspace(t)
	writeFixtureFile(t, filepath.Join(root, "tooling", "notes.md"), "# Notes\n\n[gone](gone.md)\n")
	ws := fixtureWorkspace("w", root, appProject("lib", "tooling/lib"))

	report, _, err := BuildDocsLinksResult(ws, docsPatterns("**/README.md", "**/doc/**/*.md"))
	if err != nil || report.Documents != 3 {
		t.Fatalf("report = %+v, err = %v, want the three documents the patterns select", report, err)
	}

	// The broken note is read once a pattern selects it.
	report, _, err = BuildDocsLinksResult(ws, docsPatterns("**/README.md", "**/doc/**/*.md", "tooling/notes.md"))
	if err == nil || len(report.Findings) != 1 || report.Findings[0].File != "tooling/notes.md" {
		t.Fatalf("report = %+v, err = %v, want the selected note's broken link", report, err)
	}

	// An exclusion removes a document from the selection.
	report, _, err = BuildDocsLinksResult(ws, docsPatterns("**/*.md", "!tooling/notes.md"))
	if err != nil || report.Documents != 3 {
		t.Fatalf("report = %+v, err = %v, want the excluded note left out", report, err)
	}

	// A lint opt-out skips no project once patterns are configured.
	writeFixtureFile(t, filepath.Join(root, "tooling", "lib", "README.md"), "# Lib\n\n## Usage\n\n[gone](gone.md)\n")
	writeFixtureFile(t, filepath.Join(root, "tooling", "lib", "putnami.json"), `{"options": {"lint": {"docs-links": false}}}`)
	options := docsPatterns("**/README.md", "**/doc/**/*.md")
	options["lint"] = map[string]any{"docs-links": false}
	report, _, err = BuildDocsLinksResult(ws, options)
	if err == nil || len(report.Findings) != 1 || report.Findings[0].File != "tooling/lib/README.md" {
		t.Fatalf("report = %+v, err = %v, want the opted-out project's document checked", report, err)
	}
}

// TestAnInvalidDocumentsEntryFails pins that a configuration the check cannot
// read fails the step, naming the entry, instead of reading as none.
func TestAnInvalidDocumentsEntryFails(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "configured-docs-patterns-select-the-documents", "an-invalid-entry-fails")
	root := docsWorkspace(t)
	ws := fixtureWorkspace("w", root, appProject("lib", "tooling/lib"))
	cases := map[string]struct {
		documents any
		want      string
	}{
		"not a list":        {documents: "README.md", want: "must be a list"},
		"unknown type":      {documents: []any{map[string]any{"type": "notes", "patterns": []any{"**/*.md"}}}, want: `options.evidence.documents[0] has type "notes"`},
		"no include":        {documents: []any{map[string]any{"type": "docs", "patterns": []any{"!vendor/**"}}}, want: "holds no include pattern"},
		"unknown members":   {documents: []any{map[string]any{"type": "docs", "patterns": []any{"**/README.md"}, "zeta": 1, "alpha": 1}}, want: `holds "alpha"`},
		"empty pattern":     {documents: []any{map[string]any{"type": "docs", "patterns": []any{""}}}, want: "patterns[0] must be a non-empty file pattern"},
		"not an object":     {documents: []any{"docs"}, want: "documents[0] must be an object"},
		"patterns string":   {documents: []any{map[string]any{"type": "docs", "patterns": "**/README.md"}}, want: "patterns must be a list"},
		"empty format":      {documents: []any{map[string]any{"type": "docs", "format": "", "patterns": []any{"**/README.md"}}}, want: "format must be a non-empty format name"},
		"selects no file":   {documents: []any{map[string]any{"type": "docs", "patterns": []any{"**/docs/**/*.md"}}}, want: "options.evidence.documents[0].patterns select no file"},
		"invalid other one": {documents: []any{map[string]any{"type": "docs", "patterns": []any{"**/README.md"}}, map[string]any{"type": "spec"}}, want: "options.evidence.documents[1].patterns must be a list"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := BuildDocsLinksResult(ws, map[string]map[string]any{"evidence": {"documents": test.documents}})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want it to name %q", err, test.want)
			}
			if !errors.Is(err, protocolcli.ErrInvalidConfig) {
				t.Fatalf("err = %v, want an invalid configuration", err)
			}
		})
	}
}

// TestDocumentsWithoutADocsEntryKeepTheDefaultDocuments pins that only docs
// entries name the link check's documents: an empty list, or entries of the
// other document types with a format, leave the default documents and the
// project opt-outs in place.
func TestDocumentsWithoutADocsEntryKeepTheDefaultDocuments(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "configured-docs-patterns-select-the-documents", "the-patterns-select-the-documents")
	root := docsWorkspace(t)
	writeFixtureFile(t, filepath.Join(root, "tooling", "notes.md"), "# Notes\n\n[gone](gone.md)\n")
	ws := fixtureWorkspace("w", root, appProject("lib", "tooling/lib"))
	for name, documents := range map[string]any{
		"empty list": []any{},
		"other types": []any{
			map[string]any{"type": "decision", "format": "madr", "patterns": []any{"**/doc/adr/*.md"}},
			map[string]any{"type": "spec", "format": "speckit", "patterns": []any{"specs/**/spec.md"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			report, _, err := BuildDocsLinksResult(ws, map[string]map[string]any{"evidence": {"documents": documents}})
			if err != nil || report.Documents != 3 {
				t.Fatalf("report = %+v, err = %v, want the three default documents", report, err)
			}
		})
	}
	// A docs entry with a format reads like one without.
	report, _, err := BuildDocsLinksResult(ws, map[string]map[string]any{"evidence": {"documents": []any{
		map[string]any{"type": "docs", "format": "markdown", "patterns": []any{"**/README.md"}},
	}}})
	if err != nil || report.Documents != 2 {
		t.Fatalf("report = %+v, err = %v, want the two READMEs a formatted docs entry selects", report, err)
	}
}
