package infra

import (
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func sampleAggregated() AggregatedManifest {
	src := []Source{{Project: "lib", Contributor: ContributorManual}}
	return AggregatedManifest{
		ProtocolVersion: ProtocolVersion,
		Workload:        "w",
		Databases: []AggregatedDatabase{
			{Name: "primary", Engine: EnginePostgres, Sources: src},
			{Name: "primary", Engine: EngineMySQL, Sources: src},
		},
		Events: &AggregatedEvents{
			Publishes:  []AggregatedTopic{{Name: "a.created", Sources: src}},
			Subscribes: []AggregatedTopic{{Name: "b.updated", Sources: src}},
		},
		Storage:       []AggregatedStorage{{Name: "images", Sources: src}},
		Secrets:       []AggregatedSecret{{Name: "jwks", Sources: src}},
		ScheduledJobs: []AggregatedScheduledJob{{Name: "rollup", Schedule: "0 2 * * *", Sources: src}},
	}
}

func TestApplyOverrides_NilIgnoreIsNoop(t *testing.T) {
	m := sampleAggregated()
	out, diags := ApplyOverrides(m, Overrides{})
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if len(out.Databases) != 2 {
		t.Errorf("Databases changed by a no-op override: %d", len(out.Databases))
	}
}

func TestApplyOverrides_DropsDatabaseByIdentity(t *testing.T) {
	m := sampleAggregated()
	ov := Overrides{Ignore: &IgnoreRules{
		Databases: []DatabaseRef{{Name: "primary", Engine: EnginePostgres}},
	}}
	out, diags := ApplyOverrides(m, ov)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if len(out.Databases) != 1 {
		t.Fatalf("Databases = %d, want 1 (postgres dropped, mysql kept)", len(out.Databases))
	}
	if out.Databases[0].Engine != EngineMySQL {
		t.Errorf("kept engine = %q, want mysql (only the (primary,postgres) identity is dropped)", out.Databases[0].Engine)
	}
}

func TestApplyOverrides_DropsByName(t *testing.T) {
	m := sampleAggregated()
	ov := Overrides{Ignore: &IgnoreRules{
		Storage:       []string{"images"},
		Secrets:       []string{"jwks"},
		ScheduledJobs: []string{"rollup"},
		Events:        &EventsRef{Publishes: []string{"a.created"}},
	}}
	out, diags := ApplyOverrides(m, ov)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if len(out.Storage) != 0 {
		t.Errorf("Storage not dropped: %+v", out.Storage)
	}
	if len(out.Secrets) != 0 {
		t.Errorf("Secrets not dropped: %+v", out.Secrets)
	}
	if len(out.ScheduledJobs) != 0 {
		t.Errorf("ScheduledJobs not dropped: %+v", out.ScheduledJobs)
	}
	if out.Events == nil || len(out.Events.Publishes) != 0 {
		t.Errorf("publish topic not dropped: %+v", out.Events)
	}
	if out.Events == nil || len(out.Events.Subscribes) != 1 {
		t.Errorf("subscribe topic should remain: %+v", out.Events)
	}
}

func TestApplyOverrides_EmptyEventsBlockBecomesNil(t *testing.T) {
	m := sampleAggregated()
	ov := Overrides{Ignore: &IgnoreRules{
		Events: &EventsRef{Publishes: []string{"a.created"}, Subscribes: []string{"b.updated"}},
	}}
	out, _ := ApplyOverrides(m, ov)
	if out.Events != nil {
		t.Errorf("Events should be nil once all topics are dropped, got %+v", out.Events)
	}
}

func TestApplyOverrides_UnusedRuleWarns(t *testing.T) {
	m := sampleAggregated()
	ov := Overrides{Ignore: &IgnoreRules{
		Storage:   []string{"does-not-exist"},
		Databases: []DatabaseRef{{Name: "ghost", Engine: EnginePostgres}},
	}}
	_, diags := ApplyOverrides(m, ov)
	if diag.HasErrors(diags) {
		t.Fatalf("unused rules must be warnings, not errors: %v", diags)
	}
	var unused int
	for _, d := range diags {
		if d.Code == ErrorCodeUnusedOverride {
			unused++
		}
	}
	if unused != 2 {
		t.Errorf("unused-override warnings = %d, want 2", unused)
	}
}

func TestApplyOverrides_DoesNotMutateInput(t *testing.T) {
	m := sampleAggregated()
	ov := Overrides{Ignore: &IgnoreRules{Storage: []string{"images"}}}
	_, _ = ApplyOverrides(m, ov)
	if len(m.Storage) != 1 {
		t.Errorf("ApplyOverrides mutated the input manifest: Storage = %d", len(m.Storage))
	}
}

func TestParseOverrides_RejectsUnknownField(t *testing.T) {
	_, diags := ParseOverrides([]byte(`{"ignore":{},"bogus":1}`))
	if !findCode(diags, ErrorCodeUnknownField) {
		t.Errorf("want %s, got %v", ErrorCodeUnknownField, diags)
	}
}

func TestValidateOverrides_BadEngineAndName(t *testing.T) {
	ov := &Overrides{Ignore: &IgnoreRules{
		Databases: []DatabaseRef{{Name: "ok", Engine: "oracle"}},
		Storage:   []string{"BadName"},
	}}
	diags := ValidateOverrides(ov)
	if !findCode(diags, ErrorCodeInvalidEngine) {
		t.Errorf("want %s, got %v", ErrorCodeInvalidEngine, diags)
	}
	if !findCode(diags, ErrorCodeInvalidName) {
		t.Errorf("want %s, got %v", ErrorCodeInvalidName, diags)
	}
}

func TestLoadOverrides_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, OverridesFilename)
	content := `{"$schema":"x","ignore":{"secrets":["legacy"]}}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	ov, diags := LoadOverrides(path)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if ov == nil || ov.Ignore == nil || len(ov.Ignore.Secrets) != 1 || ov.Ignore.Secrets[0] != "legacy" {
		t.Errorf("loaded overrides = %+v, want ignore.secrets=[legacy]", ov)
	}
}
