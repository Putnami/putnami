package migration

import (
	"testing"

	"go.putnami.dev/logger"
)

// TestRegistry_Records pins the registry's half of the migration log contract
// (protocols/logging/conformance): the pinned "database.migration" logger name
// shared with the SQL migrator/runner, and identifiers carried in a camelCase
// "migration" group rather than as flat snake_case attrs.
func TestRegistry_Records(t *testing.T) {
	sink := logger.NewMemorySink()
	r := NewRegistry()
	// A root logger with NO name, so the derived name is exactly the pinned one.
	r.log = migrationLoggerFrom(logger.New("", logger.LevelDebug, sink))

	if err := r.AddSource(stubSource{kind: KindSQL, ns: "iam"}); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	if err := r.RegisterRunner(&stubRunner{kind: KindSQL}); err != nil {
		t.Fatalf("RegisterRunner: %v", err)
	}

	if sink.Len() != 2 {
		t.Fatalf("expected 2 records (source + runner), got %d", sink.Len())
	}
	for _, entry := range sink.Entries {
		if entry.Logger != loggerName {
			t.Errorf("record %q logger = %q, want %q", entry.Message, entry.Logger, loggerName)
		}
		group := migrationGroupOf(t, entry)
		if got := group["kind"]; got != string(KindSQL) {
			t.Errorf("record %q migration.kind = %v, want %q", entry.Message, got, KindSQL)
		}
		if _, ok := group["sourcesForKind"]; !ok {
			t.Errorf("record %q missing camelCase migration.sourcesForKind (got %v)", entry.Message, group)
		}
		// No flat, snake_case leftovers outside the group.
		for _, attr := range entry.Attrs {
			if attr.Key != "migration" {
				t.Errorf("record %q carries out-of-group attr %q", entry.Message, attr.Key)
			}
		}
	}
}

// migrationGroupOf returns a record's "migration" group.
func migrationGroupOf(t *testing.T, entry logger.LogEntry) map[string]any {
	t.Helper()
	for _, attr := range entry.Attrs {
		if attr.Key != "migration" {
			continue
		}
		group, ok := attr.Value.Any().(map[string]any)
		if !ok {
			t.Fatalf("migration attr is not a map: %T", attr.Value.Any())
		}
		return group
	}
	t.Fatalf("no migration group on record %q", entry.Message)
	return nil
}
