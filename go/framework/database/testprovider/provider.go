// Package testprovider is the first slice of the Putnami cross-language test
// database provider. It turns a canonical test binding
// (go.putnami.dev/protocol/database TestBinding) into ready-to-use, isolated,
// migrated databases for one or more named datasources — the Go half of the
// uniform Go/TS test-DB contract.
//
// The slice targets an externally provided Postgres (CI's service container or
// a developer's local server) selected by mode=require: the deploy/test
// environment injects a TestBinding via DATABASE_TEST_BINDINGS, the provider
// creates an isolated database (or schema) per datasource, applies the
// workload's published migration bundle with the existing machinery
// (database.ApplyBundle), sets the owning schema as the search_path, and hands
// back a runtime Binding the framework adapter consumes through
// database.PoolConfigFromBinding. Docker auto-provisioning (mode=auto) and
// migrated-template reuse are later slices.
//
// It is multi-datasource by construction: the binding is a map keyed by logical
// datasource name, so a workload can provision auth, billing, etc. at once with
// no single-DSN env convention. Unit tests pay no database cost — the live path
// runs only when DATABASE_TEST_BINDINGS is injected.
package testprovider

import (
	"cmp"
	"context"
	stderrors "errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.putnami.dev/database"
	perrors "go.putnami.dev/errors"
	pdb "go.putnami.dev/protocol/database"
	diag "go.putnami.dev/protocol/diagnostic"
	protocolmigration "go.putnami.dev/protocol/migration"
)

// EnvTestBinding is the environment variable a CI/test environment injects to
// hand the provider the canonical TestBinding (JSON). It is the multi-datasource
// counterpart of the runtime DATABASE_BINDINGS — there is deliberately no
// single-DSN PUTNAMI_TEST_DB_DSN convention, which cannot model a workload that
// uses several datasources.
const EnvTestBinding = "DATABASE_TEST_BINDINGS"

// codeProvider tags errors raised while resolving or provisioning test
// databases.
const codeProvider perrors.Code = "db.testprovider"

// ErrSkip signals that no usable test binding/provider exists and the binding's
// mode is skip, so a caller can t.Skip the suite rather than fail it.
var ErrSkip = stderrors.New("database test provider: no usable test binding (mode=skip)")

// Options configures one provisioning run.
type Options struct {
	// Binding is the resolved test binding. When nil, it is read and validated
	// from EnvTestBinding.
	Binding *pdb.TestBinding
	// Bundle is the published migration bundle (the directory containing
	// bundle.json) applied to each provisioned database when the binding sets
	// applyMigrations. Nil skips migration apply.
	Bundle fs.FS
	// newSuffix injects the isolated-identifier suffix; nil uses the creation
	// time followed by random hex (see isolatedSuffix). Exposed for
	// deterministic tests.
	newSuffix func() string
}

// Result is what a test receives: a runtime Binding whose datasources point at
// the freshly provisioned, migrated, isolated databases (consume it via
// database.PoolConfigFromBinding), and a Cleanup that tears them down. Cleanup
// is safe to call once; callers typically defer it or register it with
// t.Cleanup. It does not depend on the context Provision received, so a
// t.Context() that is already canceled when Cleanup runs still drops the
// databases.
type Result struct {
	Binding *pdb.Binding
	Cleanup func() error
}

// ResolveTestBinding parses and validates the canonical TestBinding from raw
// (typically the DATABASE_TEST_BINDINGS value). It returns (nil, nil) when raw
// is blank so a caller can apply the binding's mode policy.
func ResolveTestBinding(raw string) (*pdb.TestBinding, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	tb, diags := pdb.ParseAndValidateTestBinding([]byte(trimmed))
	if diag.HasErrors(diags) {
		return nil, perrors.Newf(codeProvider, "invalid %s:\n%s", EnvTestBinding, diag.ErrorText(diags))
	}
	return tb, nil
}

// effectiveMode is the binding's mode, defaulting to require — the safest
// default for the first slice: a test that asks for a database fails loudly
// when none is available rather than silently passing.
func effectiveMode(tb *pdb.TestBinding) pdb.TestMode {
	if tb == nil || tb.Mode == "" {
		return pdb.TestModeRequire
	}
	return tb.Mode
}

