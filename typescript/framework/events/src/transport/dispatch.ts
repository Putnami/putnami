import { incCounter, observeHistogram } from '@putnami/application';
import { type DetachedScope, runInContext, useLogger } from '@putnami/runtime';
import { SCOPE_CONTAINER_KEY } from '@putnami/runtime/inject';
import type { EventContext } from '../context';
import type { HandlerDefinition } from '../handler/handler';
import type { Message } from '../topic/message';
import { asError, EVENT_HANDLER_LOGGER, OUTCOME_FAILURE, OUTCOME_SUCCESS } from './delivery-logging';
import {
  assertTransportMessageAcknowledged,
  assertValidPayload,
  invokeWithTimeout,
  type MessageAckState,
} from './message-utils';
import type { Envelope } from './transport';

// ---------------------------------------------------------------------------
// Canonical per-delivery dispatch — shared by every transport
// ---------------------------------------------------------------------------

export interface DispatchDeps {
  /** Optional factory for creating a DI scope per handler invocation. */
  readonly scopeFactory?: () => Promise<DetachedScope>;
  /**
   * When provided, a handler failure is routed here (inside the event
   * context, after logging/telemetry) instead of being rethrown — the
   * memory broker uses this for retry/DLQ routing. When absent, the
   * failure is rethrown so the transport can nack/return it.
   */
  readonly onFailure?: (error: unknown) => void;
}

/**
 * Runs one handler invocation through the canonical dispatch pipeline
 * every transport must share: event async context, optional DI scope
 * (exposed to the handler via the scope container key), payload
 * validation, timeout enforcement, manual-ack assertion, lifecycle
 * logging, and success/failure telemetry
 * (`events.handle.<topic>.success|failure|duration`).
 *
 * Transports own only what genuinely differs per backend: envelope
 * decoding before, and ack/nack/retry routing after.
 */
export async function dispatchToHandler(
  message: Message<unknown>,
  ackState: MessageAckState,
  abortController: AbortController,
  envelope: Envelope,
  definition: HandlerDefinition,
  callback: (message: Message<unknown>) => Promise<void>,
  deps: DispatchDeps = {},
): Promise<void> {
  const opts = definition.options;

  const ctx: EventContext = {
    traceId: envelope.traceId ?? envelope.id,
    eventTopic: envelope.topic,
    eventMessageId: envelope.id,
    eventAttempt: envelope.attempt,
  };

  let detachedScope: DetachedScope | undefined;

  try {
    await runInContext(ctx, async () => {
      const logger = useLogger(EVENT_HANDLER_LOGGER);
      const start = Date.now();

      logger.with('event', {
        topic: envelope.topic,
        messageId: envelope.id,
        attempt: envelope.attempt,
      });

      try {
        // Create a per-handler DI scope when a scope factory is available.
        // The scope is injected into the event context so that
        // useContainer() / resolve() / .inject() work inside handlers.
        //
        // Scope creation lives INSIDE this try, and INSIDE runInContext, so a
        // scope-factory rejection produces the SAME terminal failure record as a
        // handler failure (trace id, `event` group, structured error, duration
        // histogram) before being routed to onFailure.
        if (deps.scopeFactory) {
          detachedScope = await deps.scopeFactory();
          (ctx as Record<string | symbol, unknown>)[SCOPE_CONTAINER_KEY] = detachedScope.scope;
        }

        assertValidPayload(message, definition.topic);
        await invokeWithTimeout(callback, message, opts.timeout, abortController);
        assertTransportMessageAcknowledged(ackState, opts);

        const durationMs = Date.now() - start;
        logger.with('event', { outcome: OUTCOME_SUCCESS, durationMs });
        logger.info('message handled');

        incCounter(`events.handle.${envelope.topic}.success`);
        observeHistogram(`events.handle.${envelope.topic}.duration`, durationMs);
      } catch (error) {
        const durationMs = Date.now() - start;
        logger.with('event', { outcome: OUTCOME_FAILURE, durationMs });
        logger.error('message handling failed', asError(error));

        incCounter(`events.handle.${envelope.topic}.failure`);
        observeHistogram(`events.handle.${envelope.topic}.duration`, durationMs);

        if (deps.onFailure) {
          deps.onFailure(error);
          return;
        }
        throw error;
      }
    });
  } finally {
    await detachedScope?.close();
  }
}
