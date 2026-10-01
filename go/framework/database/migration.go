package database

import (
	"context"
	stdsql "database/sql"
	stderrors "errors"
	"sort"
	"strings"
	"time"

	perrors "go.putnami.dev/errors"
	"go.putnami.dev/logger"
	"go.putnami.dev/migration"
	protocolmigration "go.putnami.dev/protocol/migration"
)

// Definition is the authoring contract for a single SQL migration. It
// mirrors the TypeScript MigrationDefinition shape so both runtimes
// consume the same migration.migrations state store.
//
// Most fields are stamped by the loader when a Definition originates
// from an embed.FS source; inline authors only need to set Name + SQL
// (+ Down when reversibility is desired).
type Definition struct {
	// Name is the framework-visible migration identifier. The canonical
	// form is "<namespace>/<basename>" (e.g.
	// "iam/20260520120000_create_users"); the loader stamps the prefix
	// when missing. Definitions are ordered lexicographically by Name
	// within a datasource.
	Name string
	// SQL is the up migration content. It is executed inside a
	// transaction with the state-store insert.
	SQL string
	// Down is the optional rollback SQL. Empty means non-reversible.
	Down string
	// Namespace identifies the feature plugin that owns this migration.
	// Populated by the loader from the source's Namespace().
	Namespace string
	// Datasource is the logical DB this migration targets. Populated by
	// the loader from the source's Datasource(); empty resolves to the
	// canonical default datasource at apply time.
	Datasource string
	// Source is a human-readable origin string for diagnostics: e.g.
	// "embed:iam/migrations/20260520120000_create_users.up.sql" or
	// "inline:iam". Set by the loader; not part of the hash.
	Source string
}

// MigrationConfig configures the per-datasource Migrator engine and, when
// used as PluginConfig.Migration, also carries plugin-level toggles.
type MigrationConfig struct {
	// Datasource is the logical database name this Migrator targets.
	// Empty resolves to the canonical default datasource.
	Datasource string
	// Schema is the schema this datasource's migrations create their objects
	// in — the schema half of the source's Datasource. When set, the Migrator
	// sets it as the transaction-local search_path before each up/down body, so
	// migrations can use unqualified DDL and one migration set can target many
	// schemas by registering it under several Datasource{Name, Schema} pairs.
	// Empty leaves the connection's own search_path (pool/binding) in force.
	Schema string
	// Definitions are the pre-loaded migrations for this datasource —
	// already namespaced, sorted lexicographically by Name, with Source
	// stamped. Authors typically build this via the SQLRunner loader;
	// passing a slice manually is useful for tests.
	Definitions []Definition
	// LockTimeout caps the time the migrator waits to ACQUIRE the
	// per-datasource advisory lock. Zero = wait forever (current
	// behavior).
	//
	// This timeout intentionally does NOT bound subsequent SQL execution.
	// Once the lock is held, a long-running migration (large index
	// build, partition swap) is allowed to run to completion — killing
	// it mid-flight on a wall-clock timeout would leave the schema in
	// an inconsistent state. Operators investigating a stuck migrate
	// job should look at the held lock and the in-flight statement,
	// not raise LockTimeout.
	LockTimeout time.Duration

	// --- Plugin-level toggles (ignored by NewMigrator; honored by the
	// plugin's SQLRunner construction) ---

	// AutoApply controls whether the SQL runner's Apply runs during the
	// framework's automatic Migrate lifecycle phase. Default false in
	// production services where a dedicated migrate CLI/job is the
	// authoritative path. Set true for samples and local development
	// where on-startup application is convenient.
	AutoApply bool
}

// Migrator applies and rolls back migrations for a single datasource
// against a single *sql.DB handle. It is constructed by the SQLRunner;
// tests can construct it directly with an explicit definition slice.
type Migrator struct {
	db          *stdsql.DB
	dbName      string
	schema      string
	definitions []Definition
	lockTimeout time.Duration
	log         *logger.Logger
}

