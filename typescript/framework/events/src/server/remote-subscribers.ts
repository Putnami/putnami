import { randomUUID } from 'node:crypto';
import type { ResolvedHandlerOptions } from '../handler/handler.type';
import type { Message } from '../topic/message';

// ---------------------------------------------------------------------------
// Remote subscriber registry — subscriber + long-poll/ack state
//
// Owns the per-subscriber delivery queues, in-flight (pending-ack) deliveries,
// and the long-poll waiter mechanics. The HTTP layer in `memory-server.ts`
// translates these outcomes into responses; this class holds no HTTP concerns.
// ---------------------------------------------------------------------------

type RemoteCallback = (message: Message<unknown>) => Promise<void>;

export interface RemoteDelivery {
  deliveryId: string;
  message: {
    id: string;
    topic: string;
    payload: unknown;
    key?: string;
    dedupeKey?: string;
    topicVersion?: string;
    timestamp: string;
    attributes: Record<string, string>;
    attempt: number;
    traceId?: string;
  };
}

interface RemoteDeliveryHandle {
  readonly timer?: ReturnType<typeof setTimeout>;
  readonly resolve: () => void;
  readonly reject: (error: Error) => void;
}

interface RemoteSubscriber {
  readonly id: string;
  readonly topic: string;
  readonly callback: RemoteCallback;
  readonly queue: RemoteDelivery[];
  readonly pending: Map<string, RemoteDeliveryHandle>;
  pullWaiter?: (delivery: RemoteDelivery | undefined) => void;
}

/** Outcome of a long-poll pull, mapped to an HTTP response by the caller. */
export type PullResult =
  | { readonly status: 'not-found' }
  | { readonly status: 'concurrent' }
  | { readonly status: 'empty' }
  | { readonly status: 'delivery'; readonly delivery: RemoteDelivery };

/** Outcome of an acknowledgement, mapped to an HTTP response by the caller. */
export type AckResult = 'ok' | 'subscriber-not-found' | 'delivery-not-found';

export class RemoteSubscriberRegistry {
  private readonly subscribers = new Map<string, RemoteSubscriber>();

  constructor(private readonly unsubscribe: (topic: string, callback: RemoteCallback) => void) {}

  /** Register a remote subscriber. Its broker subscription is wired by the caller. */
  register(id: string, topic: string, callback: RemoteCallback): void {
    this.subscribers.set(id, { id, topic, callback, queue: [], pending: new Map() });
  }

  /**
   * Assign a delivery to a subscriber and wait for it to be acked, nacked, or to
   * time out. Resolves on ack; rejects on nack or handler timeout.
   */
  async enqueue(id: string, options: ResolvedHandlerOptions, message: Message<unknown>): Promise<void> {
    const subscriber = this.subscribers.get(id);
    if (!subscriber) {
      throw new Error(`Remote subscriber '${id}' is not registered`);
    }

    const deliveryId = randomUUID();
    const delivery: RemoteDelivery = {
      deliveryId,
      message: {
        id: message.id,
        topic: message.topic,
        payload: message.payload,
        key: message.key,
        dedupeKey: message.dedupeKey,
        topicVersion: message.topicVersion,
        timestamp: message.timestamp.toISOString(),
        attributes: message.attributes,
        attempt: message.attempt,
        traceId: message.traceId,
      },
    };

    await new Promise<void>((resolve, reject) => {
      const timeoutMs = options.timeout;
      const timer =
        timeoutMs > 0
          ? setTimeout(() => {
              this.removeQueuedDelivery(subscriber, deliveryId);
              this.clearPendingDelivery(subscriber, deliveryId);
              reject(new Error(`Remote handler timed out after ${timeoutMs}ms`));
            }, timeoutMs)
          : undefined;

      subscriber.pending.set(deliveryId, { timer, resolve, reject });

      if (subscriber.pullWaiter) {
        const waiter = subscriber.pullWaiter;
        subscriber.pullWaiter = undefined;
        waiter(delivery);
      } else {
        subscriber.queue.push(delivery);
      }
    });
  }

  /** Long-poll for the next assigned delivery, bounded by `timeoutMs`. */
  async pull(id: string, timeoutMs: number): Promise<PullResult> {
    const subscriber = this.subscribers.get(id);
    if (!subscriber) {
      return { status: 'not-found' };
    }

    const queued = subscriber.queue.shift();
    if (queued) {
      return { status: 'delivery', delivery: queued };
    }

    if (subscriber.pullWaiter) {
      return { status: 'concurrent' };
    }

    const delivery = await new Promise<RemoteDelivery | undefined>((resolve) => {
      const timeoutId = setTimeout(() => {
        subscriber.pullWaiter = undefined;
        resolve(undefined);
      }, timeoutMs);

      subscriber.pullWaiter = (value) => {
        clearTimeout(timeoutId);
        subscriber.pullWaiter = undefined;
        resolve(value);
      };
    });

    return delivery ? { status: 'delivery', delivery } : { status: 'empty' };
  }

  /** Acknowledge or reject a pending delivery. */
  ack(id: string, deliveryId: string, status: 'ack' | 'nack', reason?: string): AckResult {
    const subscriber = this.subscribers.get(id);
    if (!subscriber) {
      return 'subscriber-not-found';
    }

    const pending = subscriber.pending.get(deliveryId);
    if (!pending) {
      return 'delivery-not-found';
    }

    this.clearPendingDelivery(subscriber, deliveryId);
    if (status === 'ack') {
      pending.resolve();
    } else {
      pending.reject(new Error(reason ?? 'Remote handler failed'));
    }

    return 'ok';
  }

  /** Remove a subscriber, rejecting any pending deliveries with `reason`. */
  remove(id: string, reason: string): void {
    const subscriber = this.subscribers.get(id);
    if (!subscriber) {
      return;
    }

    this.unsubscribe(subscriber.topic, subscriber.callback);

    if (subscriber.pullWaiter) {
      subscriber.pullWaiter(undefined);
    }

    for (const deliveryId of subscriber.pending.keys()) {
      const pending = subscriber.pending.get(deliveryId);
      if (!pending) {
        continue;
      }

      this.clearPendingDelivery(subscriber, deliveryId);
      pending.reject(new Error(reason));
    }

    subscriber.queue.length = 0;
    this.subscribers.delete(id);
  }

  /** Remove every subscriber (used during server shutdown). */
  clear(reason: string): void {
    for (const id of [...this.subscribers.keys()]) {
      this.remove(id, reason);
    }
    this.subscribers.clear();
  }

  private clearPendingDelivery(subscriber: RemoteSubscriber, deliveryId: string): void {
    const pending = subscriber.pending.get(deliveryId);
    if (!pending) {
      return;
    }

    if (pending.timer) {
      clearTimeout(pending.timer);
    }
    subscriber.pending.delete(deliveryId);
  }

  private removeQueuedDelivery(subscriber: RemoteSubscriber, deliveryId: string): void {
    const next = subscriber.queue.filter((delivery) => delivery.deliveryId !== deliveryId);
    subscriber.queue.length = 0;
    subscriber.queue.push(...next);
  }
}
