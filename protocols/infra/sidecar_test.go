package infra

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWriteSidecarWritesAtomic(t *testing.T) {
	dir := t.TempDir()

	err := WriteSidecar(dir, "database", PerProjectManifest{
		Databases: []Database{{Name: "primary", Engine: EnginePostgres, Schemas: []string{"iam"}}},
	})
	if err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}

	path := SidecarPath(dir, "database")
	wantPath := filepath.Join(dir, AggregatedManifestDir, PerProjectManifestDir, "database.json")
	if path != wantPath {
		t.Errorf("SidecarPath = %q, want %q", path, wantPath)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	var m PerProjectManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Schema != PerProjectSchemaURL {
		t.Errorf("schema = %q, want %q", m.Schema, PerProjectSchemaURL)
	}
	if m.ProtocolVersion != ProtocolVersion {
		t.Errorf("protocolVersion = %d, want %d", m.ProtocolVersion, ProtocolVersion)
	}
	if !reflect.DeepEqual(m.Databases, []Database{{Name: "primary", Engine: EnginePostgres, Schemas: []string{"iam"}}}) {
		t.Errorf("databases = %+v", m.Databases)
	}
}

func TestWriteSidecarRemovesFileWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSidecar(dir, "storage", PerProjectManifest{
		Storage: []StorageBucket{{Name: "uploads"}},
	}); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	// A producer whose declared resources go to zero must remove its sidecar
	// so the next generator sync does not commit stale data.
	if err := WriteSidecar(dir, "storage", PerProjectManifest{}); err != nil {
		t.Fatalf("clear write: %v", err)
	}

	if _, err := os.Stat(SidecarPath(dir, "storage")); !os.IsNotExist(err) {
		t.Fatalf("expected sidecar removed, stat err = %v", err)
	}
}

func TestWriteSidecarMultipleProducersCoexist(t *testing.T) {
	dir := t.TempDir()

	if err := WriteSidecar(dir, "database", PerProjectManifest{
		Databases: []Database{{Name: "primary", Engine: EnginePostgres}},
	}); err != nil {
		t.Fatalf("database write: %v", err)
	}
	if err := WriteSidecar(dir, "storage", PerProjectManifest{
		Storage: []StorageBucket{{Name: "uploads"}},
	}); err != nil {
		t.Fatalf("storage write: %v", err)
	}

	// Each producer owns its own file; no read-modify-write merge happens.
	for _, slug := range []string{"database", "storage"} {
		if _, err := os.Stat(SidecarPath(dir, slug)); err != nil {
			t.Errorf("%s sidecar missing: %v", slug, err)
		}
	}
}

func TestWriteSidecarRejectsInvalidSlug(t *testing.T) {
	dir := t.TempDir()
	for _, slug := range []string{"", "Database", "with space", "../escape", "/leading"} {
		if err := WriteSidecar(dir, slug, PerProjectManifest{
			Storage: []StorageBucket{{Name: "x"}},
		}); err == nil {
			t.Errorf("WriteSidecar accepted invalid slug %q", slug)
		}
	}
}

func TestRemoveSidecarMissingFileIsNoOp(t *testing.T) {
	dir := t.TempDir()
	if err := RemoveSidecar(dir, "events"); err != nil {
		t.Fatalf("RemoveSidecar on missing file: %v", err)
	}
}
