package database

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go.putnami.dev/app"
	"go.putnami.dev/protocol/infra"
)

// TestPluginDescribeOnlyViaApp exercises the describe-only registration path:
// a database plugin registered with app.UseForDescribe emits its infra scratch
// fragment during the describe phase without being configured or started — so
// no pool is opened and nothing connects. The manifest is derived from the pool
// config alone (engine, database, explicit schemas); migration-attributed
// schemas require the runtime owner and are intentionally absent here.
func TestPluginDescribeOnlyViaApp(t *testing.T) {
	out := t.TempDir()
	a := app.New("svc")
	a.UseForDescribe(NewPlugin(PluginConfig{
		Pool: PoolConfig{Database: "primary", Schemas: []string{"app"}},
	}))

	if err := a.Describe(out, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	manifest, diags := infra.LoadPerProjectManifest(infra.SidecarPathIn(out, "database"))
	for _, d := range diags {
		if d.Severity == "error" {
			t.Fatalf("loaded sidecar has errors: %v", diags)
		}
	}
	if manifest == nil || len(manifest.Databases) != 1 {
		t.Fatalf("expected 1 database in sidecar, got %+v", manifest)
	}
	db := manifest.Databases[0]
	if db.Name != "primary" || db.Engine != infra.EnginePostgres {
		t.Errorf("database = %+v, want primary/postgres", db)
	}
	if !reflect.DeepEqual(db.Schemas, []string{"app"}) {
		t.Errorf("schemas = %v, want [app]", db.Schemas)
	}
}

// TestDescribeWritesSidecarWhereGeneratorSyncReads guards the describe→sync
// contract between describe output and generator sync. The build CLI runs the describe phase with
// DescribeContext.OutputDir set to the workload's ".gen" directory (and
// ListenAndServe's fallback uses "<cwd>/.gen"), while the generator sync reads
// each producer's sidecar from infra.SidecarPath(projectRoot, slug) ==
// "<projectRoot>/.gen/infra/<slug>.json". The describer must land its scratch
// fragment at exactly that path.
//
// The regression it catches: passing OutputDir (already ".gen") to the
// project-root WriteSidecar nested the file at "<projectRoot>/.gen/.gen/infra/",
// one ".gen" too deep, so generator sync silently dropped the contribution and
// infra/requirements.json came back empty.
func TestDescribeWritesSidecarWhereGeneratorSyncReads(t *testing.T) {
	projectRoot := t.TempDir()
	// Mirror the build CLI / ListenAndServe fallback: OutputDir is "<root>/.gen".
	outputDir := filepath.Join(projectRoot, infra.AggregatedManifestDir)

	a := app.New("svc")
	a.UseForDescribe(NewPlugin(PluginConfig{
		Pool: PoolConfig{Database: "primary", Schemas: []string{"app"}},
	}))
	if err := a.Describe(outputDir, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	// The generator sync globs "<projectRoot>/.gen/infra/*.json" — exactly
	// infra.SidecarPath(projectRoot, slug). The scratch fragment must be there.
	if _, err := os.Stat(infra.SidecarPath(projectRoot, "database")); err != nil {
		t.Fatalf("sidecar not at generator-visible path %s: %v",
			infra.SidecarPath(projectRoot, "database"), err)
	}

	// And it must not be nested one ".gen" too deep.
	doubled := filepath.Join(outputDir, infra.AggregatedManifestDir, infra.PerProjectManifestDir, "database.json")
	if _, err := os.Stat(doubled); !os.IsNotExist(err) {
		t.Fatalf("sidecar written one .gen too deep at %s (stat err=%v)", doubled, err)
	}
}
