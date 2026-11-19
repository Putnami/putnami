import { Config, useConfig } from '../config';
import { Default, Env } from '../schema';
import { LEVEL_PRIORITY, type LogLevel } from './logger.type';

const VALID_LOG_LEVELS: readonly LogLevel[] = ['debug', 'info', 'warn', 'error'];
const DEFAULT_LEVEL: LogLevel = 'info';
const warnedInvalidLevels = new Set<string>();

/**
 * Normalize a raw level string to a valid LogLevel, like Go's ParseLevel: case
 * is ignored and `warning` means `warn`. Unknown values fall back to 'info'
 * with a one-time console warning so that a typo or bad env var never silently
 * drops error logs.
 */
export function normalizeLogLevel(raw: string | undefined): LogLevel {
  const lower = raw?.toLowerCase();
  const level = lower === 'warning' ? 'warn' : lower;
  if (level && (VALID_LOG_LEVELS as readonly string[]).includes(level)) {
    return level as LogLevel;
  }
  if (raw && !warnedInvalidLevels.has(raw)) {
    warnedInvalidLevels.add(raw);
    // Use process.stderr directly to avoid going through the logger (circular).
    process.stderr.write(
      `[putnami/runtime] Invalid LOG_LEVEL value "${raw}". ` +
        `Valid values are: ${VALID_LOG_LEVELS.join(', ')}. Falling back to "${DEFAULT_LEVEL}".\n`,
    );
  }
  return DEFAULT_LEVEL;
}

export const LoggerConfig = Config('logger', {
  json: Default(Boolean, true),
  level: Env('LOG_LEVEL', Default(String, DEFAULT_LEVEL)),
  buffer: Default(Boolean, false),
  bufferMaxSize: Default(Number, 100),
  bufferFlushInterval: Default(Number, 5000),
  refreshSeconds: Default(Number, 30),
});

type LoggerConfigType = {
  json: boolean;
  level: LogLevel;
  buffer: boolean;
  bufferMaxSize: number;
  bufferFlushInterval: number;
  refreshSeconds: number;
};

const defaultConfig: LoggerConfigType = {
  json: true,
  level: normalizeLogLevel(process.env['LOG_LEVEL']),
  buffer: false,
  bufferMaxSize: 100,
  bufferFlushInterval: 5000,
  refreshSeconds: 30,
};

let _loggerConfig: LoggerConfigType | undefined;

export const getLoggerConfig = (): LoggerConfigType => {
  if (!_loggerConfig) {
    try {
      const config = useConfig(LoggerConfig);
      const level = normalizeLogLevel(config.level);
      // Auto-detect Cloud Run: force JSON (structured logging) when K_SERVICE is present,
      // matching the CLI auto-detection in cli-options.ts.
      if (process.env['K_SERVICE'] && !config.json) {
        _loggerConfig = { ...config, level, json: true };
      } else {
        _loggerConfig = { ...config, level };
      }
    } catch (_e) {
      // Config not ready yet (bootstrap phase) — return defaults without caching
      // so we retry on next call once config is available.
      return defaultConfig;
    }
  }
  return _loggerConfig;
};

// liveLevel is the level setLogLevel set. Until a call, the level is the one
// logger.level and LOG_LEVEL give.
let liveLevel: LogLevel | undefined;

/**
 * Changes the minimum level of every logger that checks its level, process-wide.
 * The next record follows it.
 *
 * setLogLevel is the runtime control of the level: a started application calls
 * it when `logger.level` changes. The last call wins. An export directive
 * tightens the level through {@link setLogLevelLimit} instead, and leaves it
 * alone.
 */
export const setLogLevel = (level: LogLevel): void => {
  liveLevel = normalizeLogLevel(level);
};

/**
 * Returns the live minimum level: the last {@link setLogLevel} value, otherwise
 * `logger.level`, then `LOG_LEVEL`, then `info`.
 */
export const currentLogLevel = (): LogLevel => liveLevel ?? getLoggerConfig().level;

// levelLimit is the function setLogLevelLimit installed.
let levelLimit: (() => LogLevel | undefined) | undefined;

/**
 * Installs `limit`, a process-wide floor that every logger that checks the
 * level consults on each record, without changing the live level: a record
 * passes when it passes both the live level and the level `limit` returns.
 * `limit` returning `undefined` sets no floor; passing `undefined` removes it.
 *
 * An export directive uses it to tighten the level for a while: `limit`
 * returns the directive's level while the directive holds. A record at
 * `error` passes whatever the limit, and a record the limit filters counts
 * as dropped.
 */
export const setLogLevelLimit = (limit: (() => LogLevel | undefined) | undefined): void => {
  levelLimit = limit;
};

/** Reset the cached logger config, the live level and the level limit (for tests). */
export const resetLoggerConfig = (): void => {
  _loggerConfig = undefined;
  liveLevel = undefined;
  levelLimit = undefined;
  warnedInvalidLevels.clear();
};

/**
 * Reports whether a record at `messageLevel` passes the live minimum level and
 * the level limit. A record at `error` always passes.
 */
export const shouldLog = (messageLevel: LogLevel): boolean => {
  if (messageLevel === 'error') {
    return true;
  }
  const priority = LEVEL_PRIORITY[messageLevel];
  if (priority < LEVEL_PRIORITY[currentLogLevel()]) {
    return false;
  }
  const limit = levelLimit?.();
  return limit === undefined || priority >= LEVEL_PRIORITY[limit];
};
