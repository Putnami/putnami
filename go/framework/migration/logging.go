package migration

import (
	"log/slog"

	"go.putnami.dev/logger"
)

// ---------------------------------------------------------------------------
// Migration boundary log contract — the registry's half of it.
//
// The record shapes of the migration boundary are built in exactly one place per
// module, so no call site can invent a logger name or a field name: the SQL
// migrator and runner records live in go/framework/database/database_logging.go,
// the registry's records live here, and the TypeScript twins are
// typescript/framework/{database/src/observability/database-logging.ts,
// migration/src/logging.ts}. The contract itself is normative in
// protocols/logging/conformance (manifest + README).
// ---------------------------------------------------------------------------

// loggerName is the pinned logger name of the migration boundary
// (protocols/logging/conformance, "Pinned logger names": dots, never colons).
// Registry records share it with the SQL migrator and runner so an operator
// filters the whole migration boundary with one logger name.
const loggerName = "database.migration"

// migrationLoggerFrom derives the pinned migration logger from root. The
// registry resolves it once at construction.
func migrationLoggerFrom(root *logger.Logger) *logger.Logger {
	return root.Named(loggerName)
}

// migrationAttr renders a record's "migration" group as one closed attr: a
// record carries exactly the camelCase fields it declares, and an attr replaces
// a same-named context key at the sink.
func migrationAttr(group map[string]any) slog.Attr {
	return slog.Any("migration", group)
}
