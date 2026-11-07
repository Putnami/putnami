package decisions

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
)

const feature = "tooling/observe-decisions"

func write(t *testing.T, root, name, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRecordsAreTheMarkdownFilesOfTheDecisionDirectories(t *testing.T) {
	spectest.Proves(t, feature, "madr-records", "records-are-the-markdown-files-of-the-decision-directories")
	root := t.TempDir()
	for _, name := range []string{
		"docs/decisions/0001-a.md",
		"docs/decisions/archive/0000-old.md",
		"docs/decisions/README.md",
		"docs/decisions/index.md",
		"docs/decisions/adr-template.md",
		"docs/decisions/.drafts/0009-draft.md",
		"docs/decisions/notes.txt",
		"docs/adr/0002-b.md",
		"docs/architecture/0003-c.md",
		"adr/0004-d.md",
	} {
		write(t, root, name, "# x\n")
	}
	got, err := Find(root, Dirs(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"docs/adr/0002-b.md", "docs/decisions/0001-a.md", "docs/decisions/archive/0000-old.md"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("default directories: records = %v, want %v", got, want)
	}
	got, err = Find(root, Dirs([]string{"./adr/", "docs/architecture", "missing"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"adr/0004-d.md", "docs/architecture/0003-c.md"}; !reflect.DeepEqual(got, want) {
		t.Errorf("named directories: records = %v, want %v", got, want)
	}
	if got, err := Find(t.TempDir(), Dirs(nil)); err != nil || len(got) != 0 {
		t.Errorf("no directory: records = %v, %v, want none", got, err)
	}
	if IsRecord("docs/decisions/README.md", Dirs(nil)) || !IsRecord("docs/decisions/0001-a.md", Dirs(nil)) || IsRecord("docs/decisionsx/0001-a.md", Dirs(nil)) {
		t.Error("IsRecord does not follow the directories and the skipped names")
	}
}

func TestFrontMatterFieldsAreRead(t *testing.T) {
	spectest.Proves(t, feature, "madr-fields", "front-matter-fields-are-read")
	record := Parse("docs/decisions/0002-ledger.md", []byte(`---
status: "Accepted"
date: 2025-03-04
title: Payments own the ledger
---

# A heading that is not the title

## Status

proposed
`))
	want := Record{Path: "docs/decisions/0002-ledger.md", Title: "Payments own the ledger", Status: evidence.DecisionAccepted, Date: "2025-03-04"}
	if !reflect.DeepEqual(record, want) {
		t.Errorf("record = %+v, want %+v", record, want)
	}
	// A front matter without a title takes the heading; an invalid date is
	// dropped.
	record = Parse("docs/adr/0005-x.md", []byte("---\nstatus: superseded by ADR-0006\ndate: 2025-13-01\n---\n# Use X\n"))
	if record.Title != "Use X" || record.Status != evidence.DecisionSuperseded || record.Date != "" {
		t.Errorf("record = %+v", record)
	}
}

func TestBodyFieldsAreRead(t *testing.T) {
	spectest.Proves(t, feature, "madr-fields", "body-fields-are-read")
	cases := map[string]Record{
		"# Charges are synchronous\n\n* Status: accepted\n* Date: 2025-02-10\n":           {Title: "Charges are synchronous", Status: evidence.DecisionAccepted, Date: "2025-02-10"},
		"# ADR 0001 — One document\n\n- **Status**: Deprecated\n- **Date**: 2026-11-01\n": {Title: "ADR 0001 — One document", Status: evidence.DecisionDeprecated, Date: "2026-11-01"},
		"# Queues\n\n## Status\n\nSuperseded by [0002](0002-x.md)\n\n## Context\n":        {Title: "Queues", Status: evidence.DecisionSuperseded},
		"# Rejected idea\n\nStatus: rejected\n":                                           {Title: "Rejected idea", Status: evidence.DecisionRejected},
		"# Draft\n\n* Status: proposed | rejected | accepted\n":                           {Title: "Draft", Status: evidence.DecisionProposed},
		"# Unclear\n\n* Status: in review\n* Date: last week\n":                           {Title: "Unclear", Status: evidence.DecisionUnknown},
		"```\n# Not a heading\nStatus: accepted\n```\n\nno heading here\n":                {Title: "0007-fallback", Status: evidence.DecisionUnknown},
	}
	for text, want := range cases {
		want.Path = "docs/decisions/0007-fallback.md"
		if got := Parse(want.Path, []byte(text)); !reflect.DeepEqual(got, want) {
			t.Errorf("Parse(%q) = %+v, want %+v", text, got, want)
		}
	}
	if got := Parse("docs/decisions/0008-empty.md", nil); got.Title != "0008-empty" || got.Status != evidence.DecisionUnknown {
		t.Errorf("an empty record = %+v", got)
	}
}

func TestFrontMatterAndSectionScopesAreRead(t *testing.T) {
	spectest.Proves(t, feature, "madr-scope", "front-matter-and-section-scopes-are-read")
	cases := map[string][]string{
		"---\nstatus: accepted\nscope:\n  - src/payments/\n  - ./src/ledger\n---\n# X\n":                                           {"src/payments", "src/ledger"},
		"---\nscope: /services/api/**\n---\n# X\n":                                                                                 {"services/api/**"},
		"---\nscope: [a, b]\n---\n# X\n\n## Scope\n\n- c\n":                                                                        {"a", "b"},
		"# X\n\n## Scope\n\n- `src/**/queue/*.go`\n- docs/queues.md (the queue map)\n* ../outside\n\n## Decision\n\n- not/scope\n": {"src/**/queue/*.go", "docs/queues.md"},
		"# X\n\nNo scope.\n": nil,
	}
	for text, want := range cases {
		if got := Parse("docs/decisions/0001-x.md", []byte(text)).Scope; !reflect.DeepEqual(got, want) {
			t.Errorf("scope of %q = %v, want %v", text, got, want)
		}
	}
}

func TestAPlainPatternGovernsItsTree(t *testing.T) {
	spectest.Proves(t, feature, "decision-scope-matching", "a-plain-pattern-governs-its-tree")
	for _, file := range []string{"src/payments", "src/payments/charge.go", "src/payments/ledger/entry.go"} {
		if !Matches("src/payments", file) {
			t.Errorf("src/payments does not govern %s", file)
		}
	}
	for _, file := range []string{"src/payments-legacy/charge.go", "src/pay", "docs/src/payments/x.md"} {
		if Matches("src/payments", file) {
			t.Errorf("src/payments governs %s", file)
		}
	}
}

func TestGlobPatternsMatchSegments(t *testing.T) {
	spectest.Proves(t, feature, "decision-scope-matching", "glob-patterns-match-segments")
	cases := []struct {
		pattern, file string
		want          bool
	}{
		{"src/*/queue.go", "src/billing/queue.go", true},
		{"src/*/queue.go", "src/billing/sub/queue.go", false},
		{"src/**/queue/*.go", "src/billing/queue/worker.go", true},
		{"src/**/queue/*.go", "src/queue/worker.go", true},
		{"src/**/queue/*.go", "src/a/b/queue/worker.go", true},
		{"src/**/queue/*.go", "src/billing/queue/sub/worker.go", false},
		{"**/*.sql", "migrations/0001.sql", true},
		{"**", "anything/at/all", true},
		{"src/?.go", "src/a.go", true},
		{"src/?.go", "src/ab.go", false},
		{"src/[", "src/[", false},
	}
	for _, c := range cases {
		if got := Matches(c.pattern, c.file); got != c.want {
			t.Errorf("Matches(%q, %q) = %v, want %v", c.pattern, c.file, got, c.want)
		}
	}
}
