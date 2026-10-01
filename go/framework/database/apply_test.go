package database

import (
	"context"
	stderrors "errors"
	"strings"
	"testing"

	perrors "go.putnami.dev/errors"
)

// TestLoadSQLSource_PublicWrapperMatchesInternal proves the exported
// helper hands back the same Definitions the SQLRunner would have
// materialized internally — same names, same SQL bytes, same order.
func TestLoadSQLSource_PublicWrapperMatchesInternal(t *testing.T) {
	fsys := mapFS(map[string]string{
		"001_init.up.sql":   "CREATE TABLE foo (id text);",
		"001_init.down.sql": "DROP TABLE foo;",
		"002_more.up.sql":   "ALTER TABLE foo ADD COLUMN bar text;",
	})
	src := NewSQLSource("ns", Datasource{Name: "default"}, fsys)

	pub, err := LoadSQLSource(src)
	if err != nil {
		t.Fatalf("LoadSQLSource: %v", err)
	}
	priv, err := loadSQLSource(src)
	if err != nil {
		t.Fatalf("loadSQLSource: %v", err)
	}
	if len(pub) != len(priv) {
		t.Fatalf("len mismatch: pub=%d priv=%d", len(pub), len(priv))
	}
	for i := range pub {
		if pub[i] != priv[i] {
			t.Errorf("definition %d mismatch:\n  pub=%+v\n  priv=%+v", i, pub[i], priv[i])
		}
	}
}

// TestApplyToPool_NilPoolErrors guards the early-return: ApplyToPool
// without a pool can't open the stdlib DB, so the function should
// surface CodeMigrationStartup rather than panic inside the wrapper.
func TestApplyToPool_NilPoolErrors(t *testing.T) {
	src := NewSQLSource("ns", Datasource{Name: "default"}, mapFS(map[string]string{
		"001_init.up.sql": "CREATE TABLE foo (id text);",
	}))
	_, err := ApplyToPool(context.Background(), nil, src)
	if err == nil {
		t.Fatal("expected error for nil pool")
	}
	var coded *perrors.Error
	if !stderrors.As(err, &coded) {
		t.Fatalf("expected structured error, got %T: %v", err, err)
	}
	if coded.Code() != CodeMigrationStartup {
		t.Errorf("expected %q code, got %q", CodeMigrationStartup, coded.Code())
	}
	if !strings.Contains(err.Error(), "pool") {
		t.Errorf("error should mention pool: %v", err)
	}
}

// TestApplyToPool_NoSourcesIsNoop exercises the empty-sources guard:
// callers that conditionally build their source list shouldn't pay for
// a pool round-trip when there's nothing to apply.
func TestApplyToPool_NoSourcesIsNoop(t *testing.T) {
	// A nil pool here proves the function bails before touching the
	// pool — if it didn't, stdlib.OpenDBFromPool would panic on the
	// nil receiver.
	recs, err := ApplyToPool(context.Background(), nil)
	if err != nil {
		t.Fatalf("zero-sources call must succeed, got %v", err)
	}
	if recs != nil {
		t.Errorf("zero-sources call must return nil records, got %d", len(recs))
	}
}
