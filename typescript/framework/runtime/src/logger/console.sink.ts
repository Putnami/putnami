import type { LogEntry, LogSink } from './logger.type';

/**
 * Text output sink. Writes human-readable log lines to stdout/stderr.
 *
 * Format: `[traceId] [LEVEL] [logger] message ...data`
 * Error entries include the full stack trace.
 */
export class ConsoleSink implements LogSink {
  write(entry: LogEntry): void {
    const parts: string[] = [];

    if (entry.traceId) {
      parts.push(`[${entry.traceId}]`);
    }
    parts.push(`[${entry.level.toUpperCase()}]`);
    if (entry.logger) {
      parts.push(`[${entry.logger}]`);
    }
    parts.push(entry.message);

    // Append structured data inline
    if (entry.data) {
      for (const d of entry.data) {
        if (typeof d === 'string') {
          parts.push(d);
        } else {
          try {
            parts.push(JSON.stringify(d));
          } catch {
            parts.push(String(d));
          }
        }
      }
    }

    // Append context as JSON if present
    if (entry.context && Object.keys(entry.context).length > 0) {
      try {
        parts.push(JSON.stringify(entry.context));
      } catch {
        // skip
      }
    }

    const line = parts.join(' ');

    if (entry.level === 'error' || entry.level === 'warn') {
      process.stderr.write(`${line}\n`);
    } else {
      process.stdout.write(`${line}\n`);
    }

    // Print stack trace for errors on stderr
    if (entry.error?.stack) {
      process.stderr.write(`${entry.error.stack}\n`);
    }
  }
}
