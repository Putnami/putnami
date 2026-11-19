import { getLocalSources, loadGlobalConfig } from '../config/config-source';
import { normalizeLogLevel, setLogLevel } from './logger.config';

/**
 * Reads `logger.level` from the sources that load without the network: the
 * `conf/` files and `CONFIG_DATA`. Returns an empty string when the key is
 * absent.
 */
export function readLocalLogLevel(): string {
  const tree = loadGlobalConfig(getLocalSources());
  const block = tree?.['logger'];
  if (!block || typeof block !== 'object') {
    return '';
  }
  const level = (block as Record<string, unknown>)['level'];
  return typeof level === 'string' ? level : '';
}

/**
 * Applies the level rule to the value of `logger.level`: a non-empty value
 * wins, then `LOG_LEVEL`, then `info`. An unknown value counts as `info`.
 */
export function resolveLogLevel(configured: string): ReturnType<typeof normalizeLogLevel> {
  return normalizeLogLevel(configured || process.env['LOG_LEVEL'] || undefined);
}

/**
 * Reads `logger.level` with `read` once, then every `intervalSeconds`, and
 * applies a value that differs from the previous read with `setLogLevel`
 * through {@link resolveLogLevel}. The first read only records the value, so
 * a level set since start stays until the configuration changes. A failed read
 * keeps the level and the previous value. With an interval of zero or less,
 * nothing is read.
 *
 * Returns a function that stops the watcher. The timer does not keep the
 * process alive.
 */
export function watchLogLevel(intervalSeconds: number, read: () => string = readLocalLogLevel): () => void {
  if (!(intervalSeconds > 0)) {
    return () => {};
  }
  let last: string | undefined;
  try {
    last = read();
  } catch {
    last = undefined;
  }
  const timer = setInterval(() => {
    let value: string;
    try {
      value = read();
    } catch {
      return;
    }
    if (last !== undefined && value === last) {
      return;
    }
    last = value;
    setLogLevel(resolveLogLevel(value));
  }, intervalSeconds * 1000);
  timer.unref?.();
  return () => clearInterval(timer);
}
