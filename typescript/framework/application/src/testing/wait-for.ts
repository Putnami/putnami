/**
 * Poll a function until it returns a defined value, or timeout.
 *
 * Useful for waiting on async side-effects in tests (e.g. event handlers,
 * background jobs) without resorting to fragile `Bun.sleep()` calls.
 *
 * @param fn - Async function to poll. Return a value to resolve, or `undefined` to keep polling.
 * @param timeoutMs - Maximum time to wait before returning `undefined`. Defaults to 4000ms.
 * @param intervalMs - Polling interval. Defaults to 100ms.
 *
 * @example
 * ```typescript
 * const user = await waitFor(async () => {
 *   return db.users.findOne({ email: 'test@example.com' });
 * });
 * expect(user).toBeDefined();
 * ```
 */
export async function waitFor<T>(
  fn: () => Promise<T | undefined>,
  timeoutMs = 4000,
  intervalMs = 100,
): Promise<T | undefined> {
  const startedAt = Date.now();
  while (Date.now() - startedAt < timeoutMs) {
    const value = await fn();
    if (value !== undefined) return value;
    await new Promise((resolve) => setTimeout(resolve, intervalMs));
  }
  return undefined;
}
