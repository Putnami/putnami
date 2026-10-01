import { type Context, tryContext } from '../context';
import type { ConsoleSink } from './console.sink';
import type { JsonSink } from './json.sink';
import { getLoggerConfig } from './logger.config';
import {
  LEVEL_PRIORITY,
  type LogEntry,
  type LogLevel,
  type LogSink,
  type SerializedError,
  type WithOptions,
} from './logger.type';
import { redact } from './redact';

/**
 * Structured logger that fans log entries out to its configured sinks.
 *
 * Each entry carries the logger name, level, message, any `Error` (with its
 * `cause` chain) and structured data extracted from the call's params, plus the
 * request trace id and context when emitted inside a `runInContext` scope.
 * Levels below the configured threshold are dropped. Use {@link Logger.named}
 * to derive a named child and {@link Logger.with} to attach structured fields.
 */
export class Logger {
  private sinks: LogSink[];
  private loggerName?: string;
  private detachedContext: Record<string, unknown>;
  private skipLevelCheck: boolean;

  constructor(sinks: LogSink[], name?: string, detachedContext: Record<string, unknown> = {}, skipLevelCheck = false) {
    this.sinks = sinks;
    this.loggerName = name;
    this.detachedContext = detachedContext;
    this.skipLevelCheck = skipLevelCheck;
  }

  /** The logger's name, or an empty string when unnamed. */
  get name(): string {
    return this.loggerName || '';
  }

  /** Derive a logger that tags its entries with `name`, sharing this logger's sinks and detached context. */
  named(name: string): Logger {
    return new Logger(this.sinks, name, { ...this.detachedContext }, this.skipLevelCheck);
  }

  /**
   * Attaches a structured field to subsequent log entries.
   *
   * Inside a request scope (`runInContext`), the field is written to the
   * request-scoped `logContext` and `this` is returned, so the field is shared
   * by every logger resolved within that request and discarded when it ends.
   *
   * Outside a request scope there is no per-request store. Mutating
   * `this.detachedContext` would be a global side effect — and the default
   * logger is a process-wide singleton, so it would leak the field into every
   * later log line across the whole process. Instead, the detached path is
   * immutable: it returns a *new* `Logger` carrying a copy of the context plus
   * the new field, leaving `this` untouched. Chain or reassign to accumulate:
   *
   * ```typescript
   * const scoped = logger.with('userId', 'u-1').with('tenant', 'acme');
   * scoped.info('request handled');
   * ```
   */
  with(key: string, value: unknown, options: WithOptions = {}): Logger {
    return this.applyOp(key, value, 'set', options);
  }

  /**
   * Appends `value` to the accumulating list at the dot-separated `path`,
   * addressing nested groups (`'event.publishes'`). Dual-path like {@link with}:
   * inside a request scope it mutates the shared `logContext` and returns `this`;
   * detached it returns a new Logger with copy-on-write along the touched path.
   *
   * An absent leaf becomes `[value]`; an existing array is pushed to; any other
   * value is promoted to `[existing, value]` so earlier values are never lost.
   * Once the list reaches {@link MAX_APPENDED_FIELD_VALUES} the value is dropped
   * silently — pair every `append` with an `increment` of a sibling counter so
   * the true total survives the cap.
   */
  append(path: string, value: unknown): Logger {
    return this.applyOp(path, value, 'append', {});
  }

  /**
   * Adds `delta` to the counter at the dot-separated `path`, addressing nested
   * groups. Dual-path like {@link with}: in-scope it mutates the shared
   * `logContext` and returns `this`; detached it returns a new Logger. A
   * numeric leaf is incremented; anything else (or an absent leaf) is set to
   * `delta`.
   */
  increment(path: string, delta = 1): Logger {
    return this.applyOp(path, delta, 'increment', {});
  }