// NewMigrator creates a Migrator bound to db and configured with cfg.
// cfg.Definitions is taken as the authoritative migration set; the
// Migrator does not consult any global registry.
func NewMigrator(db *stdsql.DB, cfg MigrationConfig) *Migrator {
	defs := make([]Definition, len(cfg.Definitions))
	copy(defs, cfg.Definitions)
	sort.SliceStable(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return &Migrator{
		db:          db,
		dbName:      resolveDatasource(cfg.Datasource, ""),
		schema:      strings.TrimSpace(cfg.Schema),
		definitions: defs,
		lockTimeout: cfg.LockTimeout,
		log:         migrationLoggerFrom(logger.Default()),
	}
}

// Datasource returns the logical DB name this Migrator targets.
func (m *Migrator) Datasource() string { return m.dbName }

// Definitions returns the migrations registered with this Migrator. The
// returned slice is a copy.
func (m *Migrator) Definitions() []Definition {
	out := make([]Definition, len(m.definitions))
	copy(out, m.definitions)
	return out
}

// Up applies every pending migration registered for this datasource.
func (m *Migrator) Up(ctx context.Context) ([]Migration, error) {
	return m.upTo(ctx, "")
}

// UpTo applies pending migrations forward to and including the named
// migration. The name may be either the full namespaced form
// "iam/20260520120000_create_users" or the bare basename when it is
// unambiguous; ambiguity is an error.
func (m *Migrator) UpTo(ctx context.Context, name string) ([]Migration, error) {
	return m.upTo(ctx, name)
}

func (m *Migrator) upTo(ctx context.Context, target string) ([]Migration, error) {
	if len(m.definitions) == 0 {
		m.log.Debug("no migrations registered", migrationAttr(map[string]any{
			"datasource": m.dbName,
		}))
		return nil, nil
	}

	resolvedTarget, err := m.resolveName(target)
	if err != nil {
		return nil, err
	}

	var result []Migration
	err = m.withLock(ctx, func(conn *stdsql.Conn) error {
		applied, err := listMigrations(ctx, conn, m.dbName, true)
		if err != nil {
			return err
		}
		appliedNames := make(map[string]struct{}, len(applied))
		for _, a := range applied {
			appliedNames[a.Name] = struct{}{}
		}

		for _, def := range m.definitions {
			if _, done := appliedNames[def.Name]; done {
				if resolvedTarget != "" && def.Name == resolvedTarget {
					break
				}
				continue
			}
			record, err := m.apply(ctx, conn, def)
			if err != nil {
				return err
			}
			result = append(result, record)
			if resolvedTarget != "" && def.Name == resolvedTarget {
				break
			}
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	m.log.Debug("migration scan completed", migrationAttr(map[string]any{
		"datasource": m.dbName,
		"count":      len(result),
	}))
	return result, nil
}

// Status returns every recorded migration row for this datasource,
// ordered by executed_at ascending. It is strictly observational: it neither
// creates the state store nor acquires the migration advisory lock. A state
// store that has not been created yet is reported as empty.
func (m *Migrator) Status(ctx context.Context) ([]Migration, error) {
	return m.readStateStore(ctx, false)
}

// Rollback rolls back the most recently applied migration (by
// executed_at) for this datasource. Returns nil when nothing has been
// applied.
func (m *Migrator) Rollback(ctx context.Context) (*Migration, error) {
	var result *Migration
	err := m.withLock(ctx, func(conn *stdsql.Conn) error {
		applied, err := listMigrations(ctx, conn, m.dbName, true)
		if err != nil {
			return err
		}
		if len(applied) == 0 {
			return nil
		}
		sortByExecutedAtDesc(applied)
		rec, err := m.rollback(ctx, conn, applied[0])
		if err != nil {
			return err
		}
		result = &rec
		return nil
	})
	return result, err
}

// RollbackTo rolls back every migration applied after the named
// migration in reverse execution order. The named migration itself
// remains applied.
func (m *Migrator) RollbackTo(ctx context.Context, name string) ([]Migration, error) {
	resolved, err := m.resolveName(name)
	if err != nil {
		return nil, err
	}
	var result []Migration
	err = m.withLock(ctx, func(conn *stdsql.Conn) error {
		applied, err := listMigrations(ctx, conn, m.dbName, true)
		if err != nil {
			return err
		}
		sortByExecutedAtDesc(applied)
		target := -1
		for i, a := range applied {
			if a.Name == resolved {
				target = i
				break
			}
		}
		if target == -1 {
			return perrors.Newf(CodeMigrationRollbackFailed,
				"migration %q not found in applied migrations for datasource %q", name, m.dbName)
		}
		for _, mig := range applied[:target] {
			rec, err := m.rollback(ctx, conn, mig)
			if err != nil {
				return err
			}
			result = append(result, rec)
		}
		return nil
	})
	return result, err
}

// Reset rolls back every applied migration for this datasource in
// reverse execution order. Not exposed via the migrate CLI.
func (m *Migrator) Reset(ctx context.Context) ([]Migration, error) {
	var result []Migration
	err := m.withLock(ctx, func(conn *stdsql.Conn) error {
		applied, err := listMigrations(ctx, conn, m.dbName, true)
		if err != nil {
			return err
		}
		sortByExecutedAtDesc(applied)
		for _, mig := range applied {
			rec, err := m.rollback(ctx, conn, mig)
			if err != nil {
				return err
			}
			result = append(result, rec)
		}
		return nil
	})
	return result, err
}

// Verify reads every applied row from the state store and compares its
// hash against the current registered definitions. Returns an empty
// migration.DriftReport when registry and state agree.
func (m *Migrator) Verify(ctx context.Context) (migration.DriftReport, error) {
	report := migration.DriftReport{Kind: migration.KindSQL}
	known := make(map[string]Definition, len(m.definitions))
	for _, d := range m.definitions {
		known[d.Name] = d
	}

	applied, err := m.readStateStore(ctx, true)
	if err != nil {
		return report, err
	}
	seen := make(map[string]struct{}, len(applied))
	for _, row := range applied {
		seen[row.Name] = struct{}{}
		def, ok := known[row.Name]
		if !ok {
			report.MissingFromRegistry = append(report.MissingFromRegistry,
				recordFromMigration(row, "", m.dbName, ""))
			continue
		}
		currentHash := sha256Hex(def.SQL)
		if currentHash != row.Hash {
			report.HashDrifts = append(report.HashDrifts, migration.HashDrift{
				Namespace:   def.Namespace,
				Name:        def.Name,
				StoredHash:  row.Hash,
				CurrentHash: currentHash,
				Target:      m.dbName,
			})
		}
	}
	for _, def := range m.definitions {
		if _, applied := seen[def.Name]; applied {
			continue
		}
		report.MissingFromStore = append(report.MissingFromStore, migration.Record{
			Kind:      migration.KindSQL,
			Namespace: def.Namespace,
			Name:      def.Name,
			Status:    migration.StatusPending,
			Hash:      sha256Hex(def.SQL),
			Source:    def.Source,
			Target:    m.dbName,
		})
	}
	return report, nil
}

// resolveName accepts either a namespaced full name or a bare basename
// and returns the unique full name. Ambiguous basenames produce an
// error naming all candidates.
func (m *Migrator) resolveName(name string) (string, error) {
	if name == "" {
		return "", nil
	}
	if strings.Contains(name, "/") {
		// Already namespaced — verify it exists.
		for _, d := range m.definitions {
			if d.Name == name {
				return name, nil
			}
		}
		return "", perrors.Newf(CodeMigrationInvalidDef,
			"migration %q not registered for datasource %q", name, m.dbName)
	}
	var matches []string
	for _, d := range m.definitions {
		if strings.HasSuffix(d.Name, "/"+name) {
			matches = append(matches, d.Name)
		}
	}
	switch len(matches) {
	case 0:
		return "", perrors.Newf(CodeMigrationInvalidDef,
			"migration %q not registered for datasource %q", name, m.dbName)
	case 1:
		return matches[0], nil
	default:
		sort.Strings(matches)
		return "", perrors.Newf(CodeMigrationInvalidDef,
			"migration basename %q is ambiguous; candidates: %s",
			name, strings.Join(matches, ", "))
	}
}

func (m *Migrator) withLock(ctx context.Context, fn func(*stdsql.Conn) error) error {
	if err := m.validate(); err != nil {
		return err
	}
	conn, err := m.db.Conn(ctx)
	if err != nil {
		return perrors.Wrapf(err, CodeMigrationStateStore, "acquire migration connection")
	}
	defer conn.Close() //nolint:errcheck // best-effort close

	if err := ensureStateStore(ctx, conn); err != nil {
		return err
	}

	lockKey := advisoryLockKey(m.dbName)
	lockCtx := ctx
	if m.lockTimeout > 0 {
		var cancel context.CancelFunc
		lockCtx, cancel = context.WithTimeout(ctx, m.lockTimeout)
		defer cancel()
	}
	if _, err := conn.ExecContext(lockCtx, acquireAdvisoryLockSQL, lockKey); err != nil {
		return perrors.Wrapf(err, CodeMigrationLockFailed,
			"acquire migration advisory lock",
			perrors.String("datasource", m.dbName))
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(releaseCtx, releaseAdvisoryLockSQL, lockKey); err != nil {
			// An independent operational signal (the lock outlives the run), kept
			// as its own record with the structured error rather than a
			// stringified one.
			m.log.Warn("failed to release migration advisory lock",
				migrationAttr(map[string]any{"datasource": m.dbName}),
				logger.ErrorAttr(err))
		}
	}()
	return fn(conn)
}

// readStateStore is the non-mutating counterpart to withLock. Status and drift
// inspection must work with the application runtime role, which deliberately
// has no CREATE privilege; only dedicated migration execution may initialize
// the state store or acquire the writer lock.
func (m *Migrator) readStateStore(ctx context.Context, successOnly bool) ([]Migration, error) {
	if err := m.validate(); err != nil {
		return nil, err
	}
	conn, err := m.db.Conn(ctx)
	if err != nil {
		return nil, perrors.Wrapf(err, CodeMigrationStateStore, "acquire migration connection")
	}
	defer conn.Close() //nolint:errcheck // best-effort close

	records, err := listMigrations(ctx, conn, m.dbName, successOnly)
	if isMissingMigrationStateStore(err) {
		return nil, nil
	}
	return records, err
}

func (m *Migrator) validate() error {
	if m.db == nil {
		return perrors.Newf(CodeMigrationInvalidDef, "migration database handle is nil")
	}
	return nil
}

func (m *Migrator) apply(ctx context.Context, conn *stdsql.Conn, def Definition) (Migration, error) {
	start := time.Now()
	hash := sha256Hex(def.SQL)
	downHash := ""
	if def.Down != "" {
		downHash = sha256Hex(def.Down)
	}
	id := protocolmigration.CanonicalID(m.dbName, def.Name)
	executedAt := time.Now().UTC().Format(time.RFC3339)

	m.log.Debug("executing migration", migrationAttr(map[string]any{
		"name":       def.Name,
		"datasource": m.dbName,
		"hash":       hash,
		"source":     def.Source,
	}))

	execErr := withTx(ctx, conn, func(tx *stdsql.Tx) error {
		if err := clearStatementTimeout(ctx, tx); err != nil {
			return err
		}
		if err := applySearchPath(ctx, tx, m.schema); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, def.SQL); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, upsertSuccessSQL,
			id, m.dbName, def.Name, hash, executedAt,
			time.Since(start).Milliseconds(),
			nullableString(def.Down), nullableString(downHash),
		)
		return err
	})

	// One elapsed measurement for the terminal record, the bookkeeping row, and
	// the returned Migration, so all three agree.
	elapsed := time.Since(start)

	if execErr != nil {
		m.recordFailure(ctx, conn, id, def, hash, downHash, executedAt, elapsed.Milliseconds(), execErr)
		// The per-migration terminal record. Logged once here with the raw cause;
		// the caller still returns the wrapped error, which aborts the run.
		logMigrationFailed(ctx, m.log, def.Name, m.dbName, elapsed, execErr)
		return Migration{}, perrors.Wrapf(execErr, CodeMigrationApplyFailed,
			"apply migration",
			perrors.String("name", def.Name),
			perrors.String("datasource", m.dbName))
	}

	logMigrationApplied(ctx, m.log, def.Name, m.dbName, elapsed)

	return Migration{
		ID:              id,
		DBName:          m.dbName,
		Name:            def.Name,
		Hash:            hash,
		ExecutedAt:      executedAt,
		ExecutionTimeMs: elapsed.Milliseconds(),
		Success:         1,
		DownSQL:         def.Down,
		DownHash:        downHash,
	}, nil
}

func (m *Migrator) recordFailure(
	ctx context.Context,
	conn *stdsql.Conn,
	id string,
	def Definition,
	hash, downHash, executedAt string,
	elapsedMs int64,
	cause error,
) {
	_, err := conn.ExecContext(ctx, upsertFailureSQL,
		id, m.dbName, def.Name, hash, executedAt, elapsedMs,
		cause.Error(), nullableString(def.Down), nullableString(downHash),
	)
	if err != nil {
		// Bookkeeping failure — an independent signal from the migration's own
		// terminal record, which the caller emits with the apply error.
		m.log.Warn("failed to record migration failure",
			migrationAttr(map[string]any{
				"name":       def.Name,
				"datasource": m.dbName,
			}),
			logger.ErrorAttr(err),
		)
	}
}

func (m *Migrator) rollback(ctx context.Context, conn *stdsql.Conn, mig Migration) (Migration, error) {
	downSQL := mig.DownSQL
	source := "database"
	if downSQL == "" {
		for _, def := range m.definitions {
			if def.Name == mig.Name && def.Down != "" {
				downSQL = def.Down
				source = "registry"
				break
			}
		}
	}
	if downSQL == "" {
		return Migration{}, perrors.Newf(CodeMigrationRollbackFailed,
			"migration %q has no down SQL — cannot roll back", mig.Name)
	}
	if mig.DownSQL != "" && mig.DownHash != "" && sha256Hex(mig.DownSQL) != mig.DownHash {
		return Migration{}, perrors.Newf(CodeMigrationRollbackFailed,
			"rollback integrity check failed for %q — stored down_sql has been tampered with",
			mig.Name)
	}

	m.log.Debug("executing rollback", migrationAttr(map[string]any{
		"name":       mig.Name,
		"datasource": m.dbName,
		"source":     source,
	}))

	rolledBack := false
	err := withTx(ctx, conn, func(tx *stdsql.Tx) error {
		var claimed string
		row := tx.QueryRowContext(ctx,
			`DELETE FROM migration.migrations WHERE id = $1 AND success = 1 RETURNING id`,
			mig.ID)
		if err := row.Scan(&claimed); err != nil {
			if stderrors.Is(err, stdsql.ErrNoRows) {
				return nil
			}
			return err
		}
		if err := clearStatementTimeout(ctx, tx); err != nil {
			return err
		}
		if err := applySearchPath(ctx, tx, m.schema); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, downSQL); err != nil {
			return err
		}
		rolledBack = true
		return nil
	})
	if err != nil {
		return Migration{}, perrors.Wrapf(err, CodeMigrationRollbackFailed,
			"rollback migration",
			perrors.String("name", mig.Name),
			perrors.String("datasource", m.dbName))
	}
	if !rolledBack {
		mig.ExecutedAt = time.Now().UTC().Format(time.RFC3339)
		return mig, nil
	}
	mig.ExecutedAt = time.Now().UTC().Format(time.RFC3339)
	return mig, nil
}
