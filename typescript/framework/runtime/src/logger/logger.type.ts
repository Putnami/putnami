export type LogLevel = 'debug' | 'info' | 'warn' | 'error';

export interface WithOptions {
  replace?: boolean;
}

/**
 * A serialized Error, including its `cause` chain (captured to a bounded depth)
 * and any custom own-enumerable fields the error carried (e.g. a driver error's
 * `config`), redacted through the same key denylist as the rest of the log.
 */
export interface SerializedError {
  name: string;
  message: string;
  stack?: string;
  cause?: SerializedError;
  /** Redacted custom own-enumerable properties carried by the error. */
  [key: string]: unknown;
}

export interface LogEntry {
  level: LogLevel;
  message: string;
  timestamp: string;
  logger?: string;
  traceId?: string;
  context?: Record<string, unknown>;
  error?: SerializedError;
  data?: unknown[];
}

export interface LogSink {
  write(entry: LogEntry): void;
  flush?(): Promise<void>;
  close?(): Promise<void>;
}

export const LEVEL_PRIORITY: Record<LogLevel, number> = {
  debug: 0,
  info: 1,
  warn: 2,
  error: 3,
};
