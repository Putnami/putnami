package infra

import (
	"reflect"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestMerge_EmptyContributions(t *testing.T) {
	m, diags := Merge("w", nil)
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if m.Workload != "w" {
		t.Errorf("Workload = %q, want w", m.Workload)
	}
	if m.ProtocolVersion != ProtocolVersion {
		t.Errorf("ProtocolVersion = %d, want %d", m.ProtocolVersion, ProtocolVersion)
	}
}

func TestMerge_DatabaseSchemaUnion(t *testing.T) {
	contribs := []ProjectContribution{
		{Project: "a", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Databases:       []Database{{Name: "primary", Engine: EnginePostgres, Schemas: []string{"audit"}}},
		}},
		{Project: "b", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Databases:       []Database{{Name: "primary", Engine: EnginePostgres, Schemas: []string{"iam", "audit"}}},
		}},
	}
	m, diags := Merge("w", contribs)
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if len(m.Databases) != 1 {
		t.Fatalf("Databases len = %d, want 1", len(m.Databases))
	}
	wantSchemas := []string{"audit", "iam"}
	if !reflect.DeepEqual(m.Databases[0].Schemas, wantSchemas) {
		t.Errorf("Schemas = %v, want %v", m.Databases[0].Schemas, wantSchemas)
	}
	if len(m.Databases[0].Sources) != 2 {
		t.Errorf("Sources len = %d, want 2", len(m.Databases[0].Sources))
	}
}

func TestMerge_DatabaseDifferentEnginesSplit(t *testing.T) {
	contribs := []ProjectContribution{
		{Project: "a", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Databases:       []Database{{Name: "primary", Engine: EnginePostgres}},
		}},
		{Project: "b", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Databases:       []Database{{Name: "primary", Engine: EngineMySQL}},
		}},
	}
	m, _ := Merge("w", contribs)
	if len(m.Databases) != 2 {
		t.Fatalf("Databases len = %d, want 2", len(m.Databases))
	}
}

func TestMerge_StorageConflictingRetention(t *testing.T) {
	contribs := []ProjectContribution{
		{Project: "a", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Storage:         []StorageBucket{{Name: "images", Retention: "30d"}},
		}},
		{Project: "b", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Storage:         []StorageBucket{{Name: "images", Retention: "60d"}},
		}},
	}
	m, diags := Merge("w", contribs)
	if !findCode(diags, ErrorCodeConflictingValue) {
		t.Fatalf("want conflicting_value diagnostic, got %v", diags)
	}
	if len(m.Storage) != 1 {
		t.Fatalf("Storage len = %d, want 1", len(m.Storage))
	}
	if m.Storage[0].Retention != "30d" {
		t.Errorf("Retention = %q, want first-wins value 30d", m.Storage[0].Retention)
	}
}

func TestMerge_StorageFillEmptyRetention(t *testing.T) {
	contribs := []ProjectContribution{
		{Project: "a", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Storage:         []StorageBucket{{Name: "images"}},
		}},
		{Project: "b", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Storage:         []StorageBucket{{Name: "images", Retention: "30d"}},
		}},
	}
	m, diags := Merge("w", contribs)
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if m.Storage[0].Retention != "30d" {
		t.Errorf("Retention = %q, want 30d (later non-empty value fills earlier empty)", m.Storage[0].Retention)
	}
}

func TestMerge_StorageUnionsAccess(t *testing.T) {
	// Two projects share a bucket needing different access; the merged grant is
	// the union (read ∪ write = readwrite) so neither consumer is under-granted.
	// Unlike retention, differing access is not a conflict — it is additive.
	contribs := []ProjectContribution{
		{Project: "reader", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Storage:         []StorageBucket{{Name: "images", Access: StorageAccessRead}},
		}},
		{Project: "writer", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Storage:         []StorageBucket{{Name: "images", Access: StorageAccessWrite}},
		}},
	}
	m, diags := Merge("w", contribs)
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if len(m.Storage) != 1 {
		t.Fatalf("Storage len = %d, want 1", len(m.Storage))
	}
	if m.Storage[0].Access != StorageAccessReadWrite {
		t.Errorf("Access = %q, want union readwrite", m.Storage[0].Access)
	}
}

