import { useLogger } from '@putnami/runtime';

export const DEFAULT_CONFIG_SERVER_TIMEOUT_MS = 5000;
export const DEFAULT_CONFIG_SERVER_RETRY_BUDGET_MS = 30_000;

const REMOTE_RETRY_INITIAL_DELAY_MS = 100;
const REMOTE_RETRY_MAX_DELAY_MS = 2000;
const sleepBuffer = new SharedArrayBuffer(4);
const sleepView = new Int32Array(sleepBuffer);

export interface RemoteAttemptFailure {
  message: string;
  cause?: unknown;
  context?: Record<string, unknown>;
  retryable: boolean;
}

export function retryableRemoteFailure(
  message: string,
  cause?: unknown,
  context?: Record<string, unknown>,
): RemoteAttemptFailure {
  return { message, cause, context, retryable: true };
}

export function terminalRemoteFailure(
  message: string,
  cause?: unknown,
  context?: Record<string, unknown>,
): RemoteAttemptFailure {
  return { message, cause, context, retryable: false };
}

export function remoteFailureDetail(failure: RemoteAttemptFailure): string {
  return failure.cause instanceof Error
    ? failure.cause.message
    : failure.cause
      ? String(failure.cause)
      : failure.message;
}

export function remoteRetryDelayMs(failedAttempts: number): number {
  if (failedAttempts <= 1) {
    return REMOTE_RETRY_INITIAL_DELAY_MS;
  }
  return Math.min(REMOTE_RETRY_INITIAL_DELAY_MS * 2 ** (failedAttempts - 1), REMOTE_RETRY_MAX_DELAY_MS);
}

export function sleepSync(ms: number): void {
  if (ms <= 0) {
    return;
  }
  Atomics.wait(sleepView, 0, 0, Math.ceil(ms));
}

export function configServerTimeoutFromEnv(): number {
  return configServerDurationFromEnv('CONFIG_SERVER_TIMEOUT', DEFAULT_CONFIG_SERVER_TIMEOUT_MS);
}

export function configServerRetryBudgetFromEnv(): number {
  if (process.env['CONFIG_SERVER_RETRY_BUDGET']?.trim() === '0') {
    return 0;
  }
  return configServerDurationFromEnv('CONFIG_SERVER_RETRY_BUDGET', DEFAULT_CONFIG_SERVER_RETRY_BUDGET_MS);
}

export function configServerDurationFromEnv(name: string, fallbackMs: number): number {
  const raw = process.env[name]?.trim();
  if (!raw) {
    return fallbackMs;
  }
  const parsed = parseConfigServerDurationMs(raw);
  if (parsed === undefined || parsed < 0) {
    useLogger('config-server').warn('invalid config-server duration env var, using default', {
      env: name,
      value: raw,
      defaultMs: fallbackMs,
    });
    return fallbackMs;
  }
  return parsed;
}

function parseConfigServerDurationMs(value: string): number | undefined {
  const match = value.match(/^(\d+(?:\.\d+)?)(ms|s|m|h)$/);
  if (!match) {
    return undefined;
  }
  const amount = Number.parseFloat(match[1]);
  if (!Number.isFinite(amount)) {
    return undefined;
  }
  const unit = match[2];
  if (unit === 'ms') return amount;
  if (unit === 's') return amount * 1000;
  if (unit === 'm') return amount * 60_000;
  if (unit === 'h') return amount * 3_600_000;
  return undefined;
}