  private applyOp(path: string, value: unknown, op: FieldOp, options: WithOptions): Logger {
    const context = tryContext<Context>();

    if (context) {
      // Request scope: mutate the request-scoped logContext (shared within the
      // request, torn down when it ends) and return this for fluent chaining.
      if (!context.logContext) {
        context.logContext = {};
      }
      applyField(context.logContext, path, value, op, options, false);
      return this;
    }

    // Detached scope: never mutate the (possibly shared) detachedContext.
    // Return a new Logger with a copy-on-write context along the touched path.
    const nextContext = { ...this.detachedContext };
    applyField(nextContext, path, value, op, options, true);
    return new Logger(this.sinks, this.loggerName, nextContext, this.skipLevelCheck);
  }

  /** Log at `debug` level. Extra params are captured as structured data; an `Error` param is recorded with its cause chain. */
  debug(message?: unknown, ...params: unknown[]): void {
    this.emit('debug', message, params);
  }

  /** Log at `info` level. Extra params are captured as structured data; an `Error` param is recorded with its cause chain. */
  info(message?: unknown, ...params: unknown[]): void {
    this.emit('info', message, params);
  }

  /** Log at `warn` level. Extra params are captured as structured data; an `Error` param is recorded with its cause chain. */
  warn(message?: unknown, ...params: unknown[]): void {
    this.emit('warn', message, params);
  }

  /** Log at `error` level. The message or an `Error` param is recorded with its cause chain; other params become structured data. */
  error(message?: unknown, ...params: unknown[]): void {
    this.emit('error', message, params);
  }

  /** Flush every sink that buffers entries, resolving once all have drained. */
  async flush(): Promise<void> {
    for (const sink of this.sinks) {
      await sink.flush?.();
    }
  }

  /** Close every sink, releasing its resources. The logger should not be used afterwards. */
  async close(): Promise<void> {
    for (const sink of this.sinks) {
      await sink.close?.();
    }
  }

  private emit(level: LogLevel, message: unknown, params: unknown[]): void {
    if (!this.skipLevelCheck && !shouldLog(level)) {
      return;
    }

    const context = tryContext<Context>();
    const entry = buildEntry(level, message, params, context, this.loggerName);
    if (Object.keys(this.detachedContext).length > 0) {
      entry.context = {
        ...(redact(this.detachedContext) as Record<string, unknown>),
        ...(entry.context || {}),
      };
    }

    for (const sink of this.sinks) {
      sink.write(entry);
    }
  }
}

function shouldLog(level: LogLevel): boolean {
  const config = getLoggerConfig();
  // config.level is always a valid LogLevel (normalised by getLoggerConfig).
  return LEVEL_PRIORITY[level] >= LEVEL_PRIORITY[config.level];
}

/**
 * Cap on the number of values a single {@link Logger.append} target may hold.
 * Kept in sync with `MaxAppendedFieldValues` in `go/framework/logger` and the
 * `protocols/logging/conformance` corpus.
 */
export const MAX_APPENDED_FIELD_VALUES = 100;

/** Context-accumulation operations supported by {@link applyField}. */
type FieldOp = 'set' | 'append' | 'increment';

/**
 * Applies one context-accumulation operation to `target`.
 *
 * `set` (used by {@link Logger.with}) keeps the original literal-key,
 * shallow-merge semantics — `path` is a whole key, never split on dots — so
 * existing callers are unaffected. `append`/`increment` interpret `path` as a
 * dot-separated address into nested groups, creating plain-object intermediates
 * (and replacing any non-plain-object intermediate with a fresh `{}`).
 *
 * When `clone` is set (the detached copy-on-write path) each container touched
 * along the path is shallow-copied before mutation, so a possibly-shared source
 * structure is never mutated; when it is unset (the in-scope path) the shared
 * `logContext` is mutated in place so accumulation is visible request-wide.
 * Never throws.
 */
