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

export interface Delivery {
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
  /**
   * The states whose queue holds a delivery. `states` is weak so an unsubscribed
   * handler's state can go; this set keeps a reference only while it has queued
   * work, so `stop()` can count and discard every queued delivery, including one
   * whose handler was unsubscribed meanwhile.
   */
  private readonly parked = new Set<DeliveryState>();

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
      this.parked.add(state);
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
    if (state.queue.length === 0) {
      this.parked.delete(state);
    }

    state.active++;
    this.deliver(next.envelope, next.subscription);
  }

  /** Number of queued (admitted-but-not-started) deliveries across all subscriptions. */
  queued(): number {
    let count = 0;
    for (const state of this.parked) {
      count += state.queue.length;
    }
    return count;
  }

  /**
   * Drop all delivery state (used during shutdown) and return the deliveries
   * that were still queued, so the caller can record each one it abandons.
   */
  reset(): Delivery[] {
    const abandoned: Delivery[] = [];
    for (const state of this.parked) {
      abandoned.push(...state.queue);
    }
    this.parked.clear();
    this.states = new WeakMap();
    return abandoned;
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
