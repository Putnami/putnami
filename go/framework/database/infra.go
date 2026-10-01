package database

import (
	"fmt"
	"sort"
	"strings"

	"go.putnami.dev/app"
	perrors "go.putnami.dev/errors"
	pdb "go.putnami.dev/protocol/database"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/infra"
	protocolmigration "go.putnami.dev/protocol/migration"
)

// codeInfra marks failures emitting infra requirements.
const codeInfra perrors.Code = "db.infra"

// sidecarSlug names this producer's per-project infra scratch fragment
// (<project>/.gen/infra/database.json).
const sidecarSlug = "database"

// Compile-time check: the plugin emits infra requirements during describe.
var _ app.Describer = (*Plugin)(nil)

// Describe emits the project's framework-generated infra-requirements into
// the database producer's scratch fragment at <OutputDir>/infra/database.json
// — where OutputDir is the workload's ".gen" directory. It runs during the
// build's describe phase (after Configure, before Start) and declares the
// workload's datasources: every named datasource (the primary Datasource plus
// each PluginConfig.Datasources entry) as a {name, engine} requirement via the
// database-protocol bridge, so each datasource-bound runtime pool is
// independently provisionable and bindable. A legacy explicit-connection pool
// with no named datasource falls back to declaring the pool's own database
// (named by Pool.Database) carrying the schemas it references directly
// (Pool.Schemas).
//
// Schemas owned by SQL migration sources are not derived here: each SQLSource
// declares its own (datasource, schema) via migration.SchemaContributor, which
// the migration registry folds into .gen/infra/migration.json during the same
// describe phase — source-driven, so it needs no configured pool and is
// independent of the runtime persistence backend. The generator sync merges
// both fragments into committed infra/requirements.json.
//
// The write is atomic and deterministic, satisfying the generator scratch
// contract.
func (p *Plugin) Describe(dctx *app.DescribeContext) error {
	if dctx == nil || !dctx.Wants(p.Name()) {
		return nil
	}

	var manifest *infra.PerProjectManifest
	var diags []diag.Diagnostic
	if names := p.cfg.namedDatasources(); len(names) > 0 {
		// Canonical surface: derive each datasource's infra entry from the
		// database protocol via the projection bridge. The schema is
		// intentionally omitted — it lives in the runtime binding and the
		// migration sources, not in the build-time, secret-free requirement.
		manifest, diags = infraManifestForDatasources(names)
	} else {
		// A declared pool datasource must be complete (name + schema) so the
		// emitted requirement names a schema the deployer can provision. A
		// partial declaration is an authoring error caught at build time.
		if !p.cfg.Pool.Datasource.isZero() {
			if err := p.cfg.Pool.Datasource.Validate(); err != nil {
				return perrors.Wrap(err, codeInfra)
			}
		}
		engine := p.cfg.Pool.Engine
		if engine == "" {
			engine = infra.EnginePostgres
		}
		manifest, diags = buildInfraManifest(engine, deriveDatabaseDecls(p.cfg.Pool))
	}
	if errs := diag.Errors(diags); len(errs) > 0 {
		return perrors.Newf(codeInfra, "emit infra requirements: %s", joinDiagnostics(errs))
	}
	var fragment infra.PerProjectManifest
	if manifest != nil {
		fragment = *manifest
	}
	if err := infra.WriteSidecarIn(dctx.OutputDir, sidecarSlug, fragment); err != nil {
		return perrors.Wrapf(err, codeInfra, "emit infra requirements")
	}
	return nil
}

// databaseDecl is one logical database the framework derived, named by its
// datasource and carrying the schemas known at that point. Decls for the
// same name are merged additively when the manifest is built.
type databaseDecl struct {
	name    string
	schemas []string
}

// deriveDatabaseDecls turns the pool config into per-database declarations: the
// pool's own database carrying the schemas it references directly.
//
// When the pool declares a Datasource (the uniform provider), its Name and
// Schema are authoritative. Otherwise the legacy Pool.Database (defaulting to
// the canonical default datasource) and Pool.Schemas apply. Pool.Schemas are
// always merged in.
//
// Schemas owned by SQL migration sources are not derived here: each SQLSource
// declares its (datasource, schema) through migration.SchemaContributor.
func deriveDatabaseDecls(pool PoolConfig) []databaseDecl {
	schemas := append([]string(nil), pool.Schemas...)

	if name := strings.TrimSpace(pool.Datasource.Name); name != "" {
		if schema := strings.TrimSpace(pool.Datasource.Schema); schema != "" {
			schemas = append(schemas, schema)
		}
		return []databaseDecl{{name: name, schemas: schemas}}
	}

	poolDB := strings.TrimSpace(pool.Database)
	if poolDB == "" {
		poolDB = protocolmigration.DefaultDatasource
	}
	return []databaseDecl{{name: poolDB, schemas: schemas}}
}

