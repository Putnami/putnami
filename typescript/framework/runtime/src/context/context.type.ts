import type { Logger } from '../logger/logger';

export type Context = Record<string, unknown> & {
  logger?: Logger;
  traceId?: string;
  logContext?: Record<string, unknown>;
  signal?: AbortSignal;
};
