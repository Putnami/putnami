/**
 * JSONL Event Schema for command hooks (v1)
 *
 * Each line of output from a command hook is a JSON object following this schema.
 * Events are used for logging, progress tracking, and result reporting.
 */

import { OneOf, Optional, schema, type ValidationError, validateSchema } from '../shared/schema';

/**
 * Event severity levels
 */
export type HookEventLevel = 'trace' | 'debug' | 'info' | 'warn' | 'error';

/**
 * Event types
 */
export type HookEventType = 'meta' | 'log' | 'progress' | 'artifact' | 'metric' | 'summary' | 'error';

/**
 * Base event structure - all events must have these fields
 */
export interface HookEventBase {
  /** Schema version for future compatibility */
  v: 1;
  /** ISO 8601 timestamp */
  time: string;
  /** Severity level */
  level: HookEventLevel;
  /** Event type */
  type: HookEventType;
  /** Human-readable message */
  message: string;
}

/**
 * Meta event - first event, provides context about the hook execution
 */
export interface HookEventMeta extends HookEventBase {
  type: 'meta';
  data: {
    extension: string;
    hook: string;
    version?: string;
  };
}

/**
 * Log event - general logging
 */
export interface HookEventLog extends HookEventBase {
  type: 'log';
  /** Optional step identifier for grouping logs */
  step?: string;
  /** Additional structured data */
  data?: Record<string, unknown>;
}

/**
 * Progress event - task progress indication
 */
export interface HookEventProgress extends HookEventBase {
  type: 'progress';
  /** Optional step identifier */
  step?: string;
  /** Progress percentage (0-100) */
  percent?: number;
  /** Current item being processed */
  current?: string;
  /** Total items to process */
  total?: number;
}

/**
 * Artifact event - file written or generated
 */
export interface HookEventArtifact extends HookEventBase {
  type: 'artifact';
  /** Action taken on the file */
  action: 'write' | 'delete' | 'copy';
  /** File path (relative to project root) */
  path: string;
  /** Additional metadata */
  data?: {
    size?: number;
    hash?: string;
  };
}

/**
 * Metric event - performance or measurement data
 */
export interface HookEventMetric extends HookEventBase {
  type: 'metric';
  data: {
    name: string;
    value: number;
    unit?: string;
  };
}

/**
 * Summary event - final result of the hook execution
 */
export interface HookEventSummary extends HookEventBase {
  type: 'summary';
  data: {
    /** Whether cache was used */
    cacheHit?: boolean;
    /** Number of outputs generated */
    outputs?: number;
    /** Duration in milliseconds */
    durationMs?: number;
    /** Export paths for build system */
    exports?: Record<string, string>;
    /** Asset paths for build system */
    assets?: Record<string, string>;
  };
}

/**
 * Error event - error details
 */
export interface HookEventError extends HookEventBase {
  type: 'error';
  level: 'error';
  /** Whether the error is recoverable */
  recoverable?: boolean;
  data?: {
    code?: string;
    stack?: string;
  };
}

/**
 * Union of all event types
 */
export type HookEvent =
  | HookEventMeta
  | HookEventLog
  | HookEventProgress
  | HookEventArtifact
  | HookEventMetric
  | HookEventSummary
  | HookEventError;

/**
 * Exit codes for command hooks
 */
export const HookExitCodes = {
  SUCCESS: 0,
  BUILD_ERROR: 1,
  INVALID_CONFIG: 2,
  INTERRUPTED: 130,
} as const;

export type HookExitCode = (typeof HookExitCodes)[keyof typeof HookExitCodes];

/**
 * Context passed to command hooks via --putnami-context file
 */
export interface HookContext {
  /** Workspace root directory */
  workspaceRoot: string;
  /** Project root directory */
  projectRoot: string;
  /** Extension root directory */
  extensionRoot: string;
  /** Output directory for generated files */
  outputRoot: string;
  /** Cache directory */
  cacheRoot: string;
  /** Enable debug output */
  debug: boolean;
  /** Hook name being executed */
  hook: string;
  /** Extension name */
  extension: string;
  /**
   * Putnami project identity: the `putnami.json` name (falling back to the
   * project path / go.mod / dir basename), NOT the npm `package.json` name.
   * Populated by the Go runner from `ctx.Project.Name`; absent when an older
   * runner writes the context file, so consumers must fall back gracefully.
   */
  projectName?: string;
  /** Extension-specific configuration */
  config?: Record<string, unknown>;
  /** Generation mode — indicates the command that triggered generation */
  mode?: 'build' | 'test' | 'serve' | 'config-extract';
}

/**
 * Helper to create a timestamped event
 */
export function createEvent<T extends HookEvent>(event: Omit<T, 'v' | 'time'>): T {
  return {
    v: 1,
    time: new Date().toISOString(),
    ...event,
  } as T;
}

/**
 * Helper to emit an event to stdout as JSONL
 */
export function emitEvent(event: HookEvent): void {
  process.stdout.write(`${JSON.stringify(event)}\n`);
}

/**
 * Emit an event and resolve once its bytes have been handed to the OS.
 *
 * When stdout is a pipe, `process.stdout.write` queues asynchronously and
 * `process.exit()` discards anything still queued. Pipe writes are delivered
 * in order, so awaiting the callback of a real (non-empty) chunk guarantees
 * that this event and every event emitted before it have left the process.
 * An empty-chunk write callback gives no such guarantee (Bun resolves it
 * immediately), so a summary larger than the 64KB pipe buffer would be
 * truncated — exit-critical events must go through this helper.
 */
