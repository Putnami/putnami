import { type DetachedScope, useLogger } from '@putnami/runtime';
import type { HandlerDefinition } from '../handler/handler';
import type { Message } from '../topic/message';
import { asError, EVENT_BROKER_LOGGER, eventFields } from '../transport/delivery-logging';
import { createTransportMessage } from '../transport/message-utils';
import type { Envelope, Transport } from '../transport/transport';
import type { DeadLetter } from './dead-letter';
import { ConcurrencyQueue, type Subscription } from './delivery-queue';
import { FailureRouter, type FailureRouterHost } from './failure-router';
import { runHandler, type HandlerRunnerDeps } from './handler-runner';

export type { DeadLetter };

// ---------------------------------------------------------------------------
// In-flight message tracking (for graceful shutdown)
// ---------------------------------------------------------------------------

interface InFlightEntry {
  promise: Promise<void>;
  resolve: () => void;
}

/** Construction options for {@link MemoryBroker}. */
export interface MemoryBrokerOptions {
  /**
   * Deliberately redeliver ~2% of successfully-handled messages to surface
   * non-idempotent handlers during local development. Default: false.
   */
  simulateDuplicates?: boolean;
  /** Maximum time (ms) to wait for in-flight messages during shutdown. Default: 10_000. */
  drainTimeout?: number;
  /**
   * Maximum number of captured dead-letters retained in the in-memory buffer
   * (the ones with `dlq:true` but no `<topic>.dlq` subscriber). Oldest entries
   * are evicted past this cap. Default: 100.
   */
  deadLetterBufferSize?: number;
}

// ---------------------------------------------------------------------------
// MemoryBroker — in-memory Transport for local development
// ---------------------------------------------------------------------------

/**
 * In-memory event broker for local development.
 *
 * Faithfully reproduces production behaviour:
 * - Competing consumers (round-robin) and broadcast distribution
 * - Exponential backoff retries with max delay cap
 * - Dead-letter queue (messages logged after max retries)
 * - Attribute-based filtering
 * - Graceful shutdown that drains both in-flight and concurrency-queued messages
 * - Each handler invocation runs in its own async context (runInContext)
 * - Telemetry metrics for handle count, duration, retries, DLQ
 * - Structured logging around message lifecycle
 *
 * Orchestrates a {@link ConcurrencyQueue} (per-handler concurrency/queueing),
 * a {@link FailureRouter} (retry + dead-letter), and {@link runHandler} (per-
 * invocation context, telemetry, and validation).
 *
 * Optionally (opt-in via `simulateDuplicates`), deliberately redelivers ~2% of
 * messages to train developers to write idempotent handlers. This is off by
 * default so the default local-dev path stays deterministic.
 */
export class MemoryBroker implements Transport {
  /** topic → list of subscriptions */
  private subscriptions = new Map<string, Subscription[]>();
  /** Round-robin counters for competing distribution */
  private roundRobin = new Map<string, number>();
  /** Track in-flight message processing for graceful shutdown */
  private inFlight = new Set<InFlightEntry>();
  /** Per-handler concurrency limiter and overflow queue */
  private readonly queue: ConcurrencyQueue;
  /** Retry + dead-letter routing for failed deliveries */
  private readonly failureRouter: FailureRouter;
  /** Whether the broker is accepting new messages */
  private running = false;
  /** Whether the broker is draining queued/in-flight work during shutdown */
  private draining = false;
  /**
   * Whether to deliberately redeliver ~2% of messages to surface
   * non-idempotent handlers. Opt-in; off by default.
   */
  private readonly simulateDuplicates: boolean;
  /** Maximum time to wait for in-flight messages during shutdown */
  private readonly drainTimeout: number;
  /** Optional factory for creating DI scopes per handler invocation */
  private scopeFactory?: () => Promise<DetachedScope>;

  constructor(options?: MemoryBrokerOptions) {
    this.simulateDuplicates = options?.simulateDuplicates ?? false;
    this.drainTimeout = options?.drainTimeout ?? 10_000;

    this.queue = new ConcurrencyQueue(
      (envelope, sub) => this.deliver(envelope, sub),
      () => this.running || this.draining,
    );

    const host: FailureRouterHost = {
      enqueue: (envelope, sub) => this.enqueueDelivery(envelope, sub),
      selectTargets: (topic, subs, envelope) => this.selectTargets(topic, subs, envelope),
      getSubscriptions: (topic) => this.subscriptions.get(topic),
      isRunning: () => this.running,
    };
    this.failureRouter = new FailureRouter(host, options?.deadLetterBufferSize ?? 100);
  }

