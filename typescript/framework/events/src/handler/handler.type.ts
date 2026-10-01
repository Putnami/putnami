import type { Message } from '../topic/message';

// ---------------------------------------------------------------------------
// Handler options
// ---------------------------------------------------------------------------

/** How messages are distributed across handler instances */
export type Distribution = 'competing' | 'broadcast';

/** Acknowledgement mode */
export type AckMode = 'auto' | 'manual';

/**
 * Configuration for an event handler.
 * Conservative defaults: up to 10 delivery attempts with exponential backoff, DLQ enabled.
 */
export interface HandlerOptions {
  /** Stable subscription/group name for external transports. */
  group?: string;
  /** How messages distribute across instances. Default: 'competing' */
  distribution?: Distribution;
  /**
   * Maximum delivery attempts before the message goes to the DLQ. This counts
   * total attempts, not extra retries: `1` = a single attempt with no retry,
   * `N` = up to N attempts (N-1 retries). Default: 10
   */
  maxRetries?: number;
  /** Maximum backoff delay in ms. Default: 60_000 (60s) */
  maxBackoff?: number;
  /** Handler execution timeout in ms. Default: 30_000 (30s) */
  timeout?: number;
  /** Maximum concurrent invocations for this handler. 0 means unlimited. Default: 0 */
  concurrency?: number;
  /** Maximum queued deliveries waiting for concurrency slots. 0 means unlimited. Default: 0 */
  queueLimit?: number;
  /** Behavior when queueLimit is reached. Default: 'throw' */
  overflow?: 'throw' | 'drop';
  /** Send failed messages to a dead-letter queue. When false, failed messages are logged and discarded. Default: true */
  dlq?: boolean;
  /** Acknowledgement mode. Default: 'auto' (return = ack, throw = nack) */
  ack?: AckMode;
}

/** Resolved handler options with all defaults applied */
export interface ResolvedHandlerOptions {
  readonly group?: string;
  readonly distribution: Distribution;
  readonly maxRetries: number;
  readonly backoff: 'exponential';
  readonly maxBackoff: number;
  readonly timeout: number;
  readonly concurrency: number;
  readonly queueLimit: number;
  readonly overflow: 'throw' | 'drop';
  readonly dlq: boolean;
  readonly ack: AckMode;
}

export const DEFAULT_HANDLER_OPTIONS: ResolvedHandlerOptions = {
  group: undefined,
  distribution: 'competing',
  maxRetries: 10,
  backoff: 'exponential',
  maxBackoff: 60_000,
  timeout: 30_000,
  concurrency: 0,
  queueLimit: 0,
  overflow: 'throw',
  dlq: true,
  ack: 'auto',
};

export function resolveHandlerOptions(opts?: HandlerOptions): ResolvedHandlerOptions {
  return {
    group: opts?.group,
    distribution: opts?.distribution ?? DEFAULT_HANDLER_OPTIONS.distribution,
    maxRetries: Math.max(1, opts?.maxRetries ?? DEFAULT_HANDLER_OPTIONS.maxRetries),
    backoff: 'exponential',
    maxBackoff: Math.max(0, opts?.maxBackoff ?? DEFAULT_HANDLER_OPTIONS.maxBackoff),
    timeout: Math.max(0, opts?.timeout ?? DEFAULT_HANDLER_OPTIONS.timeout),
    concurrency: Math.max(0, opts?.concurrency ?? DEFAULT_HANDLER_OPTIONS.concurrency),
    queueLimit: Math.max(0, opts?.queueLimit ?? DEFAULT_HANDLER_OPTIONS.queueLimit),
    overflow: opts?.overflow ?? DEFAULT_HANDLER_OPTIONS.overflow,
    dlq: opts?.dlq ?? DEFAULT_HANDLER_OPTIONS.dlq,
    ack: opts?.ack ?? DEFAULT_HANDLER_OPTIONS.ack,
  };
}

// ---------------------------------------------------------------------------
// Handler function signature
// ---------------------------------------------------------------------------

/** The function invoked when a message arrives */
export type HandlerFn<T> = (message: Message<T>) => void | Promise<void>;

/** The function invoked when a message arrives with DI-injected dependencies */
export type InjectedHandlerFn<TDeps, T> = (deps: TDeps, message: Message<T>) => void | Promise<void>;

// ---------------------------------------------------------------------------
// Attribute filter (server-side capable)
// ---------------------------------------------------------------------------

/** Server-side attribute filter: only deliver messages matching these attributes */
export interface AttributeFilter {
  attributes: Record<string, string>;
}
