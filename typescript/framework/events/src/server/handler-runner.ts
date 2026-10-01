import type { DetachedScope } from '@putnami/runtime';
import type { Message } from '../topic/message';
import { dispatchToHandler } from '../transport/dispatch';
import type { MessageAckState } from '../transport/message-utils';
import type { Envelope } from '../transport/transport';
import type { Subscription } from './delivery-queue';

// ---------------------------------------------------------------------------
// Handler invocation — context, DI scope, telemetry, logging, failure routing
// ---------------------------------------------------------------------------

export interface HandlerRunnerDeps {
  /** Optional factory for creating a DI scope per handler invocation. */
  readonly scopeFactory?: () => Promise<DetachedScope>;
  /** Invoked when the handler throws, so the caller can retry or dead-letter. */
  readonly onFailure: (error: unknown, envelope: Envelope, sub: Subscription) => void;
}

/**
 * Run a single handler invocation through the canonical transport dispatch
 * pipeline (see {@link dispatchToHandler}), routing failures to
 * {@link HandlerRunnerDeps.onFailure} for retry/DLQ handling.
 */
export async function runHandler(
  message: Message<unknown>,
  ackState: MessageAckState,
  abortController: AbortController,
  envelope: Envelope,
  sub: Subscription,
  deps: HandlerRunnerDeps,
): Promise<void> {
  await dispatchToHandler(message, ackState, abortController, envelope, sub.definition, sub.callback, {
    scopeFactory: deps.scopeFactory,
    onFailure: (error) => deps.onFailure(error, envelope, sub),
  });
}
