/**
 * Async iterable that bridges push-based events (WebSocket messages)
 * to pull-based iteration (for-await loops in handlers).
 */
/** Default upper bound on buffered messages before back-pressure drops apply. */
const DEFAULT_MAX_QUEUE_LENGTH = 1024;

export class MessageStream<T> implements AsyncIterable<T> {
  private queue: T[] = [];
  private waiting: ((result: IteratorResult<T>) => void) | null = null;
  private closed = false;
  private dropped = 0;

  /**
   * @param maxQueueLength - Upper bound on buffered messages when no consumer is
   *   pulling. A fast producer with an absent/slow consumer would otherwise grow
   *   memory without limit (client-controlled DoS); once the bound is reached new
   *   messages are dropped rather than buffered.
   */
  constructor(private readonly maxQueueLength: number = DEFAULT_MAX_QUEUE_LENGTH) {}

  /** Number of messages dropped due to the queue bound. */
  get droppedCount(): number {
    return this.dropped;
  }

  /** Push a new message into the stream. Resumes the waiting consumer or enqueues. */
  push(message: T): void {
    if (this.closed) return;
    if (this.waiting) {
      const resolve = this.waiting;
      this.waiting = null;
      resolve({ value: message, done: false });
    } else if (this.queue.length < this.maxQueueLength) {
      this.queue.push(message);
    } else {
      // Bounded buffer: drop newest to keep memory bounded under back-pressure.
      this.dropped++;
    }
  }

  /** Close the stream. Ends any active for-await loop. */
  close(): void {
    this.closed = true;
    if (this.waiting) {
      const resolve = this.waiting;
      this.waiting = null;
      // Iterator protocol requires `value` to be T even when done=true; undefined is safe here
      resolve({ value: undefined as unknown as T, done: true });
    }
  }

  [Symbol.asyncIterator](): AsyncIterator<T> {
    return {
      next: (): Promise<IteratorResult<T>> => {
        if (this.queue.length > 0) {
          return Promise.resolve({ value: this.queue.shift()!, done: false });
        }
        if (this.closed) {
          // Iterator protocol requires `value` to be T even when done=true; undefined is safe here
          return Promise.resolve({ value: undefined as unknown as T, done: true });
        }
        return new Promise<IteratorResult<T>>((resolve) => {
          this.waiting = resolve;
        });
      },
    };
  }
}
