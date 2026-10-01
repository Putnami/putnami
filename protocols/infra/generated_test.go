package infra

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestSyncGeneratedRequirementsWritesCommittedManifest(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSidecar(dir, "storage", PerProjectManifest{
		Storage: []StorageBucket{{Name: "uploads", Retention: "30d"}},
	}); err != nil {
		t.Fatalf("write storage sidecar: %v", err)
	}
	if err := WriteSidecar(dir, "database", PerProjectManifest{
		Databases: []Database{{Name: "primary", Engine: EnginePostgres, Schemas: []string{"z", "a", "a"}}},
	}); err != nil {
		t.Fatalf("write database sidecar: %v", err)
	}
	if err := os.WriteFile(filepath.Join(GeneratedSidecarDir(dir), "runtime.json"), []byte(`{"scaling":{"max":1}}`), 0o644); err != nil {
		t.Fatalf("write runtime scratch: %v", err)
	}

	diags, err := SyncGeneratedRequirements(dir)
	if err != nil {
		t.Fatalf("SyncGeneratedRequirements: %v", err)
	}
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}

	data, err := os.ReadFile(ProjectRequirementsPath(dir))
	if err != nil {
		t.Fatalf("read committed requirements: %v", err)
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
  "storage": [
    {
      "name": "uploads",
      "retention": "30d"
    }
  ]
}
`
	if string(data) != want {
		t.Fatalf("committed requirements not deterministic.\ngot:\n%s\nwant:\n%s", data, want)
	}
}

func TestSyncGeneratedRequirementsMigratesV1Sidecar(t *testing.T) {
	dir := t.TempDir()
	sidecarDir := GeneratedSidecarDir(dir)
	if err := os.MkdirAll(sidecarDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sidecarDir, "database.json"), []byte(
		`{"protocolVersion":1,"databases":[{"name":"primary","engine":"postgres"}]}`,
	), 0o644); err != nil {
		t.Fatal(err)
	}

	diags, err := SyncGeneratedRequirements(dir)
	if err != nil {
		t.Fatalf("SyncGeneratedRequirements: %v", err)
	}
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if len(diags) != 1 || diags[0].Severity != diag.Warning ||
		diags[0].Code != ErrorCodeInvalidProtocolVersion {
		t.Fatalf("diagnostics = %v, want one protocol migration warning", diags)
	}

	data, err := os.ReadFile(ProjectRequirementsPath(dir))
	if err != nil {
		t.Fatalf("read migrated requirements: %v", err)
	}
	m, parseDiags := ParseAndValidatePerProjectManifest(data)
	if diag.HasErrors(parseDiags) {
		t.Fatalf("migrated manifest does not parse as v2: %v\n%s", parseDiags, data)
	}
	if m.ProtocolVersion != ProtocolVersion || len(m.Databases) != 1 || m.Databases[0].Name != "primary" {
		t.Fatalf("migrated manifest = %+v, want v%d primary database", m, ProtocolVersion)
	}
}

func TestSyncGeneratedRequirementsKeepsV1MigrationStrict(t *testing.T) {
	dir := t.TempDir()
	sidecarDir := GeneratedSidecarDir(dir)
	if err := os.MkdirAll(sidecarDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sidecarDir, "database.json"), []byte(
		`{"protocolVersion":1,"databazes":[{"name":"primary","engine":"postgres"}]}`,
	), 0o644); err != nil {
		t.Fatal(err)
	}

	diags, err := SyncGeneratedRequirements(dir)
	if err != nil {
		t.Fatalf("SyncGeneratedRequirements: %v", err)
	}
	if !diag.HasErrors(diags) {
		t.Fatalf("diagnostics = %v, want the unknown v1 field to remain an error", diags)
	}
	if len(diags) != 1 || diags[0].Code != ErrorCodeUnknownField {
		t.Fatalf("diagnostics = %v, want one unknown-field error", diags)
	}
	if _, err := os.Stat(ProjectRequirementsPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("invalid v1 sidecar produced committed requirements, stat err = %v", err)
	}
}

func TestSyncGeneratedRequirementsRemovesStaleCommittedManifest(t *testing.T) {
	dir := t.TempDir()
	path := ProjectRequirementsPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"protocolVersion":2,"secrets":["old"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := SyncGeneratedRequirements(dir); err != nil {
		t.Fatalf("SyncGeneratedRequirements: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected stale committed requirements removed, stat err = %v", err)
	}
}

func TestClearGeneratedRequirementSidecars(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSidecar(dir, "events", PerProjectManifest{
		Events: &Events{Publishes: []string{"workspace.deploy.completed"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ClearGeneratedRequirementSidecars(dir); err != nil {
		t.Fatalf("ClearGeneratedRequirementSidecars: %v", err)
	}
	if _, err := os.Stat(SidecarPath(dir, "events")); !os.IsNotExist(err) {
		t.Fatalf("expected sidecar removed, stat err = %v", err)
	}
}

func TestSyncGeneratedRequirementsReportsInvalidSidecar(t *testing.T) {
	dir := t.TempDir()
	sidecarDir := GeneratedSidecarDir(dir)
	if err := os.MkdirAll(sidecarDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sidecarDir, "database.json"), []byte(`{"protocolVersion":2,"databazes":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	diags, err := SyncGeneratedRequirements(dir)
	if err != nil {
		t.Fatalf("SyncGeneratedRequirements: %v", err)
	}
	var found bool
	for _, d := range diags {
		if d.Code == ErrorCodeUnknownField && strings.Contains(d.Message, "database.json") {
			found = true
		}
	}
	if !found {
		t.Fatalf("diagnostics = %v, want unknown_field attributed to database sidecar", diags)
	}
}
