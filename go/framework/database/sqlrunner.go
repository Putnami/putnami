package database

import (
	"context"
	stdsql "database/sql"
	"sort"
	"sync"
	"time"

	perrors "go.putnami.dev/errors"
	"go.putnami.dev/logger"
	"go.putnami.dev/migration"
)

// SQLRunner is the migration.Runner implementation backed by the SQL
// engine. It is the multi-datasource fan-out: one *Migrator per
// datasource discovered across all SQLSources, each guarded by the
// per-datasource Postgres advisory lock.
//
// The runner reads its source set from the registry at every call so a
// late-arriving contributor (e.g. a module registered after the SQL
// runner) still participates. AutoApply (false by default) gates the
// framework's automatic Migrate lifecycle phase; the migrate CLI forces
// application via ApplyOpts.Force.
type SQLRunner struct {
	registry    *migration.Registry
	openDB      func() (*stdsql.DB, error)
	lockTimeout time.Duration
	autoApply   bool
	log         *logger.Logger

	mu       sync.Mutex
	migCache map[string]*Migrator
}

// SQLRunnerOptions configures an SQLRunner. Pass openDB as a thunk so
// the runner opens its own *sql.DB lazily on first use — useful for
// tests that wire a sqlmock, and for the CLI which doesn't want to open
// a connection at plugin-construction time.
type SQLRunnerOptions struct {
	Registry    *migration.Registry
	OpenDB      func() (*stdsql.DB, error)
	LockTimeout time.Duration
	AutoApply   bool
}

// newSQLRunner constructs a runner. It does NOT register itself; the
// caller (typically the database plugin) calls registry.RegisterRunner
// after construction.
func newSQLRunner(opts SQLRunnerOptions) *SQLRunner {
	return &SQLRunner{
		registry:    opts.Registry,
		openDB:      opts.OpenDB,
		lockTimeout: opts.LockTimeout,
		autoApply:   opts.AutoApply,
		log:         migrationLoggerFrom(logger.Default()),
		migCache:    make(map[string]*Migrator),
	}
}

// Kind reports migration.KindSQL.
func (r *SQLRunner) Kind() migration.Kind { return migration.KindSQL }

// Apply runs pending SQL migrations across every datasource discovered
// in the registry's SQLSources. When opts.Force is false, the runner
// no-ops unless AutoApply was set at construction time — that is how
// production services skip startup migration in favor of a separate
// CLI/job.
func (r *SQLRunner) Apply(ctx context.Context, opts migration.ApplyOpts) ([]migration.Record, error) {
	if !opts.Force && !r.autoApply {
		r.log.Debug("SQL runner Apply skipped (AutoApply=false; pass Force=true to override)")
		return nil, nil
	}

	migrators, err := r.materializeMigrators()
	if err != nil {
		return nil, err
	}

	var out []migration.Record
	for _, datasource := range sortedKeys(migrators) {
		mig := migrators[datasource]
		var applied []Migration
		if opts.To != "" {
			applied, err = mig.upTo(ctx, opts.To)
		} else {
			applied, err = mig.Up(ctx)
		}
		out = append(out, recordsFromMigrations(applied, migration.StatusApplied, datasource, r.sourceFor)...)
		if err != nil {
			return out, err
		}
	}
	if len(out) > 0 {
		r.log.Info("migrations applied", migrationAttr(map[string]any{
			"count":       len(out),
			"datasources": len(migrators),
		}))
	} else {
		r.log.Debug("migrations up to date", migrationAttr(map[string]any{
			"datasources": len(migrators),
		}))
	}
	return out, nil
}