// Provision provisions isolated, migrated databases for every datasource in the
// test binding and returns their runtime bindings plus a cleanup.
//
// With no usable binding it honors the mode: skip → ErrSkip, require → a loud
// error, auto → a loud error for now (Docker auto-provisioning is a later
// slice). On any partial failure it tears down whatever it already created
// before returning, so a failed run leaves no orphaned databases.
//
// A test process that is killed never runs Cleanup. With database isolation
// and without keepDatabases, Provision therefore first reclaims the per-suite
// databases of the same base that such a process left behind: those with no
// open connection that are older than orphanAge (see reclaimOrphans).
func Provision(ctx context.Context, opts Options) (*Result, error) {
	tb := opts.Binding
	if tb == nil {
		var err error
		if tb, err = ResolveTestBinding(os.Getenv(EnvTestBinding)); err != nil {
			return nil, err
		}
	}
	if tb == nil || len(tb.Databases) == 0 {
		if effectiveMode(tb) == pdb.TestModeSkip {
			return nil, ErrSkip
		}
		return nil, perrors.Newf(codeProvider,
			"no test database binding: set %s (mode=%s requires a usable binding)", EnvTestBinding, effectiveMode(tb))
	}

	suffix := opts.newSuffix
	if suffix == nil {
		suffix = func() string { return isolatedSuffix(time.Now()) }
	}
	plans, err := planDatabases(tb, suffix)
	if err != nil {
		return nil, err
	}

	apply := tb.ApplyMigrations && opts.Bundle != nil
	if reuseTemplates(tb, apply) {
		digest, err := bundleDigest(opts.Bundle)
		if err != nil {
			return nil, err
		}
		for i := range plans {
			base, err := connectionDatabase(plans[i].entry.Connection)
			if err != nil {
				return nil, err
			}
			plans[i].template = templateName(cmp.Or(strings.TrimSpace(base), plans[i].name), digest)
		}
	}
	var cleanups []func() error
	cleanup := func() error {
		var errs []error
		// Tear down in reverse creation order.
		for i := len(cleanups) - 1; i >= 0; i-- {
			if e := cleanups[i](); e != nil {
				errs = append(errs, e)
			}
		}
		return stderrors.Join(errs...)
	}

	for i := range plans {
		drop, err := provisionOne(ctx, plans[i], opts.Bundle, apply)
		if drop != nil {
			cleanups = append(cleanups, drop)
		}
		if err != nil {
			_ = cleanup() //nolint:errcheck // best-effort teardown while unwinding a failed run
			return nil, err
		}
	}

	binding, err := runtimeBinding(plans)
	if err != nil {
		_ = cleanup() //nolint:errcheck // best-effort teardown while unwinding a failed run
		return nil, err
	}
	return &Result{Binding: binding, Cleanup: cleanup}, nil
}

// plan is one datasource's provisioning decision.
type plan struct {
	name      string        // logical datasource name
	isolation pdb.Isolation // database | schema
	entry     pdb.Database  // the test binding entry (engine/schema/connection)
	isoDB     string        // isolated database name (isolation=database)
	isoPrefix string        // name prefix every per-suite database of this base shares (isolation=database)
	isoSchema string        // owning schema applied as search_path
	template  string        // migrated template database to clone from (reuse); "" applies fresh
	keep      bool          // leave the isolated database in place on teardown (binding keepDatabases)
}

// isolationOf returns the binding's isolation, defaulting to database — the
// preferred boundary when CREATE DATABASE is permitted.
func isolationOf(tb *pdb.TestBinding) pdb.Isolation {
	if tb == nil || tb.Isolation == "" {
		return pdb.IsolationDatabase
	}
	return tb.Isolation
}

// reuseTemplates reports whether the run should clone per-suite databases from a
// migrated template keyed by the bundle digest, rather than replaying migrations
// for every suite. It applies only when the binding opts into bundle-template
// reuse, migrations are being applied, and isolation is database (a template is
// a database). Schema isolation and no-migration runs fall back to fresh.
func reuseTemplates(tb *pdb.TestBinding, apply bool) bool {
	return apply && tb != nil && tb.Reuse == pdb.ReuseBundleTemplate && isolationOf(tb) == pdb.IsolationDatabase
}

