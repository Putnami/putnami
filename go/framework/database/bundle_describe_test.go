package database

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/app"
	"go.putnami.dev/migration"
	protocaps "go.putnami.dev/protocol/capabilities"
	"go.putnami.dev/protocol/infra"
	protocolmigration "go.putnami.dev/protocol/migration"
)

// bundleContrib is a minimal app.MigrationContributor wrapping pre-built sources.
type bundleContrib struct {
	name    string
	sources []migration.Source
}

func (p *bundleContrib) Name() string                         { return p.name }
func (p *bundleContrib) MigrationSources() []migration.Source { return p.sources }

func writeCapabilityVersionStamp(t *testing.T, out string) {
	t.Helper()
	binding, err := protocaps.ComputeSourceBinding([]protocaps.SourceBindingFile{{
		Path: "bundle_describe_test.go", Mode: protocaps.SourceModeRegular,
		Digest: protocaps.SourceDigest([]byte("database capability test")),
	}})
	if err != nil {
		t.Fatal(err)
	}
	stamp := map[string]any{
		"name": "go.putnami.dev/database",
		"capabilityPackages": []map[string]any{{
			"package": "go.putnami.dev/database", "version": "0.1.0",
			"sourceRoot": "go/framework/database", "evidencePath": "go/framework/database/putnami.json",
			"sourceBinding": binding,
		}},
	}
	data, err := json.Marshal(stamp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "version.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDescribe_EmitsBundleAndInfraFromSourcesWithoutRunner is the Gap A
// regression: `putnami build` emits both the migration bundle and the infra
// requirements straight from contributed SQL sources, with no SQL runner
// registered and no configured pool. Before, the bundle describer consulted
// only runners (so a workload had to a.Use the database plugin and flip every
// feature to the Postgres backend just to populate the artifact) and the infra
// schemas were attributed via the configured pool. Now a SQLSource contributes
// its bundle operations and its (datasource, schema) on its own, independent of
// the runtime persistence backend.
func TestDescribe_EmitsBundleAndInfraFromSourcesWithoutRunner(t *testing.T) {
	a := app.New("svc")
	a.Use(&bundleContrib{
		name: "iam",
		sources: []migration.Source{
			NewSQLSource("iam", Datasource{Name: "default", Schema: "public"}, nil,
				Definition{
					Name: "0001_create_users",
					SQL:  "CREATE TABLE iam_users (id text primary key);",
					Down: "DROP TABLE iam_users;",
				},
			),
		},
	})

	out := t.TempDir()
	writeCapabilityVersionStamp(t, out)
	if err := a.Describe(out, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	// 1) The bundle was emitted from the source — no runner was ever registered.
	bundleDir := filepath.Join(out, "migration-bundle")
	bundle, _, diags := protocolmigration.LoadBundle(os.DirFS(bundleDir))
	for _, d := range diags {
		if d.Severity == "error" {
			t.Fatalf("emitted bundle failed to load: %v", diags)
		}
	}
	if len(bundle.Operations) != 1 {
		t.Fatalf("expected 1 bundle operation, got %d", len(bundle.Operations))
	}
	op := bundle.Operations[0]
	if op.Name != "iam/0001_create_users" {
		t.Errorf("operation name = %q, want iam/0001_create_users", op.Name)
	}
	if op.Target != protocolmigration.DefaultDatasource {
		t.Errorf("operation target = %q, want %q", op.Target, protocolmigration.DefaultDatasource)
	}

	// 2) The infra fragment names the declared schema ("public"), NOT the
	//    migration namespace ("iam") — the schema/identity decoupling.
	loaded, idiags := infra.LoadPerProjectManifest(infra.SidecarPathIn(out, "migration"))
	for _, d := range idiags {
		if d.Severity == "error" {
			t.Fatalf("emitted infra sidecar failed to load: %v", idiags)
		}
	}
	want := []infra.Database{{Name: "default", Engine: infra.EnginePostgres, Schemas: []string{"public"}}}
	if !reflect.DeepEqual(loaded.Databases, want) {
		t.Fatalf("infra databases = %#v, want %#v", loaded.Databases, want)
	}
}

// TestDescribe_MergesSameNamespaceDatasourceSQLSourcesInCapabilities proves
// that independently-owned SQL fragments can share a migration namespace and
// datasource. The runtime merges their definitions, so describe must publish
// one capability provider and source discoverer while retaining both migration
// IDs in the executable bundle.
func TestDescribe_MergesSameNamespaceDatasourceSQLSourcesInCapabilities(t *testing.T) {
	a := app.New("registry")
	a.Use(&bundleContrib{
		name: "registry-features-a",
		sources: []migration.Source{
			NewSQLSource("registry", Datasource{Name: "registry", Schema: "registry"}, nil,
				Definition{Name: "002_feature_a", SQL: "CREATE TABLE feature_a (id text primary key);"},
			),
		},
	})
	a.Use(&bundleContrib{
		name: "registry-features-b",
		sources: []migration.Source{
			NewSQLSource("registry", Datasource{Name: "registry", Schema: "registry"}, nil,
				Definition{Name: "003_feature_b", SQL: "CREATE TABLE feature_b (id text primary key);"},
			),
		},
	})

	out := t.TempDir()
	writeCapabilityVersionStamp(t, out)
	if err := a.Describe(out, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(out, "schema", "capabilities.json"))
	if err != nil {
		t.Fatalf("read capabilities manifest: %v", err)
	}
	var manifest struct {
		Discoverers []struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"discoverers"`
		Migrations []struct {
			Name       string `json:"name"`
			Datasource string `json:"datasource"`
		} `json:"migrations"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("unmarshal capabilities manifest: %v", err)
	}
	if len(manifest.Migrations) != 1 || manifest.Migrations[0].Name != "registry" || manifest.Migrations[0].Datasource != "registry" {
		t.Fatalf("migration bundles = %#v, want one registry bundle for datasource registry", manifest.Migrations)
	}
	if len(manifest.Discoverers) != 1 || manifest.Discoverers[0].Kind != "source" || manifest.Discoverers[0].Name != "sql:registry" {
		t.Fatalf("discoverers = %#v, want one source:sql:registry discoverer", manifest.Discoverers)
	}

	bundle, _, diags := protocolmigration.LoadBundle(os.DirFS(filepath.Join(out, "migration-bundle")))
	for _, d := range diags {
		if d.Severity == "error" {
			t.Fatalf("emitted bundle failed to load: %v", diags)
		}
	}
	got := make(map[string]bool, len(bundle.Operations))
	for _, op := range bundle.Operations {
		got[op.Name] = true
	}
	for _, name := range []string{"registry/002_feature_a", "registry/003_feature_b"} {
		if !got[name] {
			t.Errorf("bundle operations = %#v, missing preserved migration ID %q", bundle.Operations, name)
		}
	}
}

// conflictingSchemaApp composes two plugins that put the "registry" datasource
// in two schemas.
func conflictingSchemaApp() *app.Application {
	a := app.New("registry")
	a.Use(&bundleContrib{
		name: "registry-public",
		sources: []migration.Source{
			NewSQLSource("registry", Datasource{Name: "registry", Schema: "public"}, nil,
				Definition{Name: "002_feature_a", SQL: "CREATE TABLE feature_a (id text primary key);"},
			),
		},
	})
	a.Use(&bundleContrib{
		name: "registry-private",
		sources: []migration.Source{
			NewSQLSource("registry", Datasource{Name: "registry", Schema: "private"}, nil,
				Definition{Name: "003_feature_b", SQL: "CREATE TABLE feature_b (id text primary key);"},
			),
		},
	})
	return a
}

// The bundle describer refuses the conflict with the runner's message, before
// anything is written, the infra fragment included: the runner would refuse
// the bundle at apply time.
func TestDescribe_FailsOnConflictingSQLSourceSchemas(t *testing.T) {
	out := t.TempDir()
	writeCapabilityVersionStamp(t, out)
	err := conflictingSchemaApp().Describe(out, nil)
	const want = `datasource "registry" has conflicting schemas across sources: "public" and "private" (namespaces "registry" and "registry")`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Describe error = %v, want %q", err, want)
	}
	for _, path := range []string{
		filepath.Join(out, "migration-bundle"),
		filepath.Join(out, "schema", "capabilities.json"),
		infra.SidecarPathIn(out, "migration"),
	} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("%s must not be published; stat err = %v", path, statErr)
		}
	}
}

// Described alone, the capability manifest reports the same conflict as its
// own finding.
func TestDescribeCapabilities_FailsOnConflictingSQLSourceSchemas(t *testing.T) {
	out := t.TempDir()
	writeCapabilityVersionStamp(t, out)
	err := conflictingSchemaApp().Describe(out, []string{"capabilities"})
	if err == nil || !strings.Contains(err.Error(), "capabilities.conflicting_provider") {
		t.Fatalf("Describe error = %v, want capabilities.conflicting_provider", err)
	}
	path := filepath.Join(out, "schema", "capabilities.json")
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("invalid manifest must not be published; stat err = %v", statErr)
	}
}
