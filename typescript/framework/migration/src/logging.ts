// ---------------------------------------------------------------------------
// Migration boundary log contract — the registry's half of it.
//
// The record shapes of the migration boundary are built in exactly one place per
// package, so no call site can invent a logger name or a field name: the SQL
// migrator and runner records live in
// `@putnami/database`'s src/observability/database-logging.ts, the registry's
// records live here, and the Go twins are
// `go/framework/{database/database_logging.go,migration/logging.go}`. The
// contract itself is normative in `protocols/logging/conformance`.
// ---------------------------------------------------------------------------

/**
 * Pinned logger name of the migration boundary (contract: "Pinned logger
 * names" — dots, never colons). Registry records share it with the SQL migrator
 * and runner so an operator filters the whole migration boundary with one name.
 */
export const MIGRATION_LOGGER = 'database.migration';

/**
 * Renders a record's `migration` group as the single structured data param of a
 * log call: a record carries exactly the camelCase fields it declares, and the
 * JSON sink flattens the lone object over the entry's context.
 */
export function migrationFields(group: Record<string, unknown>): { migration: Record<string, unknown> } {
  return { migration: group };
}