// planDatabases turns the test binding into one plan per datasource, sorted by
// name for deterministic provisioning, allocating a unique isolated identifier
// for each. It is pure (no database access) so the isolation decisions and
// identifier construction are unit-tested without a live server.
func planDatabases(tb *pdb.TestBinding, suffix func() string) ([]plan, error) {
	iso := isolationOf(tb)
	names := make([]string, 0, len(tb.Databases))
	for n := range tb.Databases {
		names = append(names, n)
	}
	sort.Strings(names)

	plans := make([]plan, 0, len(names))
	for _, name := range names {
		e := tb.Databases[name]
		if e.Engine != pdb.EnginePostgres {
			return nil, perrors.Newf(codeProvider, "datasource %q uses unsupported engine %q (only postgres)", name, string(e.Engine))
		}
		if e.Connection == nil {
			return nil, perrors.Newf(codeProvider, "datasource %q has no connection in its test binding", name)
		}
		p := plan{name: name, isolation: iso, entry: e, isoSchema: e.Schema, keep: tb.KeepDatabases}
		switch iso {
		case pdb.IsolationDatabase:
			base, err := connectionDatabase(e.Connection)
			if err != nil {
				return nil, perrors.Wrapf(err, codeProvider, fmt.Sprintf("datasource %q", name))
			}
			p.isoDB = pgIdent(cmp.Or(strings.TrimSpace(base), name), "t", suffix())
			p.isoPrefix = isolatedPrefix(cmp.Or(strings.TrimSpace(base), name))
		case pdb.IsolationSchema:
			p.isoSchema = pgIdent(cmp.Or(strings.TrimSpace(e.Schema), name), "t", suffix())
		default:
			return nil, perrors.Newf(codeProvider, "datasource %q has unknown isolation %q", name, string(iso))
		}
		plans = append(plans, p)
	}
	return plans, nil
}

// templateName is the migrated template database for a datasource, keyed by the
// migration bundle digest so suites sharing a bundle reuse the same template and
// a new bundle (new digest) gets a fresh one.
func templateName(base, digest string) string {
	d := digest
	if len(d) > 12 {
		d = d[:12]
	}
	return pgIdent(base, "tmpl", d)
}

// runtimeBinding builds the *pdb.Binding tests consume from the plans. For
// database isolation the connection is cloned with its database swapped to the
// isolated one (owning schema unchanged); for schema isolation the connection
// is unchanged and the isolated schema becomes the search_path. The result is a
// canonical Binding, so a test resolves a pool from it with the same
// database.PoolConfigFromBinding the runtime uses — keeping test and runtime
// connection semantics identical. It is pure and unit-tested.
func runtimeBinding(plans []plan) (*pdb.Binding, error) {
	out := &pdb.Binding{ProtocolVersion: pdb.ProtocolVersion, Databases: make(map[string]pdb.Database, len(plans))}
	for _, p := range plans {
		conn := cloneConnection(p.entry.Connection)
		schema := p.entry.Schema
		switch p.isolation {
		case pdb.IsolationDatabase:
			if err := setConnectionDatabase(conn, p.isoDB); err != nil {
				return nil, perrors.Wrapf(err, codeProvider, fmt.Sprintf("datasource %q", p.name))
			}
		case pdb.IsolationSchema:
			schema = p.isoSchema
		}
		out.Databases[p.name] = pdb.Database{Engine: pdb.EnginePostgres, Schema: schema, Connection: conn}
	}
	return out, nil
}

// provisionOne creates the isolated database/schema for one plan, applies the
// migration bundle when requested, and returns a teardown closure. The teardown
// is returned even on error so a partially provisioned datasource is still
// dropped by the caller's unwinding cleanup.
func provisionOne(ctx context.Context, p plan, bundle fs.FS, apply bool) (drop func() error, err error) {
	switch p.isolation {
	case pdb.IsolationDatabase:
		return provisionDatabase(ctx, p, bundle, apply)
	case pdb.IsolationSchema:
		return provisionSchema(ctx, p, bundle, apply)
	default:
		return nil, perrors.Newf(codeProvider, "datasource %q has unknown isolation %q", p.name, string(p.isolation))
	}
}

