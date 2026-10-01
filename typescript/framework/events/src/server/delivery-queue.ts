import { incCounter } from '@putnami/application';
import type { HandlerDefinition } from '../handler/handler';
import type { Message } from '../topic/message';
import { DROP_QUEUE_FULL, logDropped } from '../transport/delivery-logging';
import type { Envelope } from '../transport/transport';

// ---------------------------------------------------------------------------
// Subscription — internal tracking of a handler subscription
// ---------------------------------------------------------------------------

export interface Subscription {
  readonly definition: HandlerDefinition;
  readonly callback: (message: Message<unknown>) => Promise<void>;
}

interface Delivery {
  readonly envelope: Envelope;
  readonly subscription: Subscription;
}

interface DeliveryState {
  active: number;
  readonly queue: Delivery[];
}

/**
 * Per-handler concurrency limiter and overflow queue.
 *
 * Admits deliveries up to a handler's `concurrency`, parks the rest in a
 * bounded queue (`queueLimit`), and either drops or throws on overflow. As each
 * in-flight delivery completes, the next queued one is pulled — but only while
 * the broker is running or draining, so admitted work is not lost on shutdown.
 */
export class ConcurrencyQueue {
  private states = new WeakMap<Subscription, DeliveryState>();

  constructor(
    private readonly deliver: (envelope: Envelope, sub: Subscription) => void,
    private readonly canContinue: () => boolean,
  ) {}

  /** Admit a delivery: run now, queue, drop, or throw per the concurrency/overflow policy. */
  admit(envelope: Envelope, sub: Subscription): void {
    const opts = sub.definition.options;
    if (opts.concurrency <= 0) {
      this.deliver(envelope, sub);
      return;
    }

    const state = this.getState(sub);
    if (state.active < opts.concurrency) {
      state.active++;
      this.deliver(envelope, sub);
      return;
    }

    if (opts.queueLimit <= 0 || state.queue.length < opts.queueLimit) {
      state.queue.push({ envelope, subscription: sub });
      return;
    }

    const message = `Queue limit reached for '${envelope.topic}' handler (${opts.queueLimit})`;
    if (opts.overflow === 'drop') {
      // A queue-full drop loses the message, so it reports at ERROR like every
      // other drop (contract: drops are ERROR). There is no handler error to
      // attach — the delivery never ran.
      logDropped(envelope, DROP_QUEUE_FULL, undefined, { queueLimit: opts.queueLimit });
      incCounter(`events.handle.${envelope.topic}.dropped`);
      return;
    }

    throw new Error(message);
  }

  /** Mark a delivery complete and pull the next queued one when capacity frees up. */
  complete(sub: Subscription): void {
    const opts = sub.definition.options;
    if (opts.concurrency <= 0) {
      return;
    }

    const state = this.states.get(sub);
    if (!state) {
      return;
    }

    state.active = Math.max(0, state.active - 1);
    // Keep draining the queue while the broker is running OR while a graceful
    // shutdown is in progress, so admitted-but-not-started deliveries are not
    // silently dropped on stop().
    if (!this.canContinue()) {
      return;
    }

    const next = state.queue.shift();
    if (!next) {
      return;
    }

    state.active++;
    this.deliver(next.envelope, next.subscription);
  }

  /** Number of queued (admitted-but-not-started) deliveries for a subscription. */
  queuedFor(sub: Subscription): number {
    return this.states.get(sub)?.queue.length ?? 0;
  }

  /** Drop all delivery state (used during shutdown). */
  reset(): void {
    this.states = new WeakMap();
  }

  private getState(sub: Subscription): DeliveryState {
    let state = this.states.get(sub);
    if (!state) {
      state = { active: 0, queue: [] };
      this.states.set(sub, state);
    }
    return state;
  }
}
