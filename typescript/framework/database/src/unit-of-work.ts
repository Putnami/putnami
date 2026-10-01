import {
  commit as commitAmbientTransaction,
  isTransactionActive,
  rollback as rollbackAmbientTransaction,
  type TransactionOptions,
  withTransaction,
} from './transaction';

/**
 * A DI request-scoped transaction coordinator. It owns the transaction boundary
 * for one request: {@link begin} flags the ambient context transactional (see
 * {@link withTransaction}) so every repository write — whether it comes from an
 * injected repository or a plain `new Repository(...)` — lazily joins one
 * transaction per participating datasource, and {@link commit}/{@link rollback}
 * finalize them together at the request boundary.
 *
 * # Reconciliation with the ambient transaction (single committer)
 *
 * The framework already tracks the per-datasource reserved connections on the
 * request context under a single-owner `TX_KEY` state; a `new`'d repository
 * resolves its connection from that same ambient state. The UnitOfWork does NOT
 * duplicate that state — it delegates to the ambient {@link withTransaction},
 * {@link commitAmbientTransaction}, and {@link rollbackAmbientTransaction}. It
 * becomes the *sole* committer by:
 *
 *   - opening the transaction itself via {@link begin}, so any nested
 *     `runInTransaction()` in the handler sees an already-active transaction and
 *     joins it (never committing the outer unit — nesting joins the outer unit);
 *   - guarding {@link commit}/{@link rollback} with a `finished` latch so the
 *     boundary finalizes the unit at most once; and
 *   - delegating only while {@link isTransactionActive} is true, so a finalize
 *     after the ambient state was already resolved is a safe no-op rather than a
 *     double-commit.
 *
 * # Multi-datasource semantics — best-effort, NOT two-phase commit
 *
 * A unit spanning a single datasource is atomic. Spanning multiple datasources
 * it is best-effort: the ambient commit attempts each datasource's `COMMIT`,
 * always releasing every reserved connection, and surfaces any failure as a
 * (possibly aggregate) error — a partial commit is reported as an error, never
 * as success. It is explicitly not 2PC; callers needing cross-datasource
 * atomicity must not span datasources in one unit.
 *
 * Mirrors the Go adapter's `database.UnitOfWork`.
 */
export class UnitOfWork {
  /** Forces a rollback at the boundary even when the handler reports success. */
  private rollbackOnly = false;
  /** Latches after the first commit/rollback so the unit finalizes at most once. */
  private finished = false;
  /** Whether {@link begin} has already flagged the ambient context transactional. */
  private opened = false;

  /**
   * @param options Transaction options forwarded to {@link withTransaction} on
   *   {@link begin} — notably `timeoutMs`, which bounds the whole request
   *   transaction so a wedged handler cannot pin a pooled connection forever.
   */
  constructor(private readonly options: TransactionOptions = {}) {}

  /**
   * Idempotently flag the ambient context transactional so repository writes
   * join one transaction per datasource. Zero-cost until the first write: no
   * connection is reserved and no `BEGIN` is issued until a repository actually
   * writes (see {@link withTransaction}).
   */
  begin(): void {
    if (this.opened) {
      return;
    }
    withTransaction(this.options);
    this.opened = true;
  }

  /**
   * Escape hatch: mark the unit so the boundary rolls back every enrolled
   * transaction even when the handler returns successfully. For a handler that
   * detects a business failure but still returns a non-error response. Safe to
   * call at any point during the request.
   */
  setRollbackOnly(): void {
    this.rollbackOnly = true;
  }

  /** Whether {@link setRollbackOnly} has been called. */
  get isRollbackOnly(): boolean {
    return this.rollbackOnly;
  }

  /**
   * Commit every enrolled datasource transaction (single-datasource atomic;
   * multi-datasource best-effort, NOT 2PC — a partial-commit failure is surfaced
   * as an error). A unit marked rollback-only rolls back instead. Idempotent — a
   * second call, or a call after {@link rollback}, is a no-op.
   */
  async commit(): Promise<void> {
    if (this.finished) {
      return;
    }
    this.finished = true;
    // The ambient state may already be resolved (a timeout rollback, or an
    // explicit commit inside the handler); delegating then would throw
    // "no active transaction". Skip instead — there is nothing left to finalize.
    if (!isTransactionActive()) {
      return;
    }
    if (this.rollbackOnly) {
      await rollbackAmbientTransaction();
      return;
    }
    await commitAmbientTransaction();
  }

  /**
   * Roll back every enrolled datasource transaction, releasing all reserved
   * connections. Idempotent — a second call, or a call after {@link commit}, is
   * a no-op.
   *
   * @param cause The value that triggered the rollback, if any. It is threaded
   *   to the ambient rollback so the boundary telemetry classifies it into a
   *   secret-free cause code (never the raw message). Mirrors the Go adapter's
   *   `FinalizeScope` classifying the request outcome before delegating.
   */
  async rollback(cause?: unknown): Promise<void> {
    if (this.finished) {
      return;
    }
    this.finished = true;
    if (!isTransactionActive()) {
      return;
    }
    await rollbackAmbientTransaction(cause);
  }

  /**
   * Reconcile the unit with the request outcome: commit when `outcome` is
   * nullish (and the unit is not rollback-only), roll back otherwise. Mirrors
   * the Go adapter's `FinalizeScope`. Idempotent.
   */
  async finalize(outcome?: unknown): Promise<void> {
    if (outcome !== undefined && outcome !== null) {
      // Thread the outcome so the boundary telemetry records its classified,
      // secret-free cause rather than the generic explicit-rollback sentinel.
      await this.rollback(outcome);
      return;
    }
    await this.commit();
  }
}
