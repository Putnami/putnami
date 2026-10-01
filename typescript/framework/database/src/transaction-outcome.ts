/**
 * Canonical transaction Outcome — TypeScript hand-mirror.
 *
 * These values mirror the Go module `go.putnami.dev/protocol/transaction` (the
 * JSON schemas under `protocols/transaction/schemas`). That protocol publishes
 * `publish: ["go"]` only, so there is no generated TypeScript binding; this file
 * hand-mirrors the closed `Outcome` enum the same way
 * `postgres/binding.ts` hand-mirrors `protocols/database`.
 *
 * The string values MUST match the Go enum (`transaction.Outcome*`) and the
 * schema's `outcome` enum byte-for-byte, so the Go and TypeScript database
 * adapters report identical result codes for the same scenario. A mismatch here
 * is a cross-language protocol drift, not a local naming choice.
 */

/**
 * Outcome is the typed, closed result code of running a transactional unit of
 * work (a CAS / consume-once / rotation helper). Callers switch exhaustively on
 * a stable, cross-language taxonomy instead of a bare row count or a raw driver
 * error.
 */
export const Outcome = {
  /** The unit of work committed successfully (the transition ran). */
  Applied: 'applied',
  /**
   * The work targeted a resource that was already consumed (an idempotency key,
   * a once-only token, or a unique-violation on insert), so it was rejected as a
   * conflict rather than re-applied.
   */
  AlreadyConsumedConflict: 'already-consumed-conflict',
  /** A required target row/resource did not exist, so the work was not applied. */
  NotFound: 'not-found',
  /**
   * The transaction aborted with a serialization/deadlock failure the caller may
   * safely retry. This is the only outcome for which {@link outcomeRetryable} is
   * true.
   */
  RetryableSerializationFailure: 'retryable-serialization-failure',
} as const;

export type Outcome = (typeof Outcome)[keyof typeof Outcome];

/**
 * Whether re-running the unit of work may succeed. Only a serialization/deadlock
 * failure is inherently retryable; every other outcome is terminal. Mirrors
 * `transaction.Outcome.Retryable()` in Go.
 */
export function outcomeRetryable(o: Outcome): boolean {
  return o === Outcome.RetryableSerializationFailure;
}
