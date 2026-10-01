package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/migration"
	"go.putnami.dev/protocol/features/spectest"
	protomig "go.putnami.dev/protocol/migration"
)

// bundleStubRunner is a migration.Runner that also contributes a fixed bundle
// operation + payload, used to exercise the describe-phase bundle emission
// without a database.
type bundleStubRunner struct{}

func (bundleStubRunner) Kind() migration.Kind { return migration.KindSQL }
func (bundleStubRunner) Apply(context.Context, migration.ApplyOpts) ([]migration.Record, error) {
	return nil, nil
}
func (bundleStubRunner) Status(context.Context) ([]migration.Record, error) { return nil, nil }
func (bundleStubRunner) Rollback(context.Context, migration.RollbackOpts) ([]migration.Record, error) {
	return nil, nil
}
func (bundleStubRunner) Verify(context.Context) (migration.DriftReport, error) {
	return migration.DriftReport{}, nil
}

func (bundleStubRunner) MigrationBundleOperations() ([]protomig.BundleOperation, []protomig.BundlePayload, error) {
	up := []byte("CREATE TABLE app.t (id int);\n")
	upPath := "payload/sql/default/app/0001_init.up.sql"
	return []protomig.BundleOperation{{
			Kind:   protomig.KindSQL,
			Target: "default",
			Name:   "app/0001_init",
			Up:     protomig.PayloadRef{Path: upPath, Hash: protomig.ComputePayloadHash(up)},
		}},
		[]protomig.BundlePayload{{Path: upPath, Bytes: up}},
		nil
}

func TestDescribeMigrationBundle_WritesArtifact(t *testing.T) {
	reg := migration.NewRegistry()
	if err := reg.RegisterRunner(bundleStubRunner{}); err != nil {
		t.Fatal(err)
	}

	a := New("demo")
	a.migrationRegistry = reg

	out := t.TempDir()
	if err := a.describeMigrationBundle(&DescribeContext{OutputDir: out}); err != nil {
		t.Fatalf("describeMigrationBundle: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(out, "migration-bundle", protomig.BundleFileName))
	if err != nil {
		t.Fatalf("expected bundle.json to be written: %v", err)
	}
	bundle, diags := protomig.ParseAndValidateBundle(data)
	for _, d := range diags {
		if d.Severity == "error" {
			t.Fatalf("emitted bundle is invalid: %v", diags)
		}
	}
	if bundle.AppName != "demo" {
		t.Errorf("appName = %q, want demo", bundle.AppName)
	}
	if bundle.Digest == "" {
		t.Error("emitted bundle must carry a digest")
	}
}

func TestDescribeMigrationBundle_UsesBuildProjectIdentity(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "describe", "describe-migration-project-identity")
	for _, tc := range []struct{ mode, project, want string }{
		{"all", "data/workloads/data-api", "data/workloads/data-api"},
		{"all", "", "data-api"},
		{"", "go/framework/app", "data-api"},
	} {
		t.Run(tc.mode+"/"+tc.project, func(t *testing.T) {
			t.Setenv(describeEnvVar, tc.mode)
			t.Setenv("PUTNAMI_PROJECT_NAME", tc.project)
			a := New("data-api")
			a.migrationRegistry = migration.NewRegistry()
			if err := a.migrationRegistry.RegisterRunner(bundleStubRunner{}); err != nil {
				t.Fatal(err)
			}
			out := t.TempDir()
			if err := a.describeMigrationBundle(&DescribeContext{OutputDir: out}); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(out, "migration-bundle", protomig.BundleFileName))
			if err != nil {
				t.Fatal(err)
			}
			bundle, diagnostics := protomig.ParseAndValidateBundle(raw)
			if bundle == nil || bundle.AppName != tc.want {
				t.Fatalf("bundle identity = %+v, want %q; diagnostics: %v", bundle, tc.want, diagnostics)
			}
		})
	}
}

func TestDescribeMigrationBundle_NoMigrationsNoArtifact(t *testing.T) {
	a := New("demo")
	a.migrationRegistry = migration.NewRegistry()

	out := t.TempDir()
	stale := filepath.Join(out, "migration-bundle")
	if err := os.MkdirAll(stale, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, protomig.BundleFileName), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := a.describeMigrationBundle(&DescribeContext{OutputDir: out}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "migration-bundle")); !os.IsNotExist(err) {
		t.Fatal("apps with no bundle migrations must not leave an artifact")
	}
}

// schemaStubSource is a SQL source that contributes one bundle operation on
// the "marketing" datasource in the given schema.
type schemaStubSource struct{ namespace, schema string }

func (schemaStubSource) Kind() migration.Kind { return migration.KindSQL }
func (s schemaStubSource) Namespace() string  { return s.namespace }
func (s schemaStubSource) MigrationBundleOperations() ([]protomig.BundleOperation, []protomig.BundlePayload, error) {
	up := []byte("CREATE TABLE t (id int);\n")
	upPath := "payload/sql/marketing/" + s.namespace + "/0001_init.up.sql"
	return []protomig.BundleOperation{{
			Kind:      protomig.KindSQL,
			Target:    "marketing",
			Schema:    s.schema,
			Namespace: s.namespace,
			Name:      s.namespace + "/0001_init",
			Up:        protomig.PayloadRef{Path: upPath, Hash: protomig.ComputePayloadHash(up)},
		}},
		[]protomig.BundlePayload{{Path: upPath, Bytes: up}},
		nil
}

func TestDescribeMigrationBundle_RejectsOneDatasourceInTwoSchemas(t *testing.T) {
	reg := migration.NewRegistry()
	for _, s := range []schemaStubSource{{"app", "marketing"}, {"putnami-analytics", "public"}} {
		if err := reg.AddSource(s); err != nil {
			t.Fatal(err)
		}
	}
	a := New("demo")
	a.migrationRegistry = reg

	out := t.TempDir()
	// A bundle an earlier build wrote, which the refused build must not leave.
	stale := filepath.Join(out, "migration-bundle", "bundle.json")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := a.describeMigrationBundle(&DescribeContext{OutputDir: out})
	var conflict *migration.DatasourceSchemaConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected *migration.DatasourceSchemaConflictError, got %v", err)
	}
	const want = `datasource "marketing" has conflicting schemas across sources: "marketing" and "public" (namespaces "app" and "putnami-analytics")`
	if err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
	if _, statErr := os.Stat(filepath.Join(out, "migration-bundle")); !os.IsNotExist(statErr) {
		t.Fatalf("a refused bundle must not be left on disk, stat err = %v", statErr)
	}
}
