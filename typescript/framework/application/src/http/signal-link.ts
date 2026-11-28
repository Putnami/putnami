/**
 * One plain signal that aborts with the first of its sources to abort, with
 * that source's reason, until {@link release} detaches it from them.
 *
 * It stands in for `AbortSignal.any` on the request path. Bun keeps a signal
 * `AbortSignal.any` builds, listeners included, alive while one of its
 * sources is alive and not aborted, and a served request's signal outlives
 * the call that ends without aborting it: any listener on the combined signal
 * that reaches the request then keeps the request alive. This link follows
 * each source through one listener (the link itself, as an event listener
 * object) that {@link release} removes once the work it bounds settles, so
 * nothing on a source refers to the signal afterwards.
 */
export class SignalLink {
  private readonly controller = new AbortController();
  private readonly sources: AbortSignal[];

  constructor(sources: ReadonlyArray<AbortSignal | undefined>) {
    this.sources = sources.filter((source): source is AbortSignal => source !== undefined);
    const aborted = this.sources.find((source) => source.aborted);
    if (aborted) {
      this.controller.abort(aborted.reason);
      this.sources = [];
      return;
    }
    for (const source of this.sources) source.addEventListener('abort', this, { once: true });
  }

  get signal(): AbortSignal {
    return this.controller.signal;
  }

  /** A source aborted: the link aborts with its reason and leaves the others. */
  handleEvent(event: Event): void {
    this.controller.abort((event.target as AbortSignal).reason);
    this.release();
  }

  /** The bounded work settled: the link leaves its sources, unaborted. */
  release(): void {
    for (const source of this.sources) source.removeEventListener('abort', this);
    this.sources.length = 0;
  }
}
