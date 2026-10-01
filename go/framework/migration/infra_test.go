package migration

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go.putnami.dev/protocol/infra"
)

// schemaSource is a test Source that declares infra databases.
type schemaSource struct {
	kind      Kind
	namespace string
	databases []infra.Database
}

func (s schemaSource) Kind() Kind                       { return s.kind }
func (s schemaSource) Namespace() string                { return s.namespace }
func (s schemaSource) InfraDatabases() []infra.Database { return s.databases }

// plainSource is a Source that does not declare infra databases.
type plainSource struct {
	kind      Kind
	namespace string
}

func (s plainSource) Kind() Kind        { return s.kind }
func (s plainSource) Namespace() string { return s.namespace }

func mustAdd(t *testing.T, r *Registry, sources ...Source) {
	t.Helper()
	for _, s := range sources {
		if err := r.AddSource(s); err != nil {
			t.Fatalf("AddSource(%s): %v", s.Namespace(), err)
		}
	}
}

func TestInfraRequirementsZeroSources(t *testing.T) {
	r := NewRegistry()
	got := r.InfraRequirements()
	if got.ProtocolVersion != infra.ProtocolVersion {
		t.Fatalf("protocolVersion = %d, want %d", got.ProtocolVersion, infra.ProtocolVersion)
	}
	if len(got.Databases) != 0 {
		t.Fatalf("expected no databases, got %v", got.Databases)
	}
}

func TestInfraRequirementsOneSource(t *testing.T) {
	r := NewRegistry()
	mustAdd(t, r, schemaSource{
		kind:      KindSQL,
		namespace: "iam",
		databases: []infra.Database{{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"iam"}}},
	})

	got := r.InfraRequirements().Databases
	want := []infra.Database{{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"iam"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("databases = %#v, want %#v", got, want)
	}
}

func TestInfraRequirementsMultiSchemaSameDatabase(t *testing.T) {
	r := NewRegistry()
	// Two sources targeting the same (name, engine) with different schemas,
	// added out of sorted order to prove the output is deterministic.
	mustAdd(t, r,
		schemaSource{
			kind:      KindSQL,
			namespace: "secrets",
			databases: []infra.Database{{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"secrets"}}},
		},
		schemaSource{
			kind:      KindSQL,
			namespace: "iam",
			databases: []infra.Database{{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"iam"}}},
		},
	)

	got := r.InfraRequirements().Databases
	want := []infra.Database{{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"iam", "secrets"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("databases = %#v, want %#v", got, want)
	}
}

func TestInfraRequirementsDeduplicatesSchemas(t *testing.T) {
	r := NewRegistry()
	mustAdd(t, r,
		schemaSource{
			kind:      KindSQL,
			namespace: "iam-a",
			databases: []infra.Database{{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"iam"}}},
		},
		schemaSource{
			kind:      KindSQL,
			namespace: "iam-b",
			databases: []infra.Database{{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"iam"}}},
		},
	)

	got := r.InfraRequirements().Databases
	want := []infra.Database{{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"iam"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("databases = %#v, want %#v", got, want)
	}
}

func TestInfraRequirementsCrossKind(t *testing.T) {
	const kindDocument Kind = "document"
	r := NewRegistry()
	mustAdd(t, r,
		schemaSource{
			kind:      KindSQL,
			namespace: "iam",
			databases: []infra.Database{{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"iam"}}},
		},
		schemaSource{
			kind:      kindDocument,
			namespace: "catalog",
			databases: []infra.Database{{Name: "primary", Engine: infra.EngineMySQL, Schemas: []string{"catalog"}}},
		},
	)

	got := r.InfraRequirements().Databases
	want := []infra.Database{
		{Name: "primary", Engine: infra.EngineMySQL, Schemas: []string{"catalog"}},
		{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"iam"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("databases = %#v, want %#v", got, want)
	}
}

func TestInfraRequirementsIgnoresNonContributingSources(t *testing.T) {
	r := NewRegistry()
	mustAdd(t, r,
		plainSource{kind: KindSQL, namespace: "no-schema"},
		schemaSource{
			kind:      KindSQL,
			namespace: "iam",
			databases: []infra.Database{{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"iam"}}},
		},
	)

	got := r.InfraRequirements().Databases
	want := []infra.Database{{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"iam"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("databases = %#v, want %#v", got, want)
	}
}

// The emitted manifest must round-trip through the protocol's strict parser
// and validator so the migration framework's contribution is always a valid
// per-project manifest the aggregator can consume.
func TestInfraRequirementsValidatesAsPerProjectManifest(t *testing.T) {
	r := NewRegistry()
	mustAdd(t, r, schemaSource{
		kind:      KindSQL,
		namespace: "iam",
		databases: []infra.Database{{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"iam"}}},
	})

	manifest := r.InfraRequirements()
	if diags := infra.ValidatePerProjectManifest(&manifest); len(diags) != 0 {
		t.Fatalf("manifest failed validation: %v", diags)
	}
}

func TestWriteInfraRequirementsEmitsSidecar(t *testing.T) {
	r := NewRegistry()
	mustAdd(t, r, schemaSource{
		kind:      KindSQL,
		namespace: "iam",
		databases: []infra.Database{{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"iam"}}},
	})

	outputDir := t.TempDir()
	if err := r.WriteInfraRequirements(outputDir); err != nil {
		t.Fatalf("WriteInfraRequirements: %v", err)
	}

	path := infra.SidecarPathIn(outputDir, "migration")
	loaded, diags := infra.LoadPerProjectManifest(path)
	if len(diags) != 0 {
		t.Fatalf("emitted sidecar failed to load: %v", diags)
	}
	want := []infra.Database{{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"iam"}}}
	if !reflect.DeepEqual(loaded.Databases, want) {
		t.Fatalf("databases = %#v, want %#v", loaded.Databases, want)
	}
}

func TestWriteInfraRequirementsSkipsWhenEmpty(t *testing.T) {
	r := NewRegistry()
	outputDir := t.TempDir()
	if err := r.WriteInfraRequirements(outputDir); err != nil {
		t.Fatalf("WriteInfraRequirements: %v", err)
	}
	path := infra.SidecarPathIn(outputDir, "migration")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no sidecar to be written, stat err = %v", err)
	}
}

func TestWriteInfraRequirementsRemovesStaleSidecar(t *testing.T) {
	outputDir := t.TempDir()
	path := infra.SidecarPathIn(outputDir, "migration")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"protocolVersion":1,"databases":[{"name":"primary","engine":"postgres"}]}`), 0o600); err != nil {
		t.Fatalf("seed stale sidecar: %v", err)
	}

	// A registry with no schema-declaring sources must clear the stale file.
	r := NewRegistry()
	if err := r.WriteInfraRequirements(outputDir); err != nil {
		t.Fatalf("WriteInfraRequirements: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected stale sidecar to be removed, stat err = %v", err)
	}
}
