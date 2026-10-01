package database

import (
	"context"
	stdsql "database/sql"
	"io/fs"
	"path"
	"sort"
	"strings"

	perrors "go.putnami.dev/errors"
	"go.putnami.dev/migration"
)

const (
	upSuffix   = ".up.sql"
	downSuffix = ".down.sql"
)

// LoadSQLSource expands a SQLSource into the same []Definition the
// SQLRunner would materialize. File-pairing rules, metadata stamping,
// and sort order all match the runner's internal path, so the SHA-256
// each Migrator stamps on a Definition.SQL is byte-for-byte identical
// whether you go through the runner or call this helper directly.
//
// Authoring code inside an app.New(...) graph should contribute via
// app.MigrationContributor and let the framework's Migrate lifecycle
// phase apply migrations. This helper exists for tests and one-off
// binaries that bypass the lifecycle (privileged migrate Jobs with
// pre/post SQL, ad-hoc bootstrap scripts, test fixtures).
func LoadSQLSource(src SQLSource) ([]Definition, error) {
	return loadSQLSource(src)
}

// ApplyToPool applies the SQL migrations declared by sources against
// pool via a one-off migration.Registry + SQLRunner with Force=true.
// The pool's lifecycle stays with the caller — ApplyToPool neither
// opens it nor closes it. The wrapped *sql.DB handle acquires every
// connection under pool's search_path (so a schema-less source lands
// in the pool's datasource even on a shared physical pool) and is
// closed before return.
//
// Use this for one-off bootstrap binaries — e.g. a privileged migrate
// Job that runs CREATE ROLE / GRANT statements around the schema-apply
// step — and tests that need a real pool but not the full app
// lifecycle. Production code inside an app.New(...) graph should rely
// on app.MigrationContributor + database.NewPlugin so the framework
// owns the lifecycle and the multi-kind orchestration.
func ApplyToPool(ctx context.Context, pool *Pool, sources ...SQLSource) ([]migration.Record, error) {
	if len(sources) == 0 {
		return nil, nil
	}
	if pool == nil {
		return nil, perrors.Newf(CodeMigrationStartup, "ApplyToPool requires a non-nil pool")
	}
	reg := migration.NewRegistry()
	for _, src := range sources {
		if err := reg.AddSource(src); err != nil {
			return nil, perrors.Wrap(err, CodeMigrationInvalidDef)
		}
	}
	db := newStdlibDB(pool)
	if db == nil {
		return nil, perrors.Newf(CodeMigrationStartup, "ApplyToPool requires an open pool")
	}
	defer db.Close() //nolint:errcheck // best-effort close
	runner := newSQLRunner(SQLRunnerOptions{
		Registry:  reg,
		OpenDB:    func() (*stdsql.DB, error) { return db, nil },
		AutoApply: true,
	})
	if err := reg.RegisterRunner(runner); err != nil {
		return nil, perrors.Wrap(err, CodeMigrationStartup)
	}
	return runner.Apply(ctx, migration.ApplyOpts{Force: true})
}