func provisionDatabase(ctx context.Context, p plan, bundle fs.FS, apply bool) (func() error, error) {
	admin, err := openPool(ctx, p.entry.Connection, "")
	if err != nil {
		return nil, perrors.Wrapf(err, codeProvider, fmt.Sprintf("datasource %q: connect admin", p.name))
	}

	// A binding that keeps its databases runs on a server that dies with the
	// run, so there is nothing to reclaim and no DROP worth its checkpoint.
	if !p.keep {
		reclaimOn(ctx, admin, p.isoPrefix, time.Now())
	}

	// Reuse: clone the per-suite database from a migrated template instead of
	// replaying migrations. The template is created and migrated once, guarded
	// by an advisory lock so concurrent suites/processes don't race.
	if p.template != "" {
		if err := ensureTemplate(ctx, admin, p, bundle); err != nil {
			admin.Close()
			return nil, perrors.Wrapf(err, codeProvider, fmt.Sprintf("datasource %q: ensure template", p.name))
		}
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoteIdent(p.isoDB)+" TEMPLATE "+quoteIdent(p.template)); err != nil {
			admin.Close()
			return nil, perrors.Wrapf(err, codeProvider, fmt.Sprintf("datasource %q: clone template", p.name))
		}
		return teardownFor(ctx, admin, p), nil
	}

	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoteIdent(p.isoDB)); err != nil {
		admin.Close()
		return nil, perrors.Wrapf(err, codeProvider, fmt.Sprintf("datasource %q: create database", p.name))
	}
	drop := teardownFor(ctx, admin, p)

	target := cloneConnection(p.entry.Connection)
	if err := setConnectionDatabase(target, p.isoDB); err != nil {
		return drop, perrors.Wrapf(err, codeProvider, fmt.Sprintf("datasource %q", p.name))
	}
	if err := prepareSchemaAndApply(ctx, target, p.isoSchema, bundle, p.name, apply); err != nil {
		return drop, perrors.Wrapf(err, codeProvider, fmt.Sprintf("datasource %q", p.name))
	}
	return drop, nil
}

// dropDatabase returns a teardown closure that drops the per-suite database and
// closes the admin pool. A reused template is intentionally left warm — it is
// immutable (only cloned from), so it leaks no cross-suite state.
//
// The DROP runs on a context detached from ctx's cancellation and bounded by
// teardownTimeout: the teardown outlives the provisioning context by design.
func dropDatabase(ctx context.Context, admin *database.Pool, name string) func() error {
	return func() error {
		defer admin.Close()
		dropCtx, cancel := teardownContext(ctx)
		defer cancel()
		_, e := admin.Exec(dropCtx, "DROP DATABASE IF EXISTS "+quoteIdent(name)+" WITH (FORCE)")
		return e
	}
}

// teardownContext detaches a teardown from the provisioning context's
// cancellation and deadline, keeps its values, and bounds it by
// teardownTimeout.
func teardownContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), teardownTimeout)
}

// reclaimOn runs reclaimOrphans on one connection of the admin pool. It is
// best-effort: a connection it cannot acquire only skips the pass.
func reclaimOn(ctx context.Context, admin *database.Pool, prefix string, now time.Time) {
	conn, err := admin.PGXPool().Acquire(ctx)
	if err != nil {
		return
	}
	defer conn.Release()
	reclaimOrphans(ctx, pgOrphanServer{conn: conn}, prefix, now)
}

// teardownFor picks the per-suite teardown: the DROP above, or — when the
// binding keeps databases — only the admin pool close. DROP DATABASE forces an
// immediate cluster-wide checkpoint and waits for it, so on a busy shared
// server its cost tracks the whole fleet's writes, not the dropped database; a
// binding whose server dies with the run declares keepDatabases and skips it.
func teardownFor(ctx context.Context, admin *database.Pool, p plan) func() error {
	if p.keep {
		return func() error {
			admin.Close()
			return nil
		}
	}
	return dropDatabase(ctx, admin, p.isoDB)
}