  /**
   * Snapshot the captured dead-letters — messages that exhausted retries with
   * `dlq:true` but had no `<topic>.dlq` subscriber to receive them. Useful for
   * operators and tests that need to assert nothing was silently lost.
   */
  getDeadLetters(): readonly DeadLetter[] {
    return this.failureRouter.getDeadLetters();
  }

  /** Drain (return and clear) the captured dead-letter buffer. */
  drainDeadLetters(): DeadLetter[] {
    return this.failureRouter.drainDeadLetters();
  }

  /**
   * Sets the DI scope factory for per-handler scoping.
   * When set, each handler invocation gets its own DI scope.
   * @internal Used by EventsPlugin to wire DI support.
   */
  setScopeFactory(factory: () => Promise<DetachedScope>): void {
    this.scopeFactory = factory;
  }

  async start(): Promise<void> {
    this.running = true;
  }

  async stop(): Promise<void> {
    // Stop accepting new messages, but keep draining queued/in-flight work.
    this.running = false;
    this.draining = true;

    try {
      // Drain in-flight messages AND deliveries still parked in concurrency
      // queues. Dequeuing a queued delivery (see completeDelivery) registers a
      // fresh in-flight entry, so we loop until both the in-flight set and all
      // queues are empty — bounded by the configured drain timeout.
      if (this.inFlight.size > 0 || this.hasQueuedDeliveries()) {
        const logger = useLogger(EVENT_BROKER_LOGGER);
        logger.debug(`Draining ${this.inFlight.size} in-flight and ${this.queuedDeliveryCount()} queued message(s)...`);

        const deadline = Date.now() + this.drainTimeout;
        let timedOut = false;
        let timeoutId: ReturnType<typeof setTimeout> | undefined;
        const timeout = new Promise<void>((resolve) => {
          timeoutId = setTimeout(() => {
            timedOut = true;
            resolve();
          }, this.drainTimeout);
        });

        try {
          while ((this.inFlight.size > 0 || this.hasQueuedDeliveries()) && Date.now() < deadline) {
            const drain = Promise.all([...this.inFlight].map((e) => e.promise));
            await Promise.race([drain, timeout]);
            if (timedOut) break;
          }
        } finally {
          if (timeoutId !== undefined) {
            clearTimeout(timeoutId);
          }
        }

        const remaining = this.inFlight.size + this.queuedDeliveryCount();
        if (remaining > 0) {
          logger.warn(`Drain timed out after ${this.drainTimeout}ms with ${remaining} message(s) still pending`);
        }
      }
    } finally {
      this.draining = false;
      this.failureRouter.cancelRetries();
      this.subscriptions.clear();
      this.roundRobin.clear();
      this.inFlight.clear();
      this.queue.reset();
    }
  }

  /** Whether any subscription still has queued (admitted-but-not-started) deliveries. */
  private hasQueuedDeliveries(): boolean {
    for (const subs of this.subscriptions.values()) {
      for (const sub of subs) {
        if (this.queue.queuedFor(sub) > 0) {
          return true;
        }
      }
    }
    return false;
  }

  /** Total number of queued deliveries across all subscriptions. */
  private queuedDeliveryCount(): number {
    let count = 0;
    for (const subs of this.subscriptions.values()) {
      for (const sub of subs) {
        count += this.queue.queuedFor(sub);
      }
    }
    return count;
  }

  async publish(_topic: string, envelope: Envelope): Promise<void> {
    if (!this.running) {
      throw new Error('Broker is not running. Call start() first.');
    }

    const env = envelope;
    const topicName = env.topic;
    const subs = this.subscriptions.get(topicName);
    if (!subs || subs.length === 0) {
      return; // No subscribers — message is dropped silently
    }

    // Deliver to matching subscribers
    const targets = this.selectTargets(topicName, subs, env);
    for (const sub of targets) {
      this.enqueueDelivery(env, sub);
    }
  }

  async subscribe(
    definition: HandlerDefinition,
    callback: (message: Message<unknown>) => Promise<void>,
  ): Promise<void> {
    const topicName = definition.topic.name;
    const existing = this.subscriptions.get(topicName) ?? [];
    existing.push({ definition, callback });
    this.subscriptions.set(topicName, existing);
  }

