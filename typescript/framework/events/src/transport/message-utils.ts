import { validateSchema } from '@putnami/runtime';
import type { ResolvedHandlerOptions } from '../handler/handler.type';
import type { TopicDefinition } from '../topic';
import type { Message } from '../topic/message';
import type { Envelope } from './transport';

export interface MessageAckState {
  acked: boolean;
  nacked: boolean;
  reason?: string;
}

export interface TransportMessage {
  message: Message<unknown>;
  ackState: MessageAckState;
  abortController: AbortController;
}

export function createTransportMessage(envelope: Envelope, opts: ResolvedHandlerOptions): TransportMessage {
  const ackState: MessageAckState = {
    acked: opts.ack === 'auto',
    nacked: false,
  };
  const abortController = new AbortController();

  const message: Message<unknown> = {
    id: envelope.id,
    topic: envelope.topic,
    channel: envelope.channel,
    payload: envelope.payload,
    key: envelope.key,
    dedupeKey: envelope.dedupeKey,
    topicVersion: envelope.topicVersion,
    timestamp: new Date(envelope.timestamp),
    attributes: envelope.attributes,
    attempt: envelope.attempt,
    traceId: envelope.traceId,
    signal: abortController.signal,
    ack() {
      if (opts.ack === 'manual') {
        ackState.acked = true;
      }
    },
    nack(reason?: string) {
      if (opts.ack === 'manual') {
        ackState.nacked = true;
        ackState.reason = reason;
        throw new Error(reason ?? 'Message was negatively acknowledged');
      }
    },
  };

  return { message, ackState, abortController };
}

/**
 * Validate a message payload against its topic schema before the handler runs.
 * Shared by every transport so the documented schema guarantee holds for
 * distributed delivery (pubsub / redis / local-server), not only the in-memory
 * broker. Throws so the caller routes the message to its failure path rather
 * than acking an invalid, producer/attacker-controlled payload.
 */
export function assertValidPayload(message: Message<unknown>, topic: TopicDefinition): void {
  const { errors } = validateSchema(topic.schema, message.payload, { label: topic.name });
  if (errors.length > 0) {
    const messages = errors.map((error) => `${error.field}: ${error.message}`).join('; ');
    throw new Error(`Invalid payload for topic '${topic.name}': ${messages}`);
  }
}

export function assertTransportMessageAcknowledged(ackState: MessageAckState, opts: ResolvedHandlerOptions): void {
  if (opts.ack !== 'manual') {
    return;
  }

  if (ackState.nacked) {
    throw new Error(ackState.reason ?? 'Message was negatively acknowledged');
  }

  if (!ackState.acked) {
    throw new Error('Message was not acknowledged');
  }
}

/**
 * Wait for `work` to settle, but no longer than `timeout` ms. Returns once
 * either the work completes or the deadline elapses — the work itself is never
 * cancelled, only un-awaited. A non-positive timeout means "wait unbounded".
 *
 * Shared by transports whose `stop()` drains in-flight delivery loops so a
 * single stuck handler cannot hang shutdown indefinitely (honoring a
 * configurable drain deadline consistently across backends).
 */
export async function withDrainTimeout(work: Promise<unknown>, timeout: number): Promise<void> {
  if (timeout <= 0) {
    await work;
    return;
  }

  let timerId: ReturnType<typeof setTimeout> | undefined;
  const deadline = new Promise<void>((resolve) => {
    timerId = setTimeout(resolve, timeout);
  });

  try {
    await Promise.race([work.then(() => undefined), deadline]);
  } finally {
    if (timerId) {
      clearTimeout(timerId);
    }
  }
}

export async function invokeWithTimeout(
  callback: (message: Message<unknown>) => Promise<void>,
  message: Message<unknown>,
  timeout: number,
  abortController: AbortController,
): Promise<void> {
  const handlerPromise = callback(message);
  if (timeout <= 0) {
    await handlerPromise;
    return;
  }

  let timerId: ReturnType<typeof setTimeout> | undefined;
  const timeoutPromise = new Promise<never>((_, reject) => {
    timerId = setTimeout(() => {
      abortController.abort(new Error(`Handler timed out after ${timeout}ms`));
      reject(new Error(`Handler timed out after ${timeout}ms`));
    }, timeout);
  });

  try {
    await Promise.race([handlerPromise, timeoutPromise]);
  } finally {
    if (timerId) {
      clearTimeout(timerId);
    }
  }
}
