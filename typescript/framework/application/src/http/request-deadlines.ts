import { useLogger } from '@putnami/runtime';

/** One request waiting on its deadline in a {@link RequestDeadlines} queue. */
export interface RequestDeadline {
  /** When the request expires, on the `performance.now()` clock; set when it is queued. */
  at: number;
  /** Called, as a method of the entry, when the deadline passes first. */
  expire(): void;
  prev: RequestDeadline | undefined;
  next: RequestDeadline | undefined;
  queued: boolean;
}

/**
 * The deadlines of the requests one server serves. Every request gets the same
 * timeout, so the deadlines expire in the order the requests arrived: a list
 * kept in arrival order is kept in deadline order, and one timer, armed for its
 * head, serves every request. Adding and removing a request is O(1) and costs
 * no timer of its own.
 */
export class RequestDeadlines {
  private head: RequestDeadline | undefined;
  private tail: RequestDeadline | undefined;
  private timer: ReturnType<typeof setTimeout> | undefined;

  constructor(private readonly timeoutMs: number) {}

  /**
   * Queues an entry the caller owns, such as the request itself, so queuing
   * allocates nothing: it expires `timeoutMs` after `from` (now by default,
   * or when the request arrived, on the `performance.now()` clock) unless
   * removed first. An entry never expires before the one queued ahead of it,
   * so the queue stays in deadline order.
   */
  enqueue(entry: RequestDeadline, from: number = performance.now()): void {
    const at = from + this.timeoutMs;
    const tail = this.tail;
    entry.at = tail !== undefined && tail.at > at ? tail.at : at;
    entry.prev = tail;
    entry.next = undefined;
    entry.queued = true;
    if (tail) {
      tail.next = entry;
    } else {
      this.head = entry;
    }
    this.tail = entry;
    if (this.timer === undefined) {
      this.arm(entry.at);
    }
  }

  /** Removes a request that answered before its deadline. Removing it twice does nothing. */
  remove(entry: RequestDeadline): void {
    if (!entry.queued) return;
    entry.queued = false;
    if (entry.prev) {
      entry.prev.next = entry.next;
    } else {
      this.head = entry.next;
    }
    if (entry.next) {
      entry.next.prev = entry.prev;
    } else {
      this.tail = entry.prev;
    }
    entry.prev = undefined;
    entry.next = undefined;
  }

  /** The number of requests still waiting on their deadline. */
  get size(): number {
    let size = 0;
    for (let entry = this.head; entry; entry = entry.next) size++;
    return size;
  }

  private arm(at: number): void {
    // A removed head leaves the timer armed for its deadline: when it fires,
    // the sweep finds the next live head and arms again for it.
    this.timer = setTimeout(this.sweep, Math.max(1, Math.ceil(at - performance.now())));
    // The server keeps the process alive; the timer never has to.
    (this.timer as { unref?: () => void }).unref?.();
  }

  private readonly sweep = (): void => {
    this.timer = undefined;
    const now = performance.now();
    while (this.head !== undefined && this.head.at <= now) {
      const entry = this.head;
      this.remove(entry);
      try {
        entry.expire();
      } catch (error) {
        // One expiry that fails must not keep the next requests from theirs.
        useLogger('http').error('Request deadline expiry failed', error);
      }
    }
    if (this.head !== undefined) {
      this.arm(this.head.at);
    }
  };
}

/**
 * A controller that also aborts when its source signal aborts, with the
 * source's reason. It listens to the source as an event listener object
 * (`handleEvent`), so following the source costs no closure and no listener
 * options: the cheapest form Bun offers for a signal that ends on either cause.
 */
export class LinkedAbortController extends AbortController {
  private readonly source: AbortSignal;

  constructor(source: AbortSignal) {
    super();
    this.source = source;
  }

  /** Starts following the source: aborts now when it already aborted. */
  follow(): void {
    if (this.source.aborted) {
      this.abort(this.source.reason);
    } else {
      this.source.addEventListener('abort', this);
    }
  }

  /** The source aborted: abort with its reason. */
  handleEvent(): void {
    this.abort(this.source.reason);
  }
}
