package database

import (
	"io/fs"
	"strings"

	"go.putnami.dev/protocol/infra"
	protocolmigration "go.putnami.dev/protocol/migration"

	"go.putnami.dev/migration"
)

// Datasource fully describes the database a SQL migration source targets. It
// decouples two concerns the framework used to conflate:
//
//   - Name is the logical database the source migrates against. It matches the
//     pool's Database and is the scope for a migration's canonical id and the
//     bundle operation target. Empty resolves to the canonical default
//     datasource at registration time.
//   - Schema is the schema the source's SQL actually creates its objects in.
//     It is declared explicitly rather than inferred from the migration
//     namespace — the namespace is the source's identity (it prefixes every
//     migration name and the state-store id), almost never the schema name — so
//     the emitted infra/requirements.json names schemas that exist. The runner
//     also applies Schema as the transaction-local search_path before each
//     migration body, so a source's unqualified DDL lands in it and one
//     migration set can target many schemas via distinct Datasource pairs.
//     Schema never participates in a migration's canonical id or the bundle
//     digest — it is a deploy-time/runtime concern, not a payload one.
//
// One schema per datasource matches the one-schema-per-feature reality; a
// feature that owns a second schema declares a second source with a second
// Datasource.
type Datasource struct {
	Name   string
	Schema string
}

// SQLSource is the SQL flavor of migration.Source. A single source may
// carry an embed.FS of paired .up.sql/.down.sql files, a slice of inline
// Definitions, or both — both stack into the same registered set under
// (namespace, datasource). Multiple SQLSources for the same datasource
// merge cleanly; deduplication by name is enforced by the loader at
// Apply time.
type SQLSource struct {
	namespace  string
	datasource string
	schema     string
	fsys       fs.FS
	inline     []Definition
}

// NewSQLSource constructs an SQL migration source.
//
//   - namespace is the feature plugin's identifier (typically its
//     Plugin.Name()); it is used as the basename prefix for every
//     migration registered through this source and shows up in
//     diagnostics and the state-store id. It is identity, not a schema name.
//   - ds describes the target database: ds.Name is the logical DB (empty
//     resolves to the canonical default datasource) and ds.Schema is the
//     schema the source owns — declared explicitly so the emitted infra
//     requirements are correct, never guessed from the namespace.
//   - fsys is an optional embed.FS rooted on the feature's migrations
//     directory; the loader walks it recursively for paired
//     .up.sql/.down.sql files.
//   - inline lets a plugin stack code-defined migrations alongside the
//     embedded SQL — useful for tests and rare one-off boot data that
//     does not belong in a .sql file.
func NewSQLSource(namespace string, ds Datasource, fsys fs.FS, inline ...Definition) SQLSource {
	return SQLSource{
		namespace:  namespace,
		datasource: resolveDatasource(ds.Name, ""),
		schema:     strings.TrimSpace(ds.Schema),
		fsys:       fsys,
		inline:     append([]Definition(nil), inline...),
	}
}

// Kind reports migration.KindSQL.
func (s SQLSource) Kind() migration.Kind { return migration.KindSQL }

// Namespace returns the contributing plugin's identifier.
func (s SQLSource) Namespace() string { return s.namespace }

// Datasource returns the logical DB this source targets.
func (s SQLSource) Datasource() string { return s.datasource }

// Schema returns the schema this source owns, as declared on its Datasource.
// Empty when the source declares none.
func (s SQLSource) Schema() string { return s.schema }

// WithSchema returns a copy of the source with its declared schema replaced.
// The test provider uses WithSchema("") to neutralize the schema before
// applying: it owns the search_path through the per-suite isolated pool, so the
// runner must not override it with the source's declared schema. A trimmed,
// non-empty value re-targets the source at a different schema.
func (s SQLSource) WithSchema(schema string) SQLSource {
	s.schema = strings.TrimSpace(schema)
	return s
}

// FS returns the embedded filesystem the loader walks, if any.
func (s SQLSource) FS() fs.FS { return s.fsys }

// Inline returns the inline definitions stacked on this source. The
// returned slice is a copy.
func (s SQLSource) Inline() []Definition {
	if len(s.inline) == 0 {
		return nil
	}
	out := make([]Definition, len(s.inline))
	copy(out, s.inline)
	return out
}

// InfraDatabases implements migration.SchemaContributor. A SQL source maps to
// one Postgres database (its datasource) carrying the single schema it owns.
// Declaring the schema here — rather than inferring it from the namespace — is
// what lets the emitted infra/requirements.json name schemas that exist. The
// migration registry folds these across sources during the build's describe
// phase, walking the contributed sources directly, so emission needs no
// per-workload describe wiring and does not depend on the runtime persistence
// backend.
func (s SQLSource) InfraDatabases() []infra.Database {
	db := infra.Database{Name: s.datasource, Engine: infra.EnginePostgres}
	if s.schema != "" {
		db.Schemas = []string{s.schema}
	}
	return []infra.Database{db}
}

// Compile-time interface satisfaction checks. A SQL source is a migration
// source, declares its infra footprint (schema), and contributes to a build
// bundle on its own — the latter so the bundle describer can emit straight from
// sources when no runner is registered.
var (
	_ migration.Source                    = SQLSource{}
	_ migration.SchemaContributor         = SQLSource{}
	_ protocolmigration.BundleContributor = SQLSource{}
)

// canonicalNamespace returns the namespace the loader uses as the
// basename prefix; falls back to the protocol's default datasource
// label when empty, which keeps Definition.Name well-formed even for
// degenerate sources (mostly relevant in tests).
func (s SQLSource) canonicalNamespace() string {
	if s.namespace != "" {
		return s.namespace
	}
	return protocolmigration.DefaultDatasource
}
