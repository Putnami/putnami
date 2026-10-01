package infra

import (
	"strings"
	"testing"
)

// TestMarshalCommittedShape pins the committed layout: objects and
// arrays-of-objects stay expanded, while scalar arrays that fit on the line
// collapse. This is the exact shape Biome's expand:"auto" formatter leaves
// untouched.
func TestMarshalCommittedShape(t *testing.T) {
	m := PerProjectManifest{
		Schema:          PerProjectSchemaURL,
		ProtocolVersion: ProtocolVersion,
		Databases: []Database{
			{Name: "primary", Engine: EnginePostgres, Schemas: []string{"a", "z"}},
		},
		Events:  &Events{Publishes: []string{"x.y"}, Subscribes: []Subscription{{Topic: "a.b"}, {Topic: "c.d"}}},
		Secrets: []string{"k1", "k2"},
	}

	got, err := marshalCommitted(m)
	if err != nil {
		t.Fatalf("marshalCommitted: %v", err)
	}

	want := `{
  "$schema": "https://putnami.dev/schemas/putnami-infra.json",
  "protocolVersion": 2,
  "databases": [
    {
      "name": "primary",
      "engine": "postgres",
      "schemas": ["a", "z"]
    }
  ],
  "events": {
    "publishes": ["x.y"],
    "subscribes": ["a.b", "c.d"]
  },
  "secrets": ["k1", "k2"]
}
`
	if string(got) != want {
		t.Fatalf("committed layout mismatch.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestMarshalCommittedScalarArrayFitsThreshold exercises the 120-column fit
// rule against the boundary measured from Biome itself. A top-level "secrets"
// array opens at column 13 (2 indent + len(`"secrets": `)), so a single element
// of length L collapses while 13 + len(`["X…"]`) <= 120, i.e. L <= 103.
func TestMarshalCommittedScalarArrayFitsThreshold(t *testing.T) {
	for _, tc := range []struct {
		length        int
		wantCollapsed bool
	}{
		{103, true},
		{104, false},
	} {
		m := PerProjectManifest{
			Schema:          PerProjectSchemaURL,
			ProtocolVersion: ProtocolVersion,
			Secrets:         []string{strings.Repeat("x", tc.length)},
		}
		got, err := marshalCommitted(m)
		if err != nil {
			t.Fatalf("len %d: marshalCommitted: %v", tc.length, err)
		}
		collapsed := strings.Contains(string(got), `"secrets": ["`)
		if collapsed != tc.wantCollapsed {
			t.Fatalf("len %d: collapsed=%v want %v\n%s", tc.length, collapsed, tc.wantCollapsed, got)
		}
	}
}

// TestMarshalCommittedCountsTrailingComma verifies the trailing comma of a
// non-final property counts toward the fit check, matching Biome. With a
// scheduledJobs entry after "secrets", the comma after the array's `]` shifts
// the threshold down by one (L <= 102 collapses).
func TestMarshalCommittedCountsTrailingComma(t *testing.T) {
	for _, tc := range []struct {
		length        int
		wantCollapsed bool
	}{
		{102, true},
		{103, false},
	} {
		m := PerProjectManifest{
			Schema:          PerProjectSchemaURL,
			ProtocolVersion: ProtocolVersion,
			Secrets:         []string{strings.Repeat("x", tc.length)},
			ScheduledJobs:   []ScheduledJob{{Name: "nightly", Schedule: "0 2 * * *"}},
		}
		got, err := marshalCommitted(m)
		if err != nil {
			t.Fatalf("len %d: marshalCommitted: %v", tc.length, err)
		}
		collapsed := strings.Contains(string(got), `"secrets": ["`)
		if collapsed != tc.wantCollapsed {
			t.Fatalf("len %d: collapsed=%v want %v\n%s", tc.length, collapsed, tc.wantCollapsed, got)
		}
	}
}

// TestMarshalCommittedExpandsLongScalarArray confirms an overflowing scalar
// array falls back to one element per line, exactly like MarshalIndent.
func TestMarshalCommittedExpandsLongScalarArray(t *testing.T) {
	long := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		long = append(long, strings.Repeat("topic", 4))
	}
	m := PerProjectManifest{
		Schema:          PerProjectSchemaURL,
		ProtocolVersion: ProtocolVersion,
		Events:          &Events{Publishes: long},
	}
	got, err := marshalCommitted(m)
	if err != nil {
		t.Fatalf("marshalCommitted: %v", err)
	}
	if !strings.Contains(string(got), "\"publishes\": [\n") {
		t.Fatalf("expected publishes expanded one per line, got:\n%s", got)
	}
}