// ensureTemplate creates and migrates the template database for a plan exactly
// once across concurrent suites/processes: it serializes on a Postgres advisory
// lock (held on a single dedicated connection), skips the work when the template
// already exists, and otherwise creates it and applies the datasource's
// migrations. CREATE DATABASE ... TEMPLATE then clones it cheaply per suite.
func ensureTemplate(ctx context.Context, admin *database.Pool, p plan, bundle fs.FS) error {
	conn, err := admin.PGXPool().Acquire(ctx)
	if err != nil {
		return perrors.Wrapf(err, codeProvider, "acquire template lock connection")
	}
	defer conn.Release()

	key := lockKey(p.template)
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		return perrors.Wrapf(err, codeProvider, "acquire template advisory lock")
	}
	defer func() {
		_, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", key) //nolint:errcheck // best-effort unlock; the session ends on Release regardless
	}()

	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", p.template).Scan(&exists); err != nil {
		return perrors.Wrapf(err, codeProvider, "check template existence")
	}
	if exists {
		return nil
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+quoteIdent(p.template)); err != nil {
		return perrors.Wrapf(err, codeProvider, "create template database")
	}
	dropFailedTemplate := func() {
		// Best-effort cleanup of a failed template setup. A broken template
		// would be trusted by name on the next reuse attempt.
		_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(p.template)+" WITH (FORCE)") //nolint:errcheck
	}

	target := cloneConnection(p.entry.Connection)
	if err := setConnectionDatabase(target, p.template); err != nil {
		dropFailedTemplate()
		return err
	}
	if err := prepareSchemaAndApply(ctx, target, p.entry.Schema, bundle, p.name, true); err != nil {
		dropFailedTemplate()
		return err
	}
	return nil
}

func provisionSchema(ctx context.Context, p plan, bundle fs.FS, apply bool) (func() error, error) {
	target, err := openPool(ctx, p.entry.Connection, p.isoSchema)
	if err != nil {
		return nil, perrors.Wrapf(err, codeProvider, fmt.Sprintf("datasource %q: connect", p.name))
	}
	if _, err := target.Exec(ctx, "CREATE SCHEMA "+quoteIdent(p.isoSchema)); err != nil {
		target.Close()
		return nil, perrors.Wrapf(err, codeProvider, fmt.Sprintf("datasource %q: create schema", p.name))
	}
	drop := func() error {
		defer target.Close()
		dropCtx, cancel := teardownContext(ctx)
		defer cancel()
		_, e := target.Exec(dropCtx, "DROP SCHEMA IF EXISTS "+quoteIdent(p.isoSchema)+" CASCADE")
		return e
	}
	if apply {
		if err := applyDatasourceMigrations(ctx, target, bundle, p.name); err != nil {
			return drop, perrors.Wrapf(err, codeProvider, fmt.Sprintf("datasource %q: apply migrations", p.name))
		}
	}
	return drop, nil
}

// prepareSchemaAndApply opens a pool on the isolated database, ensures the
// owning schema exists, and applies the datasource's migrations. The migration
// machinery verifies hashes as it applies, so a corrupt bundle or drift surfaces
// here.
func prepareSchemaAndApply(ctx context.Context, conn *pdb.Connection, schema string, bundle fs.FS, datasource string, apply bool) error {
	pool, err := openPool(ctx, conn, schema)
	if err != nil {
		return err
	}
	defer pool.Close()
	if strings.TrimSpace(schema) != "" {
		if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+quoteIdent(schema)); err != nil {
			return perrors.Wrapf(err, codeProvider, "create schema")
		}
	}
	if apply {
		if err := applyDatasourceMigrations(ctx, pool, bundle, datasource); err != nil {
			return err
		}
	}
	return nil
}

// applyDatasourceMigrations applies only the bundle's migrations that target
// datasource against pool, so each physical database receives just its own
// datasource's migrations rather than the whole workload's bundle. A datasource
// with no migrations in the bundle is a no-op.
func applyDatasourceMigrations(ctx context.Context, pool *database.Pool, bundle fs.FS, datasource string) error {
	sources, err := database.LoadBundleSources(bundle)
	if err != nil {
		return err
	}
	scoped := scopeSources(sources, datasource)
	if len(scoped) == 0 {
		return nil
	}
	if _, err := database.ApplyToPool(ctx, pool, scoped...); err != nil {
		return perrors.Wrapf(err, codeProvider, "apply migrations")
	}
	return nil
}

// scopeSources keeps only the migration sources that target datasource, so a
// physical database receives just its own datasource's migrations. It also
// neutralizes each source's declared schema: the provider owns the search_path
// through the per-suite isolated pool (the isolated schema for schema isolation,
// the owning schema for database isolation), so the runner must not override it
// with the source's bundle-declared schema.
func scopeSources(sources []database.SQLSource, datasource string) []database.SQLSource {
	scoped := make([]database.SQLSource, 0, len(sources))
	for _, s := range sources {
		if s.Datasource() == datasource {
			scoped = append(scoped, s.WithSchema(""))
		}
	}
	return scoped
}