// loadSQLSource expands one SQLSource into its constituent Definitions.
// The returned slice is sorted lexicographically by Name (i.e. by
// "<namespace>/<basename>"), which is also the order Migrator.Up applies.
//
// File pairing rules:
//   - A *.up.sql file is required; a *.down.sql with the same basename
//     is optional (the migration becomes non-reversible).
//   - A *.down.sql file without a matching *.up.sql is a fatal error —
//     authors typically delete the .up.sql by mistake.
//   - Subdirectories are walked; only the basename (without suffix) is
//     used as the migration identifier, so two files in different
//     subdirectories must not share a basename.
//   - Files that don't end with .up.sql or .down.sql are skipped.
//
// Inline definitions are appended after FS-loaded ones; both are
// namespaced and have their Source diagnostic origin set.
func loadSQLSource(src SQLSource) ([]Definition, error) {
	defs := make([]Definition, 0, len(src.inline))

	if src.fsys != nil {
		fileDefs, err := loadFSDefinitions(src)
		if err != nil {
			return nil, err
		}
		defs = append(defs, fileDefs...)
	}

	for _, d := range src.inline {
		d = applySourceMetadata(d, src, "inline:"+src.canonicalNamespace())
		if d.SQL == "" {
			return nil, perrors.Newf(CodeMigrationInvalidDef,
				"inline migration %q has empty SQL", d.Name)
		}
		defs = append(defs, d)
	}

	sort.SliceStable(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs, nil
}

// loadFSDefinitions walks src.fsys and pairs files by basename.
func loadFSDefinitions(src SQLSource) ([]Definition, error) {
	ups := map[string]string{}   // basename → path
	downs := map[string]string{} // basename → path

	err := fs.WalkDir(src.fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		// Suffix match is case-insensitive so the same file pair has the
		// same behavior on case-sensitive Linux and case-insensitive
		// macOS APFS (default). Any mixed-case suffix (.UP.sql,
		// .Down.SQL) is normalized to the canonical lowercase form;
		// the basename (everything before the suffix) is taken verbatim.
		lower := strings.ToLower(name)
		switch {
		case strings.HasSuffix(lower, upSuffix):
			base := name[:len(name)-len(upSuffix)]
			if prior, dup := ups[base]; dup {
				return perrors.Newf(CodeMigrationInvalidDef,
					"duplicate up migration basename %q in namespace %q (already seen at %s, now at %s)",
					base, src.canonicalNamespace(), prior, p)
			}
			ups[base] = p
		case strings.HasSuffix(lower, downSuffix):
			base := name[:len(name)-len(downSuffix)]
			if prior, dup := downs[base]; dup {
				return perrors.Newf(CodeMigrationInvalidDef,
					"duplicate down migration basename %q in namespace %q (already seen at %s, now at %s)",
					base, src.canonicalNamespace(), prior, p)
			}
			downs[base] = p
		}
		return nil
	})
	if err != nil {
		return nil, perrors.Wrapf(err, CodeMigrationInvalidDef, "walk migrations FS",
			perrors.String("namespace", src.canonicalNamespace()))
	}

	// Detect orphan down files (down without a matching up).
	var orphans []string
	for base, p := range downs {
		if _, ok := ups[base]; !ok {
			orphans = append(orphans, p)
		}
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		return nil, perrors.Newf(CodeMigrationInvalidDef,
			"orphan down migration(s) in namespace %q (no matching .up.sql): %s",
			src.canonicalNamespace(), strings.Join(orphans, ", "))
	}

	defs := make([]Definition, 0, len(ups))
	bases := make([]string, 0, len(ups))
	for base := range ups {
		bases = append(bases, base)
	}
	sort.Strings(bases)

	for _, base := range bases {
		upPath := ups[base]
		upSQL, err := readFile(src.fsys, upPath)
		if err != nil {
			return nil, err
		}
		var downSQL string
		if downPath, ok := downs[base]; ok {
			d, err := readFile(src.fsys, downPath)
			if err != nil {
				return nil, err
			}
			downSQL = d
		}
		d := Definition{
			Name: base,
			SQL:  upSQL,
			Down: downSQL,
		}
		d = applySourceMetadata(d, src, "embed:"+path.Join(src.canonicalNamespace(), upPath))
		defs = append(defs, d)
	}
	return defs, nil
}

// applySourceMetadata stamps Namespace, Datasource, Source and the
// canonical namespaced Name on a Definition produced for src. It is
// idempotent: a Definition that already has those fields set keeps its
// values (mostly relevant for inline definitions that explicitly chose
// a different datasource).
func applySourceMetadata(d Definition, src SQLSource, originPath string) Definition {
	ns := src.canonicalNamespace()
	if d.Namespace == "" {
		d.Namespace = ns
	}
	if d.Datasource == "" {
		d.Datasource = src.datasource
	}
	if d.Source == "" {
		d.Source = originPath
	}
	// Stamp the namespaced name unless the author already provided one.
	if d.Name != "" && !strings.Contains(d.Name, "/") {
		d.Name = ns + "/" + d.Name
	}
	return d
}

func readFile(fsys fs.FS, p string) (string, error) {
	b, err := fs.ReadFile(fsys, p)
	if err != nil {
		return "", perrors.Wrapf(err, CodeMigrationInvalidDef,
			"read migration file", perrors.String("path", p))
	}
	return string(b), nil
}