// Status returns the state-store rows for every datasource targeted by
// the registry's SQLSources, plus the registry-side pending entries.
func (r *SQLRunner) Status(ctx context.Context) ([]migration.Record, error) {
	migrators, err := r.materializeMigrators()
	if err != nil {
		return nil, err
	}

	var out []migration.Record
	for _, datasource := range sortedKeys(migrators) {
		mig := migrators[datasource]
		rows, err := mig.Status(ctx)
		if err != nil {
			return out, err
		}
		appliedNames := make(map[string]struct{}, len(rows))
		for _, row := range rows {
			appliedNames[row.Name] = struct{}{}
			ns := namespaceFromName(row.Name)
			source := r.sourceFor(row.Name, datasource)
			out = append(out, recordFromMigration(row, ns, datasource, source))
		}
		// Append pending rows for registered-but-not-yet-applied
		// definitions so `status` shows the full picture.
		for _, def := range mig.Definitions() {
			if _, applied := appliedNames[def.Name]; applied {
				continue
			}
			out = append(out, migration.Record{
				Kind:      migration.KindSQL,
				Namespace: def.Namespace,
				Name:      def.Name,
				Status:    migration.StatusPending,
				Hash:      sha256Hex(def.SQL),
				Source:    def.Source,
				Target:    datasource,
			})
		}
	}
	return out, nil
}

// Rollback rolls back migrations across every datasource. Without opts.To,
// it rolls back only the most recently applied migration per datasource;
// with opts.To, it rolls back every migration applied after the named one
// (the named migration stays applied).
func (r *SQLRunner) Rollback(ctx context.Context, opts migration.RollbackOpts) ([]migration.Record, error) {
	migrators, err := r.materializeMigrators()
	if err != nil {
		return nil, err
	}

	var out []migration.Record
	for _, datasource := range sortedKeys(migrators) {
		mig := migrators[datasource]
		if opts.To == "" {
			rec, err := mig.Rollback(ctx)
			if err != nil {
				return out, err
			}
			if rec != nil {
				out = append(out, recordFromMigration(*rec, namespaceFromName(rec.Name), datasource, r.sourceFor(rec.Name, datasource)))
				out[len(out)-1].Status = migration.StatusRolledBack
			}
			continue
		}
		recs, err := mig.RollbackTo(ctx, opts.To)
		if err != nil {
			return out, err
		}
		for _, rec := range recs {
			rr := recordFromMigration(rec, namespaceFromName(rec.Name), datasource, r.sourceFor(rec.Name, datasource))
			rr.Status = migration.StatusRolledBack
			out = append(out, rr)
		}
	}
	if len(out) > 0 {
		r.log.Info("migrations rolled back", migrationAttr(map[string]any{
			"count":       len(out),
			"datasources": len(migrators),
		}))
	} else {
		r.log.Debug("no migrations rolled back", migrationAttr(map[string]any{
			"datasources": len(migrators),
		}))
	}
	return out, nil
}

// Verify aggregates per-datasource drift reports into one report.
func (r *SQLRunner) Verify(ctx context.Context) (migration.DriftReport, error) {
	report := migration.DriftReport{Kind: migration.KindSQL}
	migrators, err := r.materializeMigrators()
	if err != nil {
		return report, err
	}
	for _, datasource := range sortedKeys(migrators) {
		mig := migrators[datasource]
		dr, err := mig.Verify(ctx)
		if err != nil {
			return report, err
		}
		report.HashDrifts = append(report.HashDrifts, dr.HashDrifts...)
		report.MissingFromRegistry = append(report.MissingFromRegistry, dr.MissingFromRegistry...)
		report.MissingFromStore = append(report.MissingFromStore, dr.MissingFromStore...)
	}
	return report, nil
}

