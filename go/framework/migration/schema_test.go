package migration

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	protocolmigration "go.putnami.dev/protocol/migration"
)

func TestDatasourceSchemas_MapsEachDatasourceToItsDeclaredSchema(t *testing.T) {
	schemas, err := ResolveDatasourceSchemas([]DatasourceSchemaClaim{
		{Datasource: "marketing", Schema: "marketing", Namespace: "app"},
		{Datasource: "marketing", Namespace: "legacy"},
		{Datasource: "marketing", Schema: "marketing", Namespace: "putnami-analytics"},
		{Datasource: "identity", Schema: "identity_auth", Namespace: "iam"},
		{Datasource: "default", Namespace: "orphan"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"marketing": "marketing", "identity": "identity_auth"}
	if !reflect.DeepEqual(schemas, want) {
		t.Fatalf("schemas = %v, want %v", schemas, want)
	}
}

func TestDatasourceSchemas_RejectsOneDatasourceInTwoSchemas(t *testing.T) {
	_, err := ResolveDatasourceSchemas([]DatasourceSchemaClaim{
		{Datasource: "marketing", Schema: "marketing", Namespace: "app"},
		{Datasource: "marketing", Schema: "public", Namespace: "putnami-analytics"},
	})
	var conflict *DatasourceSchemaConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected *DatasourceSchemaConflictError, got %v", err)
	}
	const want = `datasource "marketing" has conflicting schemas across sources: "marketing" and "public" (namespaces "app" and "putnami-analytics")`
	if err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
}

func TestCheckBundleSchemas_ChecksEachKindSeparately(t *testing.T) {
	op := func(kind protocolmigration.OperationKind, target, schema, namespace string) protocolmigration.BundleOperation {
		return protocolmigration.BundleOperation{Kind: kind, Target: target, Schema: schema, Namespace: namespace}
	}
	// Another kind on the same target does not share the SQL search path, and
	// an empty target is the default datasource.
	agreeing := []protocolmigration.BundleOperation{
		op(protocolmigration.KindSQL, "", "app", "app"),
		op(protocolmigration.KindSQL, "default", "app", "billing"),
		op(protocolmigration.KindDocument, "default", "docs", "docs"),
	}
	if err := CheckBundleSchemas(agreeing); err != nil {
		t.Fatalf("CheckBundleSchemas: %v", err)
	}

	conflicting := append(slices.Clone(agreeing), op(protocolmigration.KindSQL, "default", "public", "putnami-analytics"))
	var conflict *DatasourceSchemaConflictError
	if err := CheckBundleSchemas(conflicting); !errors.As(err, &conflict) {
		t.Fatalf("expected *DatasourceSchemaConflictError, got %v", err)
	}
	if conflict.Datasource != "default" || conflict.Namespaces != [2]string{"app", "putnami-analytics"} {
		t.Fatalf("conflict = %+v", conflict)
	}
}
