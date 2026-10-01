import type { LogEntry, LogSink } from './logger.type';

export interface BufferSinkOptions {
  /** Delegate sink — receives entries on flush. */
  sink: LogSink;
  /** Flush when a context buffer reaches this many entries. Default: 100. */
  maxSize?: number;
  /** Flush all buffers every N ms. Default: 5000. */
  flushInterval?: number;
}

const DEFAULT_MAX_SIZE = 100;
const DEFAULT_FLUSH_INTERVAL = 5000;
const NO_TRACE_KEY = '__no_trace__';

/**
 * Buffered sink that groups log entries by traceId.
 *
 * Flush triggers:
 * - Buffer for a traceId exceeds `maxSize`
 * - Timer fires every `flushInterval` ms
 * - An error-level entry arrives (immediate flush of that context)
 * - Explicit `flush()` or `close()` call
 */
export class BufferSink implements LogSink {
  private sink: LogSink;
  private maxSize: number;
  private buffers = new Map<string, LogEntry[]>();
  private timer: ReturnType<typeof setInterval> | undefined;

  constructor(options: BufferSinkOptions) {
    this.sink = options.sink;
    this.maxSize = options.maxSize ?? DEFAULT_MAX_SIZE;

    const interval = options.flushInterval ?? DEFAULT_FLUSH_INTERVAL;
    this.timer = setInterval(() => this.flushSync(), interval);
    // Don't hold the process open just for log flushing
    if (this.timer && typeof this.timer === 'object' && 'unref' in this.timer) {
      this.timer.unref();
    }
  }

  write(entry: LogEntry): void {
    const key = entry.traceId || NO_TRACE_KEY;

    // Entries without traceId pass through immediately — no grouping possible
    if (!entry.traceId) {
      this.sink.write(entry);
      return;
    }

    let buffer = this.buffers.get(key);
    if (!buffer) {
      buffer = [];
      this.buffers.set(key, buffer);
    }
    buffer.push(entry);

    // Error-level: flush the entire context buffer immediately
    if (entry.level === 'error') {
      this.flushKey(key);
      return;
    }

    // Size trigger
    if (buffer.length >= this.maxSize) {
      this.flushKey(key);
    }
  }

  async flush(): Promise<void> {
    this.flushSync();
    await this.sink.flush?.();
  }

  async close(): Promise<void> {
    if (this.timer) {
      clearInterval(this.timer);
      this.timer = undefined;
    }
    this.flushSync();
    await this.sink.close?.();
  }

  private flushSync(): void {
    for (const key of this.buffers.keys()) {
      this.flushKey(key);
    }
  }

  private flushKey(key: string): void {
    const buffer = this.buffers.get(key);
    if (!buffer || buffer.length === 0) {
      return;
    }
    for (const entry of buffer) {
      this.sink.write(entry);
    }
    this.buffers.delete(key);
  }
}
