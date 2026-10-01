package migration

import (
	"fmt"

	protocolmigration "go.putnami.dev/protocol/migration"
)

// DatasourceSchemaClaim is one source's statement that its migrations on Datasource run
// in Schema. An empty Schema states nothing: the source follows whatever
// search_path the connection already has.
type DatasourceSchemaClaim struct {
	Datasource string
	Schema     string
	Namespace  string
}

// DatasourceSchemaConflictError reports two sources that put one datasource in two
// schemas. The runner applies a datasource's schema as the search_path of
// every migration on it, so the path would be ambiguous and the shared state
// store could not tell the two apart.
type DatasourceSchemaConflictError struct {
	Datasource string
	Schemas    [2]string
	Namespaces [2]string
}

func (e *DatasourceSchemaConflictError) Error() string {
	return fmt.Sprintf("datasource %q has conflicting schemas across sources: %q and %q (namespaces %q and %q)",
		e.Datasource, e.Schemas[0], e.Schemas[1], e.Namespaces[0], e.Namespaces[1])
}

// ResolveDatasourceSchemas resolves each datasource's owning schema from the claims
// of its sources, and returns a *DatasourceSchemaConflictError when two claims name
// different schemas for the same datasource.
//
// It is the one rule both the SQL runner (at apply time) and the bundle
// describer (at build time) enforce, so a workload that cannot migrate fails
// `putnami build` with the message it would otherwise meet at apply. Mirrors
// TypeScript's resolveDatasourceSchemas in @putnami/migration.
func ResolveDatasourceSchemas(claims []DatasourceSchemaClaim) (map[string]string, error) {
	owners := make(map[string]DatasourceSchemaClaim)
	for _, c := range claims {
		if c.Schema == "" {
			continue
		}
		first, seen := owners[c.Datasource]
		if !seen {
			owners[c.Datasource] = c
			continue
		}
		if first.Schema != c.Schema {
			return nil, &DatasourceSchemaConflictError{
				Datasource: c.Datasource,
				Schemas:    [2]string{first.Schema, c.Schema},
				Namespaces: [2]string{first.Namespace, c.Namespace},
			}
		}
	}
	schemas := make(map[string]string, len(owners))
	for datasource, c := range owners {
		schemas[datasource] = c.Schema
	}
	return schemas, nil
}

// CheckBundleSchemas runs ResolveDatasourceSchemas over each kind's bundle
// operations, so a bundle the runner would refuse is never written.
func CheckBundleSchemas(ops []protocolmigration.BundleOperation) error {
	byKind := make(map[protocolmigration.OperationKind][]DatasourceSchemaClaim)
	var kinds []protocolmigration.OperationKind
	for _, op := range ops {
		kind := op.Kind
		if _, seen := byKind[kind]; !seen {
			kinds = append(kinds, kind)
		}
		target := op.Target
		if target == "" {
			target = protocolmigration.DefaultDatasource
		}
		byKind[kind] = append(byKind[kind], DatasourceSchemaClaim{Datasource: target, Schema: op.Schema, Namespace: op.Namespace})
	}
	for _, kind := range kinds {
		if _, err := ResolveDatasourceSchemas(byKind[kind]); err != nil {
			return err
		}
	}
	return nil
}