func TestMerge_StorageOrsPublic(t *testing.T) {
	// public is an additive capability: if any consumer needs public read access
	// the merged bucket is public.
	contribs := []ProjectContribution{
		{Project: "a", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Storage:         []StorageBucket{{Name: "assets", Access: StorageAccessRead}},
		}},
		{Project: "b", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Storage:         []StorageBucket{{Name: "assets", Access: StorageAccessRead, Public: true}},
		}},
	}
	m, diags := Merge("w", contribs)
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if len(m.Storage) != 1 {
		t.Fatalf("Storage len = %d, want 1", len(m.Storage))
	}
	if !m.Storage[0].Public {
		t.Error("Public = false, want true (ORed across contributors)")
	}
}

func TestMerge_ManualOverridesFrameworkRetention(t *testing.T) {
	// Same project contributes a framework-inferred retention and a
	// developer-authored one. The manual value must win, and the override is
	// reported as a non-fatal warning rather than an unresolved error.
	contribs := []ProjectContribution{
		{Project: "a", Contributor: FrameworkContributor("go.putnami.dev/storage"), Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Storage:         []StorageBucket{{Name: "images", Retention: "90d"}},
		}},
		{Project: "a", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Storage:         []StorageBucket{{Name: "images", Retention: "30d"}},
		}},
	}
	m, diags := Merge("w", contribs)
	if !findCode(diags, ErrorCodeConflictingValue) {
		t.Fatalf("want conflicting_value diagnostic, got %v", diags)
	}
	if diag.HasErrors(diags) {
		t.Errorf("override should be a warning, not an error: %v", diags)
	}
	if len(m.Storage) != 1 {
		t.Fatalf("Storage len = %d, want 1", len(m.Storage))
	}
	if m.Storage[0].Retention != "30d" {
		t.Errorf("Retention = %q, want 30d (manual overrides framework)", m.Storage[0].Retention)
	}
	if len(m.Storage[0].Sources) != 2 {
		t.Errorf("Sources len = %d, want 2 (both framework and manual recorded)", len(m.Storage[0].Sources))
	}
}

func TestMerge_FrameworkDoesNotOverrideManualRetention(t *testing.T) {
	// Reverse order of inputs: the manual value is established first and a
	// later framework value must not override it. (Sorting puts framework
	// first, so this also exercises the lower-precedence-loses path.)
	contribs := []ProjectContribution{
		{Project: "a", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Storage:         []StorageBucket{{Name: "images", Retention: "30d"}},
		}},
		{Project: "a", Contributor: FrameworkContributor("go.putnami.dev/storage"), Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Storage:         []StorageBucket{{Name: "images", Retention: "90d"}},
		}},
	}
	m, diags := Merge("w", contribs)
	if !findCode(diags, ErrorCodeConflictingValue) {
		t.Fatalf("want conflicting_value diagnostic, got %v", diags)
	}
	if m.Storage[0].Retention != "30d" {
		t.Errorf("Retention = %q, want 30d (manual wins regardless of input order)", m.Storage[0].Retention)
	}
}

func TestMerge_ManualOverridesFrameworkSchedule(t *testing.T) {
	contribs := []ProjectContribution{
		{Project: "a", Contributor: FrameworkContributor("go.putnami.dev/scheduler"), Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			ScheduledJobs:   []ScheduledJob{{Name: "rollup", Schedule: "0 3 * * *"}},
		}},
		{Project: "a", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			ScheduledJobs:   []ScheduledJob{{Name: "rollup", Schedule: "0 2 * * *"}},
		}},
	}
	m, diags := Merge("w", contribs)
	if !findCode(diags, ErrorCodeConflictingValue) {
		t.Fatalf("want conflicting_value diagnostic, got %v", diags)
	}
	if diag.HasErrors(diags) {
		t.Errorf("override should be a warning, not an error: %v", diags)
	}
	if len(m.ScheduledJobs) != 1 || m.ScheduledJobs[0].Schedule != "0 2 * * *" {
		t.Errorf("ScheduledJobs = %+v, want rollup schedule 0 2 * * * (manual overrides framework)", m.ScheduledJobs)
	}
}

func TestMerge_EventsDedup(t *testing.T) {
	contribs := []ProjectContribution{
		{Project: "a", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Events:          &Events{Publishes: []string{"x"}},
		}},
		{Project: "b", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Events:          &Events{Publishes: []string{"x"}},
		}},
	}
	m, _ := Merge("w", contribs)
	if m.Events == nil || len(m.Events.Publishes) != 1 {
		t.Fatalf("Publishes len = %d, want 1", len(m.Events.Publishes))
	}
	if len(m.Events.Publishes[0].Sources) != 2 {
		t.Errorf("Sources len = %d, want 2", len(m.Events.Publishes[0].Sources))
	}
}