// buildInfraManifest assembles a per-project manifest from the declared
// databases. Decls are merged by name with a sorted-union of schemas, the
// engine is applied uniformly (one plugin wraps one physical engine), and
// the result is validated. A nil manifest with no diagnostics means there
// is nothing to declare and no sidecar should be written.
func buildInfraManifest(engine infra.Engine, decls []databaseDecl) (*infra.PerProjectManifest, []diag.Diagnostic) {
	if len(decls) == 0 {
		return nil, nil
	}
	if !infra.ValidEngines[engine] {
		return nil, []diag.Diagnostic{diag.Errorf(infra.ErrorCodeInvalidEngine, "databases[].engine",
			"engine %q is not an accepted engine: %s", engine, strings.Join(infra.EngineNames(), ", "))}
	}

	schemasByDB := map[string]map[string]struct{}{}
	for _, d := range decls {
		set, ok := schemasByDB[d.name]
		if !ok {
			set = map[string]struct{}{}
			schemasByDB[d.name] = set
		}
		for _, s := range d.schemas {
			if s = strings.TrimSpace(s); s != "" {
				set[s] = struct{}{}
			}
		}
	}

	names := make([]string, 0, len(schemasByDB))
	for name := range schemasByDB {
		names = append(names, name)
	}
	sort.Strings(names)

	databases := make([]infra.Database, 0, len(names))
	for _, name := range names {
		databases = append(databases, infra.Database{
			Name:    name,
			Engine:  engine,
			Schemas: sortedSet(schemasByDB[name]),
		})
	}

	manifest := &infra.PerProjectManifest{
		Schema:          infra.PerProjectSchemaURL,
		ProtocolVersion: infra.ProtocolVersion,
		Databases:       databases,
	}
	if diags := infra.ValidatePerProjectManifest(manifest); diag.HasErrors(diags) {
		return nil, diags
	}
	return manifest, nil
}

// infraManifestForDatasources derives the per-project infra manifest for the
// plugin's named logical datasources by projecting a canonical database
// RequirementManifest through infra.DatabasesFromManifest — the single bridge
// that keeps infra's database entries a projection of the database protocol
// (go.putnami.dev/protocol/database) rather than a hand-modeled shape.
//
// The schema is intentionally omitted: it lives in the runtime binding (a
// secret-bearing artifact) and in the migration sources' schema contributions,
// not in the build-time, secret-free requirement. An invalid datasource name
// surfaces as an infra validation diagnostic — a build-time error.
func infraManifestForDatasources(names []string) (*infra.PerProjectManifest, []diag.Diagnostic) {
	reqs := make(map[string]pdb.Requirement, len(names))
	for _, name := range names {
		reqs[name] = pdb.Requirement{Engine: pdb.EnginePostgres}
	}
	requirement := &pdb.RequirementManifest{
		ProtocolVersion: pdb.ProtocolVersion,
		Databases:       reqs,
	}
	databases, diags := infra.DatabasesFromManifest(requirement)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	manifest := &infra.PerProjectManifest{
		Schema:          infra.PerProjectSchemaURL,
		ProtocolVersion: infra.ProtocolVersion,
		Databases:       databases,
	}
	if diags := infra.ValidatePerProjectManifest(manifest); diag.HasErrors(diags) {
		return nil, diags
	}
	return manifest, nil
}

// sortedSet returns the keys of set in sorted order, or nil when empty so
// the marshaled manifest omits an empty schemas list.
func sortedSet(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// joinDiagnostics renders diagnostic messages into a single string for an
// error returned from Describe.
func joinDiagnostics(diags []diag.Diagnostic) string {
	msgs := make([]string, 0, len(diags))
	for _, d := range diags {
		msgs = append(msgs, fmt.Sprintf("%s (%s)", d.Message, d.Code))
	}
	return strings.Join(msgs, "; ")
}