// materializeMigrators groups every SQLSource currently in the registry
// by datasource, expands each via the loader, and returns one Migrator
// per datasource (cached after first construction).
func (r *SQLRunner) materializeMigrators() (map[string]*Migrator, error) {
	if r.registry == nil {
		return nil, perrors.Newf(CodeMigrationStartup, "SQLRunner is not bound to a migration.Registry")
	}
	if r.openDB == nil {
		return nil, perrors.Newf(CodeMigrationStartup, "SQLRunner has no OpenDB function")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.migCache) > 0 {
		return r.migCache, nil
	}

	// Group definitions by datasource, and capture each datasource's owning
	// schema from its sources. The schema becomes the Migrator's search_path so
	// unqualified DDL lands in it — letting one migration set target many
	// schemas via distinct Datasource{Name, Schema} pairs. Two sources that map
	// the same datasource to different schemas is a contradiction (the search
	// path would be ambiguous and the shared state store cannot tell them
	// apart), so migration.ResolveDatasourceSchemas rejects it rather than silently
	// picking one; the bundle describer runs the same rule at build time.
	byDatasource := make(map[string][]Definition)
	var claims []migration.DatasourceSchemaClaim
	for _, src := range r.registry.Sources(migration.KindSQL) {
		s, ok := src.(SQLSource)
		if !ok {
			return nil, perrors.Newf(CodeMigrationInvalidDef,
				"non-SQLSource value contributed for kind=sql: %T (namespace %q)",
				src, src.Namespace())
		}
		claims = append(claims, migration.DatasourceSchemaClaim{Datasource: s.Datasource(), Schema: s.Schema(), Namespace: s.Namespace()})
		defs, err := loadSQLSource(s)
		if err != nil {
			return nil, err
		}
		for _, d := range defs {
			byDatasource[d.Datasource] = append(byDatasource[d.Datasource], d)
		}
	}
	schemaByDatasource, err := migration.ResolveDatasourceSchemas(claims)
	if err != nil {
		return nil, perrors.Wrap(err, CodeMigrationInvalidDef)
	}
	if len(byDatasource) == 0 {
		return r.migCache, nil
	}

	// Build per-datasource Migrator instances. All share the same *sql.DB
	// — Postgres advisory locks differentiate by hashtext key.
	db, err := r.openDB()
	if err != nil {
		return nil, err
	}

	for datasource, defs := range byDatasource {
		// Detect (datasource, name) duplicates across SQLSources.
		seen := make(map[string]string)
		for _, d := range defs {
			if prior, dup := seen[d.Name]; dup {
				return nil, perrors.Newf(CodeMigrationInvalidDef,
					"duplicate migration %q for datasource %q (first source=%s, second=%s)",
					d.Name, datasource, prior, d.Source)
			}
			seen[d.Name] = d.Source
		}
		r.migCache[datasource] = NewMigrator(db, MigrationConfig{
			Datasource:  datasource,
			Schema:      schemaByDatasource[datasource],
			Definitions: defs,
			LockTimeout: r.lockTimeout,
		})
	}
	r.log.Debug("SQL runner materialized", migrationAttr(map[string]any{
		"datasources": len(r.migCache),
	}))
	return r.migCache, nil
}

// sourceFor returns the diagnostic Source string for the named
// migration on the given datasource, or empty when not found in the
// current registry view.
func (r *SQLRunner) sourceFor(name, datasource string) string {
	mig, ok := r.migCache[datasource]
	if !ok {
		return ""
	}
	for _, d := range mig.Definitions() {
		if d.Name == name {
			return d.Source
		}
	}
	return ""
}

func sortedKeys(m map[string]*Migrator) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func namespaceFromName(name string) string {
	if i := indexOfFirst(name, '/'); i > 0 {
		return name[:i]
	}
	return ""
}

func indexOfFirst(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func recordsFromMigrations(migs []Migration, status migration.RecordStatus, target string, sourceFn func(name, target string) string) []migration.Record {
	out := make([]migration.Record, 0, len(migs))
	for _, m := range migs {
		rec := recordFromMigration(m, namespaceFromName(m.Name), target, sourceFn(m.Name, target))
		rec.Status = status
		out = append(out, rec)
	}
	return out
}

// Compile-time interface satisfaction check.
var _ migration.Runner = (*SQLRunner)(nil)
