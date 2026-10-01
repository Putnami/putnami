import { setTimeout as delay } from 'node:timers/promises';

/**
 * The longest time, in milliseconds, a call keeps retrying a transient failure.
 * `go.putnami.dev/sdk/extension/robustio` and cmd/go's robustio wait the same
 * two seconds.
 */
export const RETRY_BUDGET_MS = 2000;

/**
 * The codes a Windows rename or removal fails with while another process holds
 * the file without sharing delete access: EPERM (ERROR_ACCESS_DENIED), EBUSY
 * (ERROR_SHARING_VIOLATION) and EACCES.
 */
const TRANSIENT_CODES: ReadonlySet<string> = new Set(['EACCES', 'EBUSY', 'EPERM']);

/** The policy a retry follows. Each field defaults to the host's policy. */
export interface RetryOptions {
  /** The platform whose file-sharing failures are retried. Defaults to `process.platform`. */
  readonly platform?: NodeJS.Platform;
  /** How long to keep retrying, in milliseconds. Defaults to {@link RETRY_BUDGET_MS}. */
  readonly budgetMs?: number;
}

/**
 * Reports whether `error` is a failure that another process holding the file
 * causes on `platform`. Only Windows has such failures.
 */
export function isTransient(error: unknown, platform: NodeJS.Platform = process.platform): boolean {
  if (platform !== 'win32' || typeof error !== 'object' || error === null) {
    return false;
  }
  const code = (error as { code?: unknown }).code;
  return typeof code === 'string' && TRANSIENT_CODES.has(code);
}

/**
 * Returns a function that, given the failure of an attempt, returns the wait
 * in milliseconds before the next attempt, or throws that failure when it is
 * not transient or the budget would run out before the next attempt. The wait
 * starts at 1 ms and grows by a random fraction of itself after each attempt.
 */
function backoff(options: RetryOptions = {}): (error: unknown) => number {
  const platform = options.platform ?? process.platform;
  const budgetMs = options.budgetMs ?? RETRY_BUDGET_MS;
  const start = performance.now();
  let sleepMs = 1;
  return (error) => {
    if (!isTransient(error, platform) || performance.now() - start + sleepMs >= budgetMs) {
      throw error;
    }
    const waitMs = sleepMs;
    sleepMs += Math.random() * sleepMs;
    return waitMs;
  };
}

/**
 * Calls `op` until it returns, retrying a transient failure within the budget.
 * It throws the last failure unchanged. Off Windows it calls `op` once.
 */
export function retrySync<T>(op: () => T, options?: RetryOptions): T {
  const next = backoff(options);
  for (;;) {
    try {
      return op();
    } catch (error) {
      Bun.sleepSync(next(error));
    }
  }
}

/** The asynchronous form of {@link retrySync}. */
export function retry<T>(op: () => Promise<T>, options?: RetryOptions): Promise<T> {
  const next = backoff(options);
  const attempt = async (): Promise<T> => {
    try {
      return await op();
    } catch (error) {
      await delay(next(error));
      return attempt();
    }
  };
  return attempt();
}
