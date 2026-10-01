package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go.putnami.dev/migration"
	"go.putnami.dev/protocol/infra"
)

// describeContribPlugin is a minimal contributor for the describer test.
type describeContribPlugin struct {
	name    string
	sources []migration.Source
}

func (p *describeContribPlugin) Name() string                         { return p.name }
func (p *describeContribPlugin) MigrationSources() []migration.Source { return p.sources }

type describeFakeSource struct {
	kind migration.Kind
	ns   string
}

func (s describeFakeSource) Kind() migration.Kind { return s.kind }
func (s describeFakeSource) Namespace() string    { return s.ns }

func TestDescribe_EmitsMigrationsJSON(t *testing.T) {
	a := New("describe-migrations")
	a.Use(&describeContribPlugin{
		name: "iam",
		sources: []migration.Source{
			describeFakeSource{kind: migration.KindSQL, ns: "iam"},
		},
	})
	a.Use(&describeContribPlugin{
		name: "secrets",
		sources: []migration.Source{
			describeFakeSource{kind: migration.KindSQL, ns: "secrets"},
			describeFakeSource{kind: "gcs", ns: "secrets"},
		},
	})

	outDir := t.TempDir()
	// Reusing a namespace across migration kinds is supported: the registry and
	// emitter scope migration provider identity by (kind, namespace).
	if err := a.Describe(outDir, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	bytes, err := os.ReadFile(filepath.Join(outDir, "migrations.json"))
	if err != nil {
		t.Fatalf("read migrations.json: %v", err)
	}

	var view struct {
		Kinds []struct {
			Kind             string `json:"kind"`
			RunnerRegistered bool   `json:"runnerRegistered"`
			Sources          []struct {
				Namespace string `json:"namespace"`
			} `json:"sources"`
		} `json:"kinds"`
	}
	if err := json.Unmarshal(bytes, &view); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, bytes)
	}

	// Kinds are lexicographically sorted.
	if len(view.Kinds) != 2 {
		t.Fatalf("expected 2 kinds, got %d (%+v)", len(view.Kinds), view.Kinds)
	}
	if view.Kinds[0].Kind != "gcs" || view.Kinds[1].Kind != "sql" {
		t.Errorf("kinds out of lexicographic order: %v", view.Kinds)
	}
	if view.Kinds[1].RunnerRegistered {
		t.Error("no runners registered in this test — RunnerRegistered should be false")
	}
	// SQL has two sources (iam + secrets).
	if len(view.Kinds[1].Sources) != 2 {
		t.Errorf("expected 2 SQL sources, got %d (%+v)", len(view.Kinds[1].Sources), view.Kinds[1].Sources)
	}
}

// describeSchemaSource declares an infra database, satisfying both
// migration.Source and migration.SchemaContributor.
type describeSchemaSource struct {
	kind migration.Kind
	ns   string
	dbs  []infra.Database
}

func (s describeSchemaSource) Kind() migration.Kind             { return s.kind }
func (s describeSchemaSource) Namespace() string                { return s.ns }
func (s describeSchemaSource) InfraDatabases() []infra.Database { return s.dbs }

func TestDescribe_EmitsInfraRequirementsSidecar(t *testing.T) {
	a := New("describe-infra")
	a.Use(&describeContribPlugin{
		name: "iam",
		sources: []migration.Source{
			describeSchemaSource{
				kind: migration.KindSQL,
				ns:   "iam",
				dbs:  []infra.Database{{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"iam"}}},
			},
		},
	})

	outDir := t.TempDir()
	if err := a.Describe(outDir, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	path := infra.SidecarPathIn(outDir, "migration")
	loaded, diags := infra.LoadPerProjectManifest(path)
	if len(diags) != 0 {
		t.Fatalf("emitted sidecar failed to load: %v", diags)
	}
	want := []infra.Database{{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"iam"}}}
	if !reflect.DeepEqual(loaded.Databases, want) {
		t.Fatalf("databases = %#v, want %#v", loaded.Databases, want)
	}
}

func TestDescribe_SkipsInfraRequirementsWhenNoSchemas(t *testing.T) {
	a := New("describe-infra-empty")
	// A source with no schema declaration must not produce a sidecar.
	a.Use(&describeContribPlugin{
		name: "iam",
		sources: []migration.Source{
			describeFakeSource{kind: migration.KindSQL, ns: "iam"},
		},
	})

	outDir := t.TempDir()
	if err := a.Describe(outDir, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	path := infra.SidecarPathIn(outDir, "migration")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("infra sidecar should not exist when no schemas are declared: stat err=%v", err)
	}
}

func TestDescribe_SkipsMigrationsJSONWhenNoSources(t *testing.T) {
	a := New("describe-no-sources")
	// No contributors → no migrations.json file should appear.

	outDir := t.TempDir()
	if err := a.Describe(outDir, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	path := filepath.Join(outDir, "migrations.json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("migrations.json should not exist when no sources are contributed: stat err=%v", err)
	}
}

// The Go extension declares <project>/.gen/migrations.json as a build-describe
// output (go/extension ADR 0005), so its presence has to be a function of what
// the current sources contribute. An app that stops contributing migrations must
// have the earlier view removed, or describe's cache entry would carry a view
// this run never produced and every later hit would restore it.
func TestDescribe_RemovesAStaleMigrationsJSONWhenNoSourcesRemain(t *testing.T) {
	outDir := t.TempDir()
	path := filepath.Join(outDir, "migrations.json")

	contributing := New("describe-had-sources")
	contributing.Use(&describeContribPlugin{
		name: "iam",
		sources: []migration.Source{
			describeFakeSource{kind: migration.KindSQL, ns: "iam"},
		},
	})
	if err := contributing.Describe(outDir, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("migrations.json must exist while a source is contributed: %v", err)
	}

	// The same project after its last migration source is removed.
	if err := New("describe-had-sources").Describe(outDir, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a stale migrations.json survived a run that contributes none: stat err=%v", err)
	}
}

func TestDescribe_RespectsTargetFilter(t *testing.T) {
	a := New("describe-target-filter")
	a.Use(&describeContribPlugin{
		name: "iam",
		sources: []migration.Source{
			describeFakeSource{kind: migration.KindSQL, ns: "iam"},
		},
	})

	outDir := t.TempDir()
	// Filter to "proto" → migrations describer must NOT run.
	if err := a.Describe(outDir, []string{"proto"}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "migrations.json")); !os.IsNotExist(err) {
		t.Fatalf("migrations.json should NOT be written when targets=[proto]")
	}

	// Filter to "migrations" → file must appear.
	if err := a.Describe(outDir, []string{"migrations"}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "migrations.json")); err != nil {
		t.Fatalf("migrations.json must be written when targets=[migrations]: %v", err)
	}
}
