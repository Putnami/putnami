package database

import (
	"strings"
	"testing"
	"testing/fstest"
)

func mapFS(files map[string]string) fstest.MapFS {
	out := make(fstest.MapFS, len(files))
	for path, body := range files {
		out[path] = &fstest.MapFile{Data: []byte(body)}
	}
	return out
}

func TestLoadSQLSource_PairsUpAndDown(t *testing.T) {
	fsys := mapFS(map[string]string{
		"20260520_a.up.sql":   "CREATE TABLE a();",
		"20260520_a.down.sql": "DROP TABLE a;",
		"20260521_b.up.sql":   "CREATE TABLE b();",
	})
	src := NewSQLSource("iam", Datasource{Name: "default"}, fsys)

	defs, err := loadSQLSource(src)
	if err != nil {
		t.Fatalf("loadSQLSource: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("expected 2 definitions, got %d", len(defs))
	}
	// Sorted by Name.
	if defs[0].Name != "iam/20260520_a" || defs[1].Name != "iam/20260521_b" {
		t.Errorf("unexpected order: %v", []string{defs[0].Name, defs[1].Name})
	}
	if defs[0].Down == "" {
		t.Error("expected paired down SQL on a")
	}
	if defs[1].Down != "" {
		t.Error("expected b to be non-reversible (no down)")
	}
	if !strings.HasPrefix(defs[0].Source, "embed:iam/") {
		t.Errorf("Source not stamped: %s", defs[0].Source)
	}
	if defs[0].Namespace != "iam" || defs[0].Datasource != "default" {
		t.Errorf("metadata not propagated: %+v", defs[0])
	}
}

func TestLoadSQLSource_OrphanDownIsFatal(t *testing.T) {
	fsys := mapFS(map[string]string{
		"20260520_a.up.sql":   "CREATE TABLE a();",
		"20260520_a.down.sql": "DROP TABLE a;",
		"20260521_b.down.sql": "DROP TABLE b;", // no .up.sql sibling
	})
	src := NewSQLSource("iam", Datasource{Name: "default"}, fsys)

	_, err := loadSQLSource(src)
	if err == nil {
		t.Fatal("expected orphan .down.sql to fail loading")
	}
	if !strings.Contains(err.Error(), "orphan") {
		t.Fatalf("error must mention orphan, got %v", err)
	}
}

func TestLoadSQLSource_SkipsNonSQLFiles(t *testing.T) {
	fsys := mapFS(map[string]string{
		"20260520_a.up.sql": "CREATE TABLE a();",
		"README.md":         "ignored",
		"helpers/notes.txt": "ignored",
		"20260521_b.up.sql": "CREATE TABLE b();",
	})
	src := NewSQLSource("iam", Datasource{Name: "default"}, fsys)

	defs, err := loadSQLSource(src)
	if err != nil {
		t.Fatalf("loadSQLSource: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("expected 2 defs, got %d (%v)", len(defs), defs)
	}
}

func TestLoadSQLSource_WalksSubdirectories(t *testing.T) {
	fsys := mapFS(map[string]string{
		"2026/05/20_a.up.sql":   "X",
		"2026/05/20_a.down.sql": "Y",
		"2026/05/21_b.up.sql":   "Z",
	})
	src := NewSQLSource("iam", Datasource{Name: "default"}, fsys)

	defs, err := loadSQLSource(src)
	if err != nil {
		t.Fatalf("loadSQLSource: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("expected 2, got %d", len(defs))
	}
}

func TestLoadSQLSource_DuplicateBasenameAcrossDirsIsFatal(t *testing.T) {
	fsys := mapFS(map[string]string{
		"a/001_init.up.sql": "X",
		"b/001_init.up.sql": "Y",
	})
	src := NewSQLSource("iam", Datasource{Name: "default"}, fsys)

	_, err := loadSQLSource(src)
	if err == nil {
		t.Fatal("expected duplicate-basename error")
	}
}

func TestLoadSQLSource_InlineStacksOnFS(t *testing.T) {
	fsys := mapFS(map[string]string{
		"001_init.up.sql": "CREATE TABLE x();",
	})
	src := NewSQLSource("iam", Datasource{Name: "default"}, fsys,
		Definition{Name: "002_seed", SQL: "INSERT ..."},
	)

	defs, err := loadSQLSource(src)
	if err != nil {
		t.Fatalf("loadSQLSource: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("expected 2 definitions, got %d", len(defs))
	}
	// Inline def must be namespaced and Source-stamped.
	inline := defs[1]
	if inline.Name != "iam/002_seed" {
		t.Errorf("inline name not namespaced: %s", inline.Name)
	}
	if inline.Source != "inline:iam" {
		t.Errorf("inline Source not stamped: %s", inline.Source)
	}
	if inline.Namespace != "iam" || inline.Datasource != "default" {
		t.Errorf("inline metadata not stamped: %+v", inline)
	}
}

func TestLoadSQLSource_InlineRejectsEmptySQL(t *testing.T) {
	src := NewSQLSource("iam", Datasource{Name: "default"}, nil,
		Definition{Name: "001_init"}, // missing SQL
	)
	_, err := loadSQLSource(src)
	if err == nil {
		t.Fatal("expected empty-SQL error")
	}
}

func TestLoadSQLSource_AcceptsMixedCaseSuffix(t *testing.T) {
	// Suffix match is case-insensitive so the same files behave the same
	// on case-sensitive Linux and case-insensitive macOS APFS. Mixed-case
	// suffixes (.UP.sql, .Down.SQL) must be recognized.
	fsys := mapFS(map[string]string{
		"20260520_a.UP.sql":   "CREATE TABLE a();",
		"20260520_a.Down.SQL": "DROP TABLE a;",
	})
	src := NewSQLSource("iam", Datasource{Name: "default"}, fsys)

	defs, err := loadSQLSource(src)
	if err != nil {
		t.Fatalf("loadSQLSource: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("expected 1 paired definition, got %d", len(defs))
	}
	if defs[0].Down == "" {
		t.Error("expected Down SQL from .Down.SQL sibling")
	}
}

func TestLoadSQLSource_PreservesFullyNamespacedName(t *testing.T) {
	// Inline author already provided "ns/basename" — loader keeps it.
	src := NewSQLSource("iam", Datasource{Name: "default"}, nil,
		Definition{Name: "iam/001_init", SQL: "X"},
	)
	defs, err := loadSQLSource(src)
	if err != nil {
		t.Fatalf("loadSQLSource: %v", err)
	}
	if defs[0].Name != "iam/001_init" {
		t.Errorf("name double-prefixed: %s", defs[0].Name)
	}
}
