/**
 * Simple Logger for SDK and Extensions
 *
 * A lightweight console-based logger without async context awareness.
 * Used by core packages (SDK, CLI) and extensions that should not
 * depend on the full @putnami/runtime logger.
 *
 * For context-aware logging in applications, use @putnami/runtime's useLogger().
 */

// biome-ignore-all lint/suspicious/noConsole: Logger legitimately uses console methods

export type LogLevel = 'debug' | 'info' | 'warn' | 'error';

/**
 * Simple logger interface matching the standard console methods.
 */
export interface SimpleLogger {
  debug(message?: unknown, ...args: unknown[]): void;
  info(message?: unknown, ...args: unknown[]): void;
  warn(message?: unknown, ...args: unknown[]): void;
  error(message?: unknown, ...args: unknown[]): void;
}

/**
 * Creates a new logger instance with an optional name prefix.
 *
 * @param name - Optional name to prefix log messages
 * @returns A SimpleLogger instance
 *
 * @example
 * ```typescript
 * const logger = createLogger('my-service');
 * logger.info('Starting up'); // [my-service] Starting up
 * ```
 */
export function createLogger(name?: string): SimpleLogger {
  const prefix = name ? `[${name}]` : '';

  const logWithPrefix = (method: 'debug' | 'info' | 'warn' | 'error', msg: unknown, args: unknown[]): void => {
    if (prefix) {
      console[method](prefix, msg, ...args);
    } else {
      console[method](msg, ...args);
    }
  };

  return {
    debug: (msg, ...args) => logWithPrefix('debug', msg, args),
    info: (msg, ...args) => logWithPrefix('info', msg, args),
    warn: (msg, ...args) => logWithPrefix('warn', msg, args),
    error: (msg, ...args) => logWithPrefix('error', msg, args),
  };
}

// Cache for named loggers
const loggerCache = new Map<string, SimpleLogger>();

// Default unnamed logger
let defaultLogger: SimpleLogger | undefined;

/**
 * Gets a logger instance. Named loggers are cached for reuse.
 *
 * @param name - Optional name for the logger. If provided, returns a cached
 *               logger for that name. If not provided, returns a default logger.
 * @returns A SimpleLogger instance
 *
 * @example
 * ```typescript
 * // Get default logger
 * const logger = getLogger();
 * logger.info('Hello');
 *
 * // Get named logger (cached)
 * const dbLogger = getLogger('database');
 * dbLogger.info('Connected');
 * ```
 */
export function getLogger(name?: string): SimpleLogger {
  if (name) {
    let logger = loggerCache.get(name);
    if (!logger) {
      logger = createLogger(name);
      loggerCache.set(name, logger);
    }
    return logger;
  }

  if (!defaultLogger) {
    defaultLogger = createLogger();
  }
  return defaultLogger;
}

/**
 * Clears the logger cache. Useful for testing.
 */
export function clearLoggerCache(): void {
  loggerCache.clear();
  defaultLogger = undefined;
}
