package migration

import (
	"sort"

	"go.putnami.dev/errors"
	"go.putnami.dev/protocol/infra"
)

// SchemaContributor is the optional interface a migration Source implements
// to declare the database it migrates and the schema(s) it owns. Migration
// sits one layer above the kind-specific frameworks (SQL, document, …) and
// is the cross-kind coordinator that knows which schemas a project actually
// migrates, so it is the authoritative source for the schemas list of each
// database it touches.
//
// A Source that does not implement SchemaContributor contributes nothing to
// the infra requirements manifest; its migrations still apply normally.
type SchemaContributor interface {
	// InfraDatabases returns the databases this source migrates, each
	// tagged with its engine and the schema(s) the source owns. A source
	// typically returns a single entry, but a source spanning multiple
	// targets may return several.
	InfraDatabases() []infra.Database
}

// InfraRequirements builds the per-project infra requirements manifest the
// migration framework contributes during the project's generate build
// phase. It walks every registered Source across all kinds, collects the
// infra.Database declarations from sources implementing SchemaContributor,
// and folds them into a set keyed by (Name, Engine): each database's Schemas
// is the union of the schemas declared by every source that targets it.
//
// Two sources that target the same (Name, Engine) are merged into one
// database entry; sources targeting the same Name under different engines
// stay distinct, matching the merge identity the generator sync and build
// aggregator use. The output is deterministic — databases are sorted by
// (Name, Engine) and each Schemas list is sorted and deduplicated — so the
// emitted artifact is stable across builds regardless of source registration
// order.
func (r *Registry) InfraRequirements() infra.PerProjectManifest {
	type dbKey struct {
		name   string
		engine infra.Engine
	}
	schemas := map[dbKey]map[string]struct{}{}
	var order []dbKey

	for _, k := range r.Kinds() {
		for _, s := range r.Sources(k) {
			declarer, ok := s.(SchemaContributor)
			if !ok {
				continue
			}
			for _, db := range declarer.InfraDatabases() {
				key := dbKey{name: db.Name, engine: db.Engine}
				set, seen := schemas[key]
				if !seen {
					set = map[string]struct{}{}
					schemas[key] = set
					order = append(order, key)
				}
				for _, schema := range db.Schemas {
					set[schema] = struct{}{}
				}
			}
		}
	}

	manifest := infra.PerProjectManifest{ProtocolVersion: infra.ProtocolVersion}
	if len(order) == 0 {
		return manifest
	}

	sort.Slice(order, func(i, j int) bool {
		if order[i].name != order[j].name {
			return order[i].name < order[j].name
		}
		return order[i].engine < order[j].engine
	})

	manifest.Databases = make([]infra.Database, 0, len(order))
	for _, key := range order {
		set := schemas[key]
		var list []string
		if len(set) > 0 {
			list = make([]string, 0, len(set))
			for schema := range set {
				list = append(list, schema)
			}
			sort.Strings(list)
		}
		manifest.Databases = append(manifest.Databases, infra.Database{
			Name:    key.name,
			Engine:  key.engine,
			Schemas: list,
		})
	}
	return manifest
}

// sidecarSlug names this producer's per-project infra scratch fragment
// (<project>/.gen/infra/migration.json).
const sidecarSlug = "migration"

// WriteInfraRequirements writes the migration framework's per-project infra
// contribution to the migration producer's scratch fragment at
// "<outputDir>/infra/migration.json". The Go generator later syncs that
// fragment into committed infra/requirements.json. It is called from the
// application's describe build phase, where outputDir is the workload's
// ".gen" directory (app.DescribeContext.OutputDir).
//
// The write is atomic; an empty manifest removes any stale fragment.
func (r *Registry) WriteInfraRequirements(outputDir string) error {
	if err := infra.WriteSidecarIn(outputDir, sidecarSlug, r.InfraRequirements()); err != nil {
		return errors.Wrapf(err, CodeInfraEmit, "emit infra requirements scratch fragment")
	}
	return nil
}
