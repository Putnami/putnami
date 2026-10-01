import { Logger } from './logger';
import type { LogEntry, LogSink } from './logger.type';

/**
 * In-memory sink that captures entries for assertions in tests.
 */
export class MemorySink implements LogSink {
  entries: LogEntry[] = [];

  write(entry: LogEntry): void {
    this.entries.push(entry);
  }

  clear(): void {
    this.entries.length = 0;
  }
}

/**
 * Logger backed by a MemorySink. Use in tests to assert on log output.
 *
 * @example
 * ```typescript
 * const logger = new MemoryLogger();
 * logger.info('hello');
 * expect(logger.entries).toHaveLength(1);
 * expect(logger.entries[0].message).toBe('hello');
 * ```
 */
export class MemoryLogger extends Logger {
  readonly sink: MemorySink;

  constructor(name?: string) {
    const sink = new MemorySink();
    super([sink], name, {}, true);
    this.sink = sink;
  }

  get entries(): LogEntry[] {
    return this.sink.entries;
  }

  clear(): void {
    this.sink.clear();
  }
}
