import type { LogEntry, LogLevel, LogSink } from './logger.type';

const SEVERITY_MAP: Record<LogLevel, string> = {
  debug: 'DEBUG',
  info: 'INFO',
  warn: 'WARNING',
  error: 'ERROR',
};

/**
 * Top-level record keys owned by the sink. User-supplied `context`/`data`
 * fields that collide with these are skipped so they cannot clobber the
 * log's own structured fields (severity/message/timestamp/...).
 */
const RESERVED_KEYS = new Set<string>([
  'severity',
  'message',
  'timestamp',
  'logger',
  'traceId',
  'logging.googleapis.com/trace',
  'error',
  'data',
]);

/**
 * Structured JSON sink. Outputs one JSON line per log entry.
 * Compatible with Google Cloud Logging / Cloud Run structured logging.
 *
 * @see https://cloud.google.com/logging/docs/structured-logging
 */
export class JsonSink implements LogSink {
  write(entry: LogEntry): void {
    // biome-ignore lint: This is the JSON sink — console.log is intentional
    console.log(stringifyRecord(buildJsonRecord(entry)));
  }
}

/**
 * Build the flat JSON record the {@link JsonSink} prints for one entry, with the
 * same reserved-key protection and single-data-object flattening, so the
 * conformance harness can compare the exact record the sink emits.
 */
export function buildJsonRecord(entry: LogEntry): Record<string, unknown> {
  const record: Record<string, unknown> = {
    severity: SEVERITY_MAP[entry.level],
    message: entry.message,
    timestamp: entry.timestamp,
  };

  if (entry.logger) {
    record['logger'] = entry.logger;
  }

  if (entry.traceId) {
    const gcpProject = process.env['GOOGLE_CLOUD_PROJECT'] || process.env['GCP_PROJECT'];
    if (gcpProject && !entry.traceId.includes('/')) {
      record['logging.googleapis.com/trace'] = `projects/${gcpProject}/traces/${entry.traceId}`;
    } else {
      record['traceId'] = entry.traceId;
    }
  }

  // Spread context fields as top-level entries for Cloud Logging labels.
  // Reserved keys are skipped so user context cannot clobber the log's own
  // structured fields (severity/message/timestamp/...).
  if (entry.context) {
    for (const [key, value] of Object.entries(entry.context)) {
      if (RESERVED_KEYS.has(key)) {
        continue;
      }
      record[key] = value;
    }
  }

  if (entry.error) {
    record['error'] = entry.error;
  }

  if (entry.data && entry.data.length > 0) {
    // If single data element is an object, merge it as top-level fields
    if (
      entry.data.length === 1 &&
      typeof entry.data[0] === 'object' &&
      entry.data[0] !== null &&
      !(entry.data[0] instanceof Error)
    ) {
      for (const [key, value] of Object.entries(entry.data[0] as Record<string, unknown>)) {
        if (RESERVED_KEYS.has(key)) {
          continue;
        }
        record[key] = value;
      }
    } else {
      record['data'] = entry.data;
    }
  }

  return record;
}

/**
 * Serialize a log record to a single JSON line, never throwing.
 *
 * `redact()` only descends into plain objects/arrays, so a class instance with
 * a back-reference (e.g. a thrown driver error holding a circular `socket`)
 * reaches this point untouched and would make a plain `JSON.stringify` throw
 * "Converting circular structure to JSON" — which, on the production-default
 * JSON path, would crash the caller from inside a logging call. It first tries a
 * normal stringify, then retries with a circular-safe replacer, and finally falls
 * back to a minimal record so a log call can never bring down its caller.
 */
function stringifyRecord(record: Record<string, unknown>): string {
  try {
    return JSON.stringify(record);
  } catch {
    try {
      return JSON.stringify(record, circularSafeReplacer());
    } catch {
      // Last resort: emit a minimal, guaranteed-serializable record so the
      // severity/message/timestamp are never lost even if a value is hostile
      // to JSON (e.g. a BigInt or a throwing getter survived the replacer).
      return JSON.stringify({
        severity: record['severity'],
        message: record['message'],
        timestamp: record['timestamp'],
        logSerializationError: 'record could not be serialized to JSON',
      });
    }
  }
}

/** Replacement written in place of a value already present on the current path. */
const CIRCULAR = '[Circular]';

/**
 * A `JSON.stringify` replacer that emits {@link CIRCULAR} for any object that
 * recurs on the active serialization *path* (a true cycle), breaking circular
 * references without falsely flagging a value that merely appears twice in
 * sibling positions.
 *
 * `JSON.stringify` invokes the replacer with `this` bound to the object that
 * holds `key`, depth-first. We keep an explicit ancestor stack of
 * `[holder, emitted-value]` pairs: before handling a value we unwind the stack
 * to the entry whose emitted value is the current `this` (its parent), which
 * pops every sibling subtree already fully serialized. A value is circular iff
 * it equals one of the holders still on that (ancestor-only) stack.
 */
function circularSafeReplacer(): (this: unknown, key: string, value: unknown) => unknown {
  // Each frame is [holder passed as `this`, value emitted for that holder].
  const stack: [unknown, unknown][] = [];
  return function (this: unknown, _key: string, value: unknown): unknown {
    if (stack.length > 0) {
      // Unwind sibling subtrees: pop until the top frame's emitted value is the
      // current holder (`this`), leaving only this value's ancestors on `stack`.
      while (stack.length > 0 && stack[stack.length - 1][1] !== this) {
        stack.pop();
      }
    }
    if (value !== null && typeof value === 'object') {
      for (const [holder] of stack) {
        if (holder === value) {
          return CIRCULAR;
        }
      }
      stack.push([value, value]);
    }
    return value;
  };
}
