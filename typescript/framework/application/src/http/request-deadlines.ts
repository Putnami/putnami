import { useLogger } from '@putnami/runtime';

/** One request waiting on its deadline in a {@link RequestDeadlines} queue. */
export interface RequestDeadline {
  /** When the request expires, on the `performance.now()` clock. */
  readonly at: number;
  readonly expire: () => void;
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

  /** Queues a request that expires `timeoutMs` from now by calling `expire`, unless removed first. */
  add(expire: () => void): RequestDeadline {
    const entry: RequestDeadline = {
      at: performance.now() + this.timeoutMs,
      expire,
      prev: this.tail,
      next: undefined,
      queued: true,
    };
    if (this.tail) {
      this.tail.next = entry;
    } else {
      this.head = entry;
    }
    this.tail = entry;
    if (this.timer === undefined) {
      this.arm(entry.at);
    }
    return entry;
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
