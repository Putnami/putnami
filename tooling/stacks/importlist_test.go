package stacks

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestImportListSelectsDirectoriesByPathAndGlob(t *testing.T) {
	list, err := NewImportList([]string{"/packages/pricing", "apps/*", "./libs/[ab]?", "/tools/none"})
	if err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]bool{
		"packages/pricing":  true,
		"/packages/pricing": true,
		"packages/billing":  false,
		"apps/api":          true,
		"apps/api/nested":   false,
		"libs/a1":           true,
		"libs/c1":           false,
	} {
		if got := list.Selects(dir); got != want {
			t.Errorf("Selects(%q) = %v, want %v", dir, got, want)
		}
	}
	if got := list.Unmatched(); !slices.Equal(got, []string{"/tools/none"}) {
		t.Errorf("Unmatched() = %v, want the entry no directory matched", got)
	}
	if list.Len() != 4 {
		t.Errorf("Len() = %d", list.Len())
	}
}

func TestImportListRefusesAnEntryThatNamesNoProject(t *testing.T) {
	for _, entry := range []string{"", " ", "/", ".", "..", "../outside", "packages/[", "./"} {
		if _, err := NewImportList([]string{entry}); err == nil {
			t.Errorf("NewImportList(%q) accepted an entry that names no project", entry)
		}
	}
	if pattern, err := NormalizeImportEntry("/packages//pricing/"); err != nil || pattern != "packages/pricing" {
		t.Errorf("NormalizeImportEntry = %q, %v", pattern, err)
	}
}

func TestANilImportListSelectsNothing(t *testing.T) {
	var list *ImportList
	if list.Selects("packages/pricing") || list.Len() != 0 || list.Unmatched() != nil {
		t.Error("a nil list selected a project")
	}
}

func TestImportEntriesReadsTheOptionLayer(t *testing.T) {
	got, err := ImportEntries("@putnami/nx", map[string]any{"import": []any{"/a", "b/*"}})
	if err != nil || !slices.Equal(got, []string{"/a", "b/*"}) {
		t.Errorf("ImportEntries = %v, %v", got, err)
	}
	if got, err := ImportEntries("@putnami/nx", map[string]any{"mode": "delegate"}); err != nil || got != nil {
		t.Errorf("an absent member read as %v, %v", got, err)
	}
	if _, err := ImportEntries("@putnami/nx", map[string]any{"import": "/a"}); err == nil {
		t.Error("a string read as a list")
	}
	if _, err := ImportEntries("@putnami/nx", map[string]any{"import": []any{"/a", 3}}); err == nil {
		t.Error("a number read as a path")
	}
}

func TestImportParamReadsAListOrACommaSeparatedString(t *testing.T) {
	for raw, want := range map[string][]string{
		`["/a","b"]`: {"/a", "b"},
		`"/a, b,,"`:  {"/a", "b"},
		`""`:         {},
		`[]`:         {},
	} {
		got, ok, err := ImportParam(json.RawMessage(raw))
		if err != nil || !ok || !slices.Equal(got, want) {
			t.Errorf("ImportParam(%s) = %v, %v, %v; want %v", raw, got, ok, err, want)
		}
	}
	if _, ok, err := ImportParam(nil); ok || err != nil {
		t.Error("an absent parameter was given")
	}
	if _, _, err := ImportParam(json.RawMessage(`3`)); err == nil {
		t.Error("a number read as a list")
	}
}