export function emitEventFlushed(event: HookEvent): Promise<void> {
  return new Promise((resolve, reject) => {
    process.stdout.write(`${JSON.stringify(event)}\n`, (err) => (err ? reject(err) : resolve()));
  });
}

/**
 * Helper to emit a log event
 */
export function emitLog(level: HookEventLevel, message: string, data?: Record<string, unknown>): void {
  emitEvent(
    createEvent<HookEventLog>({
      type: 'log',
      level,
      message,
      data,
    }),
  );
}

/**
 * Helper to emit a progress event
 */
export function emitProgress(message: string, percent?: number, step?: string): void {
  emitEvent(
    createEvent<HookEventProgress>({
      type: 'progress',
      level: 'info',
      message,
      percent,
      step,
    }),
  );
}

/**
 * Helper to emit an artifact event
 */
export function emitArtifact(action: 'write' | 'delete' | 'copy', path: string, message?: string): void {
  emitEvent(
    createEvent<HookEventArtifact>({
      type: 'artifact',
      level: 'info',
      action,
      path,
      message: message || `${action} ${path}`,
    }),
  );
}

/**
 * Helper to emit a summary event.
 *
 * Summaries are terminal events that can exceed the pipe buffer (they carry
 * the full exports/assets maps), so the returned promise must be awaited
 * before exiting the process.
 */
export function emitSummary(
  message: string,
  data: HookEventSummary['data'],
  level: HookEventLevel = 'info',
): Promise<void> {
  return emitEventFlushed(
    createEvent<HookEventSummary>({
      type: 'summary',
      level,
      message,
      data,
    }),
  );
}

/**
 * Helper to emit an error event.
 *
 * Error events typically precede `process.exit`, so the returned promise must
 * be awaited to guarantee delivery.
 */
export function emitError(message: string, error?: Error, recoverable = false): Promise<void> {
  return emitEventFlushed(
    createEvent<HookEventError>({
      type: 'error',
      level: 'error',
      message,
      recoverable,
      data: error
        ? {
            code: error.name,
            stack: error.stack,
          }
        : undefined,
    }),
  );
}

const HOOK_EVENT_TYPES: readonly HookEventType[] = [
  'meta',
  'log',
  'progress',
  'artifact',
  'metric',
  'summary',
  'error',
];

const HOOK_EVENT_LEVELS: readonly HookEventLevel[] = ['trace', 'debug', 'info', 'warn', 'error'];

/**
 * Validate that an arbitrary parsed value is a well-formed `HookEvent`.
 *
 * JSONL lines come from untrusted subprocess output, so the base envelope is
 * validated explicitly before the value is treated as a typed event: it must be
 * a plain object with `v === 1`, a known `type`, a known `level`, and a string
 * `message`. Hostile or malformed lines are rejected rather than cast through.
 */
function isHookEvent(value: unknown): value is HookEvent {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    return false;
  }
  const event = value as Record<string, unknown>;
  return (
    event['v'] === 1 &&
    typeof event['type'] === 'string' &&
    HOOK_EVENT_TYPES.includes(event['type'] as HookEventType) &&
    typeof event['level'] === 'string' &&
    HOOK_EVENT_LEVELS.includes(event['level'] as HookEventLevel) &&
    typeof event['message'] === 'string'
  );
}

/**
 * Parse a JSONL line into a HookEvent.
 *
 * Returns `null` for non-JSON lines and for any payload that does not satisfy
 * the v1 event envelope — a malformed or hostile line never reaches callers as
 * a typed `HookEvent`.
 */
export function parseEvent(line: string): HookEvent | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(line);
  } catch {
    return null;
  }
  return isHookEvent(parsed) ? parsed : null;
}

/**
 * Schema describing the v1 `HookContext` envelope. Used to validate context
 * files (untrusted JSON) at the trust boundary before their values flow into
 * `process.env` and build control flow.
 */
export const HookContextSchema = schema({
  workspaceRoot: String,
  projectRoot: String,
  extensionRoot: String,
  outputRoot: String,
  cacheRoot: String,
  debug: Boolean,
  hook: String,
  extension: String,
  // Optional so a context file written by an older Go runner (which never set
  // projectName) still validates — the wire format stays backward compatible.
  projectName: Optional(String),
  mode: Optional(OneOf('build', 'test', 'serve', 'config-extract')),
});

/**
 * Validate a parsed value against {@link HookContextSchema}.
 *
 * Returns the validation errors for the known scalar envelope fields. The
 * free-form `config` map is intentionally left as passthrough (its values are
 * `unknown` by contract and never flow into env vars / control flow), but a
 * present `config` must still be a plain object.
 */
export function validateHookContext(value: unknown): ValidationError[] {
  const { errors } = validateSchema(HookContextSchema, value, { label: 'context' });
  const record = (typeof value === 'object' && value !== null ? value : {}) as Record<string, unknown>;
  const config = record['config'];
  if (config !== undefined && (typeof config !== 'object' || config === null || Array.isArray(config))) {
    errors.push({ field: 'context.config', message: 'context.config must be an object' });
  }
  return errors;
}
