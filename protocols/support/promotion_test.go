package support

import (
	"os"
	"strings"
	"testing"
)

// The promotion process is a public commitment, so it is pinned against the
// code's vocabulary rather than left to review (ADR 0002).
//
// A status the validator accepts but the documentation never explains is a
// promise with no stated price: the entry validates, the catalog looks
// authoritative, and nobody can say what earns it. Adding a fourth status
// therefore costs a documented row.

const (
	promotionSectionHeading = "## Promotion and demotion"
	promotionStatusHeader   = "| Status "
	promotionLadder         = "`experimental` → `preview` → `stable`"
)

func TestPromotionDocumentationCoversEveryStatus(t *testing.T) {
	section := promotionSection(t)
	rows := statusTableRows(section)
	if len(rows) == 0 {
		t.Fatalf("%q documents no status table — the gate would be vacuous", promotionSectionHeading)
	}
	for status := range ValidStatuses {
		if !rows[string(status)] {
			t.Errorf("support status %q is accepted by ValidateCatalog but has no documented requirements", status)
		}
	}
	for row := range rows {
		if !ValidStatuses[Status(row)] {
			t.Errorf("the promotion table documents %q, which is not a v1 support status", row)
		}
	}
	if len(rows) != len(ValidStatuses) {
		t.Errorf("promotion table has %d rows for %d statuses", len(rows), len(ValidStatuses))
	}
}

func TestPromotionDocumentationStatesTheLadderAndItsRecord(t *testing.T) {
	section := promotionSection(t)
	for _, required := range []struct{ phrase, why string }{
		{promotionLadder, "the promotion order, one step at a time"},
		{"### Promotion", "what a promotion pull request carries"},
		{"### Demotion", "that a demotion may skip steps and takes effect immediately"},
		{"default: false", "that demoting to experimental also clears the default"},
		{"GOVERNANCE.md", "who approves the move"},
		{"@putnami/python", "the standing Python decision"},
	} {
		if !strings.Contains(section, required.phrase) {
			t.Errorf("the promotion section does not state %s (missing %q)", required.why, required.phrase)
		}
	}
}

func TestStatusTableRowParserDiscriminates(t *testing.T) {
	if rows := statusTableRows("prose with no table at all\n"); len(rows) != 0 {
		t.Fatalf("parser invented %d rows from prose", len(rows))
	}
	document := strings.Join([]string{
		"| Status | Requires |",
		"|---|---|",
		"| `stable` | evidence |",
		"",
		"| Other | Column |",
		"|---|---|",
		"| `not-a-status` | ignored |",
	}, "\n")
	rows := statusTableRows(document)
	if len(rows) != 1 || !rows["stable"] {
		t.Fatalf("parser read %v, want only the status table's own row", rows)
	}
}

// promotionSection returns the README section that specifies promotion, so a
// phrase living somewhere else in the file cannot satisfy the gate.
func promotionSection(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(data)
	start := strings.Index(readme, promotionSectionHeading)
	if start < 0 {
		t.Fatalf("README.md has no %q section", promotionSectionHeading)
	}
	rest := readme[start+len(promotionSectionHeading):]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		return rest[:end]
	}
	return rest
}

// statusTableRows returns the first cell of every row of the first table whose
// header column is "Status", with surrounding backticks removed.
func statusTableRows(section string) map[string]bool {
	rows := make(map[string]bool)
	inTable := false
	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		if !inTable {
			inTable = strings.HasPrefix(trimmed, promotionStatusHeader)
			continue
		}
		if !strings.HasPrefix(trimmed, "|") {
			break
		}
		cell := strings.TrimSpace(strings.SplitN(strings.TrimPrefix(trimmed, "|"), "|", 2)[0])
		if cell == "" || strings.HasPrefix(cell, "-") || strings.HasPrefix(cell, ":") {
			continue
		}
		rows[strings.Trim(cell, "`")] = true
	}
	return rows
}
