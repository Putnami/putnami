import { items } from './store';

/**
 * One revision of the catalog change feed. `cursor` is the provider's own
 * opaque position after this change: a consumer hands it back unread on a
 * continuation and never parses it. `revision` is the sequence a reader
 * compares to see for itself that nothing was skipped and nothing arrived
 * twice.
 */
export interface ItemChange {
  cursor: string;
  revision: number;
  id: string;
  name: string;
}

/** The declared shape of one change, shared by the route and the generated clients. */
export const itemChangeSchema = {
  cursor: String,
  revision: Number,
  id: String,
  name: String,
};

/**
 * The durable change feed of the catalog: every revision it ever issued, in
 * order, shared by every provider instance of one process. It is the only
 * state a continuation relies on. An instance keeps nothing about a stream it
 * served, so a continuation that lands on another instance is placed by the
 * cursor alone, and a fresh process seeds the same revisions from the same
 * catalog.
 */
export class ChangeLog {
  private readonly changes: ItemChange[] = [];
  private waiters: (() => void)[] = [];

  /** Record one change and wake every stream waiting for it. */
  append(id: string, name: string): ItemChange {
    const revision = this.changes.length + 1;
    const change: ItemChange = { cursor: `r${revision}`, revision, id, name };
    this.changes.push(change);
    for (const waiter of this.waiters.splice(0)) waiter();
    return change;
  }

  /** The revision of the latest change: where a feed opened now will complete. */
  head(): number {
    return this.changes.length;
  }

  /**
   * Resolve a cursor this log issued to the number of changes before the one
   * that follows it. An absent cursor is the start; a cursor the log never
   * issued is refused.
   */
  position(cursor: string | undefined): number | undefined {
    if (cursor === undefined || cursor === '') return 0;
    if (!/^r[1-9]\d*$/.test(cursor)) return undefined;
    const revision = Number(cursor.slice(1));
    return revision <= this.changes.length ? revision : undefined;
  }

  /**
   * Wait for the change after position `after`. Resolves to nothing when the
   * signal ended first: the stream was abandoned by its consumer or drained by
   * its server.
   */
  async next(after: number, signal: AbortSignal): Promise<ItemChange | undefined> {
    for (;;) {
      if (after < this.changes.length) return this.changes[after];
      if (signal.aborted) return undefined;
      // biome-ignore lint/performance/noAwaitInLoops: the feed waits for the next change
      await new Promise<void>((resolve) => {
        // Whichever of the change and the abort comes first removes the
        // other, so a long-lived stream holds one listener at a time.
        const wake = () => {
          signal.removeEventListener('abort', wake);
          this.waiters = this.waiters.filter((waiter) => waiter !== wake);
          resolve();
        };
        this.waiters.push(wake);
        signal.addEventListener('abort', wake, { once: true });
      });
    }
  }
}

/** The change feed every instance of this provider serves. */
export const changes = new ChangeLog();
for (const item of items.values()) changes.append(item.id, item.name);