func TestMerge_ScheduledJobConflictingSchedule(t *testing.T) {
	contribs := []ProjectContribution{
		{Project: "a", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			ScheduledJobs:   []ScheduledJob{{Name: "rollup", Schedule: "0 2 * * *"}},
		}},
		{Project: "b", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			ScheduledJobs:   []ScheduledJob{{Name: "rollup", Schedule: "0 3 * * *"}},
		}},
	}
	_, diags := Merge("w", contribs)
	if !findCode(diags, ErrorCodeConflictingValue) {
		t.Fatalf("want conflicting_value diagnostic for schedule, got %v", diags)
	}
}

func TestMerge_ScheduledJobConflictingEntrypoint(t *testing.T) {
	contribs := []ProjectContribution{
		{Project: "a", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			ScheduledJobs:   []ScheduledJob{{Name: "rollup", Schedule: "0 2 * * *", Entrypoint: "jobs.Rollup"}},
		}},
		{Project: "b", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			ScheduledJobs:   []ScheduledJob{{Name: "rollup", Schedule: "0 2 * * *", Entrypoint: "jobs.Other"}},
		}},
	}
	_, diags := Merge("w", contribs)
	if !findCode(diags, ErrorCodeConflictingValue) {
		t.Fatalf("want conflicting_value diagnostic for entrypoint, got %v", diags)
	}
}

func TestMerge_SecretsDedup(t *testing.T) {
	contribs := []ProjectContribution{
		{Project: "a", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Secrets:         []string{"jwks_key"},
		}},
		{Project: "b", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Secrets:         []string{"jwks_key"},
		}},
	}
	m, _ := Merge("w", contribs)
	if len(m.Secrets) != 1 {
		t.Fatalf("Secrets len = %d, want 1", len(m.Secrets))
	}
	if len(m.Secrets[0].Sources) != 2 {
		t.Errorf("Sources len = %d, want 2", len(m.Secrets[0].Sources))
	}
}

func TestMerge_SameProjectMultipleContributors(t *testing.T) {
	contribs := []ProjectContribution{
		{Project: "a", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Secrets:         []string{"x"},
		}},
		{Project: "a", Contributor: FrameworkContributor("go.putnami.dev/database"), Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion,
			Secrets:         []string{"x"},
		}},
	}
	m, _ := Merge("w", contribs)
	if len(m.Secrets) != 1 {
		t.Fatalf("Secrets len = %d, want 1", len(m.Secrets))
	}
	if len(m.Secrets[0].Sources) != 2 {
		t.Errorf("Sources len = %d, want 2 (manual + framework on same project must both appear)", len(m.Secrets[0].Sources))
	}
}

func TestMerge_SourcesSortedAndDeduped(t *testing.T) {
	src := Source{Project: "p", Contributor: ContributorManual}
	contribs := []ProjectContribution{
		{Project: "p", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion, Secrets: []string{"a"},
		}},
		{Project: "p", Contributor: ContributorManual, Manifest: PerProjectManifest{
			ProtocolVersion: ProtocolVersion, Secrets: []string{"a"},
		}},
	}
	m, _ := Merge("w", contribs)
	if len(m.Secrets[0].Sources) != 1 {
		t.Errorf("Sources len = %d, want 1 (duplicate project+contributor deduped)", len(m.Secrets[0].Sources))
	}
	if m.Secrets[0].Sources[0] != src {
		t.Errorf("Sources[0] = %+v, want %+v", m.Secrets[0].Sources[0], src)
	}
}

func TestWithRuntime(t *testing.T) {
	m := AggregatedManifest{ProtocolVersion: ProtocolVersion, Workload: "w"}
	domain := "api.example.com"
	rt := &Runtime{Ingress: &Ingress{Domain: &domain}}
	out := WithRuntime(m, rt)
	if out.Runtime != rt {
		t.Errorf("Runtime = %+v, want %+v", out.Runtime, rt)
	}
	if m.Runtime != nil {
		t.Error("WithRuntime mutated input")
	}
}
