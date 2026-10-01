package database

import (
	"path"

	"go.putnami.dev/migration"
	protocolmigration "go.putnami.dev/protocol/migration"
)

// MigrationBundleOperations implements protocolmigration.BundleContributor at
// the source level: it expands this one SQL source into bundle operations and
// materialized .up.sql/.down.sql payloads.
//
// Loading goes through the same loadSQLSource path the runner uses at apply
// time and reads only the authoring filesystem — no database connection is
// opened — so emission is a pure, reproducible build step. Payload hashes use
// the same sha256Hex the runner stamps on applied migrations, so a bundled
// migration and an applied one share one content hash.
//
// Contributing per-source (rather than only via the runner) is what lets the
// build emit a faithful bundle straight from the registered sources when no
// runner is registered — under app.UseForDescribe, or when the runtime backend
// is not Postgres so no SQL runner registered — without per-workload describe
// wiring.
func (s SQLSource) MigrationBundleOperations() ([]protocolmigration.BundleOperation, []protocolmigration.BundlePayload, error) {
	defs, err := loadSQLSource(s)
	if err != nil {
		return nil, nil, err
	}

	ops := make([]protocolmigration.BundleOperation, 0, len(defs))
	payloads := make([]protocolmigration.BundlePayload, 0, len(defs))
	for _, d := range defs {
		target := resolveDatasource(d.Datasource, "")
		upPath := bundleSQLPayloadPath(target, d.Name, upSuffix)

		op := protocolmigration.BundleOperation{
			Kind:         protocolmigration.KindSQL,
			Target:       target,
			Schema:       s.schema,
			Namespace:    d.Namespace,
			Name:         d.Name,
			OrderKey:     d.Name,
			Up:           protocolmigration.PayloadRef{Path: upPath, Hash: sha256Hex(d.SQL)},
			Safety:       protocolmigration.SafetySafeOnline,
			Capabilities: protocolmigration.Capabilities{Transactional: true},
		}
		payloads = append(payloads, protocolmigration.BundlePayload{Path: upPath, Bytes: []byte(d.SQL)})

		if d.Down != "" {
			downPath := bundleSQLPayloadPath(target, d.Name, downSuffix)
			op.Down = &protocolmigration.PayloadRef{Path: downPath, Hash: sha256Hex(d.Down)}
			op.Capabilities.Reversible = true
			payloads = append(payloads, protocolmigration.BundlePayload{Path: downPath, Bytes: []byte(d.Down)})
		}

		ops = append(ops, op)
	}
	return ops, payloads, nil
}

// MigrationBundleOperations implements protocolmigration.BundleContributor for
// the SQL runner by delegating to every contributed SQL source. Kept for
// callers that hold a runner; the describe phase also enumerates sources
// directly, so the two paths emit byte-identical operations.
func (r *SQLRunner) MigrationBundleOperations() ([]protocolmigration.BundleOperation, []protocolmigration.BundlePayload, error) {
	if r.registry == nil {
		return nil, nil, nil
	}

	var (
		ops      []protocolmigration.BundleOperation
		payloads []protocolmigration.BundlePayload
	)
	for _, src := range r.registry.Sources(migration.KindSQL) {
		s, ok := src.(SQLSource)
		if !ok {
			continue
		}
		sops, spayloads, err := s.MigrationBundleOperations()
		if err != nil {
			return nil, nil, err
		}
		ops = append(ops, sops...)
		payloads = append(payloads, spayloads...)
	}
	return ops, payloads, nil
}

// bundleSQLPayloadPath returns the canonical in-bundle path for a SQL payload:
// payload/sql/<target>/<namespace>/<basename>.{up,down}.sql. Name already
// carries the "<namespace>/<basename>" form stamped by the loader.
func bundleSQLPayloadPath(target, name, suffix string) string {
	return path.Join("payload", "sql", target, name) + suffix
}

// Compile-time checks: both the SQL runner and a single SQL source contribute
// to migration bundles.
var (
	_ protocolmigration.BundleContributor = (*SQLRunner)(nil)
	_ protocolmigration.BundleContributor = SQLSource{}
)