function applyField(
  target: Record<string, unknown>,
  path: string,
  value: unknown,
  op: FieldOp,
  options: WithOptions,
  clone: boolean,
): void {
  if (op === 'set') {
    setField(target, path, value, options);
    return;
  }

  const keys = path.split('.');
  const leaf = keys[keys.length - 1];
  let node = target;
  for (let i = 0; i < keys.length - 1; i++) {
    const key = keys[i];
    const existing = node[key];
    if (isPlainObject(existing)) {
      node[key] = clone ? { ...existing } : existing;
    } else {
      node[key] = {};
    }
    node = node[key] as Record<string, unknown>;
  }

  node[leaf] = op === 'append' ? appendValue(node[leaf], value, clone) : incrementValue(node[leaf], value as number);
}

/** `with()`'s field write: shallow-merge plain objects unless `replace` is set. */
function setField(target: Record<string, unknown>, key: string, value: unknown, options: WithOptions): void {
  if (options.replace || typeof value !== 'object' || value === null) {
    target[key] = value;
    return;
  }
  const existing = target[key];
  if (typeof existing === 'object' && existing !== null) {
    target[key] = { ...existing, ...value };
  } else {
    target[key] = value;
  }
}

/**
 * Append semantics: absent → `[value]`; existing array → push (capped at
 * {@link MAX_APPENDED_FIELD_VALUES}); any other value → `[existing, value]`.
 * In clone mode the existing array is copied rather than mutated in place.
 */
function appendValue(existing: unknown, value: unknown, clone: boolean): unknown {
  if (existing === undefined) {
    return [value];
  }
  if (Array.isArray(existing)) {
    if (existing.length >= MAX_APPENDED_FIELD_VALUES) {
      return existing; // cap reached: drop silently
    }
    if (clone) {
      return [...existing, value];
    }
    existing.push(value);
    return existing;
  }
  // Scalar promotion: never lose the earlier value.
  return [existing, value];
}

/** Increment semantics: numeric leaf → `+delta`; anything else/absent → `delta`. */
function incrementValue(existing: unknown, delta: number): number {
  return typeof existing === 'number' ? existing + delta : delta;
}

/** True for objects with the default (or null) prototype — not arrays/class instances. */
function isPlainObject(value: unknown): value is Record<string, unknown> {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    return false;
  }
  const proto = Object.getPrototypeOf(value);
  return proto === Object.prototype || proto === null;
}

/** Max depth to follow `Error.cause` chains, guarding against cyclic causes. */
const MAX_ERROR_CAUSE_DEPTH = 5;

/**
 * Keys serialized into their own {@link SerializedError} slots; excluded from
 * the redacted "extra fields" copy so they are not duplicated.
 */
const ERROR_RESERVED_KEYS = new Set<string>(['name', 'message', 'stack', 'cause']);

/**
 * Copy an error's custom own-enumerable properties onto the serialized entry,
 * redacted through the shared key denylist. Thrown errors commonly carry
 * sensitive fields (a driver error's `config.password`, an HTTP error's
 * `request.headers.authorization`); since `redact()` returns Error/class
 * instances untouched, those fields would otherwise reach a sink unmasked.
 */
function assignRedactedErrorFields(entry: SerializedError, source: Record<string, unknown>): void {
  const extra: Record<string, unknown> = {};
  let hasExtra = false;
  for (const key of Object.keys(source)) {
    if (ERROR_RESERVED_KEYS.has(key)) {
      continue;
    }
    extra[key] = source[key];
    hasExtra = true;
  }
  if (!hasExtra) {
    return;
  }
  // redact() masks sensitive keys (by name) and recurses into plain
  // objects/arrays, reusing the single shared denylist — no key list is
  // duplicated here. Spreading the result merges the redacted extras onto the
  // entry alongside name/message/stack/cause.
  Object.assign(entry, redact(extra) as Record<string, unknown>);
}

