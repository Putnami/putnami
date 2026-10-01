/**
 * Centralized SQL identifier safety.
 *
 * Identifiers (table and column names) cannot be parameterized, so every place
 * that interpolates one into SQL must assert it is a safe identifier at the
 * point of use — not rely on a distant constructor-time check. `assertSafeIdentifier`
 * is therefore called both when a table/column is registered (EntityHelper
 * construction) and again whenever a name is emitted into a query (WHERE,
 * INSERT, ON CONFLICT, ORDER BY).
 */

/** A PostgreSQL identifier: a letter or underscore followed by word characters. */
const SAFE_IDENTIFIER = /^[a-zA-Z_][a-zA-Z0-9_]*$/;

/**
 * Throw if `name` is not a safe, unquoted SQL identifier.
 *
 * @param name - The candidate table or column name.
 * @param context - Human-readable location used in the error message.
 */
export function assertSafeIdentifier(name: string, context: string): void {
  if (!SAFE_IDENTIFIER.test(name)) {
    throw new Error(`Unsafe SQL identifier in ${context}: "${name}"`);
  }
}

/** True when `name` is a safe, unquoted SQL identifier. */
export function isSafeIdentifier(name: string): boolean {
  return SAFE_IDENTIFIER.test(name);
}

/**
 * Assert `name` is a safe identifier and return it wrapped in PostgreSQL
 * double-quote syntax, for interpolation into a raw SQL string built with
 * `sql.unsafe(...)` (where postgres.js's `sql(name)` helper is unavailable).
 * The safe-identifier check forbids embedded quotes and dots, so wrapping is
 * injection-safe. Mirrors the Go adapter's `quoteIdentifier`.
 */
export function quoteIdentifier(name: string, context: string): string {
  assertSafeIdentifier(name, context);
  return `"${name}"`;
}