  unsubscribe(topicName: string, callback: (message: Message<unknown>) => Promise<void>): void {
    const existing = this.subscriptions.get(topicName);
    if (!existing) {
      return;
    }

    const next = existing.filter((subscription) => subscription.callback !== callback);
    if (next.length === 0) {
      this.subscriptions.delete(topicName);
      this.roundRobin.delete(topicName);
      return;
    }

    this.subscriptions.set(topicName, next);
    const currentIndex = this.roundRobin.get(topicName) ?? 0;
    this.roundRobin.set(topicName, currentIndex % next.length);
  }

  // ---------------------------------------------------------------------------
  // Internal — target selection
  // ---------------------------------------------------------------------------

  private selectTargets(topicName: string, subs: Subscription[], envelope: Envelope): Subscription[] {
    // Filter by attributes
    const matching = subs.filter((sub) => {
      if (!sub.definition.filter) return true;
      const required = sub.definition.filter.attributes;
      for (const [key, value] of Object.entries(required)) {
        if (envelope.attributes[key] !== value) return false;
      }
      return true;
    });

    if (matching.length === 0) return [];

    // Separate by distribution mode
    const broadcast = matching.filter((s) => s.definition.options.distribution === 'broadcast');
    const competing = matching.filter((s) => s.definition.options.distribution === 'competing');

    const targets: Subscription[] = [...broadcast];

    // Round-robin for competing consumers
    if (competing.length > 0) {
      const idx = this.roundRobin.get(topicName) ?? 0;
      targets.push(competing[idx % competing.length]);
      this.roundRobin.set(topicName, (idx + 1) % competing.length);
    }

    return targets;
  }

  // ---------------------------------------------------------------------------
  // Internal — delivery admission and execution
  //
  // `enqueueDelivery`/`completeDelivery` stay thin methods on the broker (rather
  // than inlining ConcurrencyQueue calls at every call site) so that all
  // delivery — initial publish, retries, and DLQ routing — flows through one
  // interceptable seam.
  // ---------------------------------------------------------------------------

  private enqueueDelivery(envelope: Envelope, sub: Subscription): void {
    this.queue.admit(envelope, sub);
  }

  private completeDelivery(sub: Subscription): void {
    this.queue.complete(sub);
  }

  private deliver(envelope: Envelope, sub: Subscription): void {
    const { message, ackState, abortController } = createTransportMessage(envelope, sub.definition.options);

    // Track in-flight
    let resolveInFlight: () => void;
    const entry: InFlightEntry = {
      promise: new Promise<void>((r) => {
        resolveInFlight = r;
      }),
      resolve: () => resolveInFlight(),
    };
    this.inFlight.add(entry);

    const deps: HandlerRunnerDeps = {
      scopeFactory: this.scopeFactory,
      onFailure: (error, env, target) => this.failureRouter.handleFailure(error, env, target),
    };

    runHandler(message, ackState, abortController, envelope, sub, deps)
      .then((): Promise<void> | void => {
        // Opt-in: ~2% chance of duplicate delivery to train idempotent handlers
        if (this.simulateDuplicates && Math.random() < 0.02) {
          const logger = useLogger(EVENT_BROKER_LOGGER);
          logger.warn(`[dev] Simulated duplicate delivery for message ${envelope.id} on topic '${envelope.topic}'`);
          const duplicateEnvelope = { ...envelope, attempt: envelope.attempt + 1 };
          const duplicate = createTransportMessage(duplicateEnvelope, sub.definition.options);
          return runHandler(
            duplicate.message,
            duplicate.ackState,
            duplicate.abortController,
            duplicateEnvelope,
            sub,
            deps,
          );
        }
      })
      .catch((error: unknown) => {
        // Defence in depth: runHandler routes handler/scope failures to the
        // failure router internally, so this rejection is residual — a scope
        // factory that rejected with no onFailure, a rejecting duplicate
        // delivery, or a scope close() that threw. Route it to the failure
        // router rather than letting it surface as an unhandled rejection that
        // could crash the worker. The router itself is wrapped so a throw here
        // never re-escapes as an unhandled rejection.
        try {
          this.failureRouter.handleFailure(error, envelope, sub);
        } catch (routingError) {
          // Stable message + structured event group + the real error object. The
          // message deliberately stays OUTSIDE the contract's closed `message …`
          // vocabulary: this is an independent operational signal (the routing
          // itself broke), not one of the five event records.
          useLogger(EVENT_BROKER_LOGGER).error('failure routing failed', asError(routingError), eventFields(envelope));
        }
      })
      .finally(() => {
        this.inFlight.delete(entry);
        this.completeDelivery(sub);
        entry.resolve();
      });
  }
}