function toErrorEntry(val: unknown, depth = 0): SerializedError | undefined {
  if (val instanceof Error) {
    const entry: SerializedError = { name: val.name, message: val.message, stack: val.stack };
    if (val.cause !== undefined && depth < MAX_ERROR_CAUSE_DEPTH) {
      entry.cause = toErrorEntry(val.cause, depth + 1);
    }
    assignRedactedErrorFields(entry, val as unknown as Record<string, unknown>);
    return entry;
  }
  if (val !== null && typeof val === 'object') {
    const e = val as Record<string, unknown>;
    if (typeof e['message'] === 'string') {
      const entry: SerializedError = {
        name: typeof e['name'] === 'string' ? e['name'] : 'Error',
        message: e['message'],
        stack: typeof e['stack'] === 'string' ? e['stack'] : undefined,
      };
      if (e['cause'] !== undefined && depth < MAX_ERROR_CAUSE_DEPTH) {
        entry.cause = toErrorEntry(e['cause'], depth + 1);
      }
      assignRedactedErrorFields(entry, e);
      return entry;
    }
  }
  return undefined;
}

function buildEntry(
  level: LogLevel,
  message: unknown,
  params: unknown[],
  context: Context | undefined,
  loggerName?: string,
): LogEntry {
  const entry: LogEntry = {
    level,
    message: formatMessage(message),
    timestamp: new Date().toISOString(),
  };

  if (loggerName) {
    entry.logger = loggerName;
  }

  if (context?.traceId) {
    entry.traceId = context.traceId;
  }

  if (context?.logContext && Object.keys(context.logContext).length > 0) {
    // redact() returns a fresh object, so the live request logContext is left
    // untouched while sensitive values never reach a sink.
    entry.context = redact(context.logContext) as Record<string, unknown>;
  }

  // Extract errors and data from params
  const data: unknown[] = [];
  for (const param of params) {
    const err = toErrorEntry(param);
    if (err) {
      entry.error = err;
    } else {
      data.push(redact(param));
    }
  }

  // Also check if message itself is an Error
  const msgErr = toErrorEntry(message);
  if (msgErr) {
    entry.error = msgErr;
  }

  if (data.length > 0) {
    entry.data = data;
  }

  return entry;
}

function formatMessage(message: unknown): string {
  if (typeof message === 'string') {
    return message;
  }
  const errEntry = toErrorEntry(message);
  if (errEntry) {
    return `${errEntry.name}: ${errEntry.message}`;
  }
  if (message === undefined) {
    return '';
  }
  try {
    return JSON.stringify(redact(message));
  } catch {
    return String(message);
  }
}

// ─── Default logger setup ───

let _defaultLogger: Logger | undefined;

const getDefaultLogger = (): Logger => {
  if (!_defaultLogger) {
    const config = getLoggerConfig();
    // Dynamic requires to avoid circular imports at module load time.
    // These modules only depend on logger.type.ts, not this file.
    const { ConsoleSink: CSink } = require('./console.sink') as { ConsoleSink: typeof ConsoleSink };
    const { JsonSink: JSink } = require('./json.sink') as { JsonSink: typeof JsonSink };

    const outputSink: LogSink = config.json ? new JSink() : new CSink();

    let sinks: LogSink[];
    if (config.buffer) {
      const { BufferSink } = require('./buffer.sink');
      sinks = [
        new BufferSink({ sink: outputSink, maxSize: config.bufferMaxSize, flushInterval: config.bufferFlushInterval }),
      ];
    } else {
      sinks = [outputSink];
    }

    _defaultLogger = new Logger(sinks);
  }
  return _defaultLogger;
};

export const useLogger = (loggerName?: string): Logger => {
  const context = tryContext<Context>();
  const baseLogger = (context?.logger as Logger | undefined) || getDefaultLogger();
  return loggerName ? baseLogger.named(loggerName) : baseLogger;
};

/** Reset the cached default logger (for tests). */
export const resetDefaultLogger = (): void => {
  _defaultLogger = undefined;
};

/** Override the cached root logger (for tests and controlled embedders). */
export const setRootLogger = (logger: Logger): void => {
  _defaultLogger = logger;
};
