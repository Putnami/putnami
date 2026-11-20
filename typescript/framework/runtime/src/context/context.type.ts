import type { Logger } from '../logger/logger';

export type Context = Record<string, unknown> & {
  logger?: Logger;
  traceId?: string;
  logContext?: Record<string, unknown>;
  /** Aborts when the work of this context must stop: the caller left or its deadline passed. */
  signal?: AbortSignal;
  /** Epoch milliseconds past which the work of this context is no longer awaited. */
  deadlineAt?: number;
};
