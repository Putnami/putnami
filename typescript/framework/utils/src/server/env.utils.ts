/**
 * Environment Utilities
 *
 * Simple environment detection utilities for determining the runtime environment.
 */

/**
 * Get the current runtime environment.
 *
 * Returns one of: 'test', 'production', or 'development' (default)
 *
 * Detection logic:
 * - 'test' if NODE_ENV is 'test'
 * - 'production' if NODE_ENV is 'production' or 'prod', or if running in GCP Cloud Run (K_SERVICE env var)
 * - 'development' otherwise
 *
 * @returns The current environment string
 *
 * @example
 * ```typescript
 * const env = getEnv();
 * if (env === 'production') {
 *   // Production-specific logic
 * }
 * ```
 */
/**
 * Restore an environment variable to a previously captured value.
 *
 * Restoring `undefined` deletes the variable instead of assigning it, so a
 * snapshot of an originally absent variable leaves it absent — assigning the
 * snapshot back unconditionally would store the string `"undefined"`.
 *
 * @param name The environment variable name
 * @param value The captured value, or `undefined` if the variable was absent
 *
 * @example
 * ```typescript
 * const previous = process.env.MY_FLAG;
 * process.env.MY_FLAG = '1';
 * try {
 *   // ...
 * } finally {
 *   restoreEnv('MY_FLAG', previous);
 * }
 * ```
 */
export function restoreEnv(name: string, value: string | undefined): void {
  if (value === undefined) {
    delete process.env[name];
    return;
  }
  process.env[name] = value;
}

export function getEnv(): string {
  if (process.env.NODE_ENV === 'test') {
    return 'test';
  }
  if (process.env.NODE_ENV === 'production' || process.env.NODE_ENV === 'prod') {
    return 'production';
  }
  // GCP Cloud Run detection
  if (process.env.K_SERVICE) {
    return 'production';
  }
  return 'development';
}