// lockKey hashes a template name into the int64 key space pg_advisory_lock uses.
func lockKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name)) //nolint:errcheck // hash.Write never errors
	return int64(h.Sum64())      //nolint:gosec // intentional bit reinterpretation into the signed advisory-lock key space
}

// bundleDigest computes the canonical migration-bundle digest used to key reused
// templates, so suites sharing a bundle share a template and a changed bundle
// gets a fresh one.
func bundleDigest(bundle fs.FS) (string, error) {
	b, _, diags := protocolmigration.LoadBundle(bundle)
	if diag.HasErrors(diags) {
		return "", perrors.Newf(codeProvider, "load migration bundle:\n%s", diag.ErrorText(diags))
	}
	return protocolmigration.ComputeBundleDigest(*b), nil
}

// openPool builds a database.Pool for one connection, reusing the runtime
// adapter's PoolConfigFromBinding so test and runtime connection rendering are
// identical. schema, when set, becomes the session search_path.
func openPool(ctx context.Context, conn *pdb.Connection, schema string) (*database.Pool, error) {
	b := &pdb.Binding{
		ProtocolVersion: pdb.ProtocolVersion,
		Databases:       map[string]pdb.Database{"_": {Engine: pdb.EnginePostgres, Schema: schema, Connection: conn}},
	}
	cfg, err := database.PoolConfigFromBinding(b, "_")
	if err != nil {
		return nil, err
	}
	return database.NewPool(ctx, cfg)
}

// connectionDatabase reports the physical database a connection targets, used as
// the base for an isolated database name.
func connectionDatabase(conn *pdb.Connection) (string, error) {
	if conn == nil {
		return "", perrors.Newf(codeProvider, "nil connection")
	}
	if d := strings.TrimSpace(conn.Database); d != "" {
		return d, nil
	}
	if dsn := strings.TrimSpace(conn.DSN); dsn != "" {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", perrors.Wrapf(err, codeProvider, "parse connection dsn")
		}
		return strings.TrimPrefix(u.Path, "/"), nil
	}
	return "", nil
}

// setConnectionDatabase swaps the physical database a connection targets to db.
// Structured (host/instance) connections set the Database field; a DSN
// connection rewrites the postgres:// URL path. A non-URL DSN cannot be
// rewritten, so database isolation requires a structured connection or a URL
// DSN there.
func setConnectionDatabase(conn *pdb.Connection, db string) error {
	if conn == nil {
		return perrors.Newf(codeProvider, "nil connection")
	}
	if strings.TrimSpace(conn.DSN) != "" {
		u, err := url.Parse(conn.DSN)
		if err != nil {
			return perrors.Wrapf(err, codeProvider, "rewrite connection dsn database")
		}
		u.Path = "/" + db
		conn.DSN = u.String()
		return nil
	}
	conn.Database = db
	return nil
}

// cloneConnection deep-copies a connection so a derived binding never mutates
// the caller's test binding.
func cloneConnection(in *pdb.Connection) *pdb.Connection {
	if in == nil {
		return nil
	}
	out := *in
	if in.SSL != nil {
		ssl := *in.SSL
		out.SSL = &ssl
	}
	if len(in.Params) > 0 {
		out.Params = make(map[string]string, len(in.Params))
		for k, v := range in.Params {
			out.Params[k] = v
		}
	}
	return &out
}

// pgIdent builds a lowercase Postgres identifier from a base, a tag ("t" for a
// per-suite database/schema, "tmpl" for a reused template), and a suffix,
// sanitizing illegal characters and clamping to Postgres' 63-byte limit so the
// "_<tag>_<suffix>" tail always survives.
func pgIdent(base, tag, suffix string) string {
	base = sanitizeIdent(base)
	if base == "" {
		base = "pn"
	}
	tail := "_" + tag + "_" + suffix
	const maxLen = 63
	if len(base)+len(tail) > maxLen && maxLen-len(tail) > 0 {
		base = base[:maxLen-len(tail)]
	}
	return base + tail
}

func sanitizeIdent(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// quoteIdent renders a validated identifier for DDL via pgx's identifier
// quoting, defending against injection even though provider-built names are
// already sanitized.
func quoteIdent(name string) string {
	return pgx.Identifier{name}.Sanitize()
}
