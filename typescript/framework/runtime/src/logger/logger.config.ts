import { Config, useConfig } from '../config';
import { Default, Env } from '../schema';
import { LEVEL_PRIORITY, type LogLevel } from './logger.type';

const VALID_LOG_LEVELS: readonly LogLevel[] = ['debug', 'info', 'warn', 'error'];
const DEFAULT_LEVEL: LogLevel = 'info';
const warnedInvalidLevels = new Set<string>();

/**
 * Normalize a raw LOG_LEVEL string to a valid LogLevel.
 * Unknown values fall back to 'info' with a one-time console warning so that
 * a typo or bad env var never silently drops error logs.
 */
export function normalizeLogLevel(raw: string | undefined): LogLevel {
  if (raw && (VALID_LOG_LEVELS as readonly string[]).includes(raw)) {
    return raw as LogLevel;
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
});

type LoggerConfigType = {
  json: boolean;
  level: LogLevel;
  buffer: boolean;
  bufferMaxSize: number;
  bufferFlushInterval: number;
};

const defaultConfig: LoggerConfigType = {
  json: true,
  level: normalizeLogLevel(process.env['LOG_LEVEL']),
  buffer: false,
  bufferMaxSize: 100,
  bufferFlushInterval: 5000,
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

/** Reset the cached logger config (for tests). */
export const resetLoggerConfig = (): void => {
  _loggerConfig = undefined;
  warnedInvalidLevels.clear();
};

export const shouldLog = (messageLevel: LogLevel): boolean => {
  const config = getLoggerConfig();
  return LEVEL_PRIORITY[messageLevel] >= LEVEL_PRIORITY[config.level];
};
