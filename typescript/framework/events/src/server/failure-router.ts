import { incCounter } from '@putnami/application';
import type { ResolvedHandlerOptions } from '../handler/handler.type';
import {
  DROP_DLQ_ENQUEUE_FAILED,
  DROP_DLQ_NO_SUBSCRIBER,
  DROP_RETRIES_EXHAUSTED,
  DROP_RETRY_ENQUEUE_FAILED,
  type DropReason,
  errorText,
  logDeadLettered,
  logDropped,
  logRetryScheduled,
} from '../transport/delivery-logging';
import type { Envelope } from '../transport/transport';
import { DeadLetterBuffer, type DeadLetter } from './dead-letter';
import type { Subscription } from './delivery-queue';

// ---------------------------------------------------------------------------
// Failure routing — exponential-backoff retries and dead-letter queue
// ---------------------------------------------------------------------------

/**
 * Broker capabilities the router calls back into. `enqueue` deliberately routes
 * through the broker's own enqueue method so a single delivery path (and its
 * concurrency/overflow policy and test seams) stays authoritative.
 */
export interface FailureRouterHost {
  /** Enqueue a (re)delivery via the broker's delivery path. */
  enqueue(envelope: Envelope, sub: Subscription): void;
  /** Select delivery targets for a topic (attribute filter + competing round-robin). */
  selectTargets(topic: string, subs: Subscription[], envelope: Envelope): Subscription[];
  /** Look up the subscriptions registered for a topic. */
  getSubscriptions(topic: string): Subscription[] | undefined;
  /** Whether the broker is still running (retries scheduled before stop() bail). */
  isRunning(): boolean;
}

/**
 * Decides what happens to a failed delivery: schedule an exponential-backoff
 * retry while attempts remain, otherwise route to the `<topic>.dlq` subscribers
 * or capture it as a dead-letter when none exist.
 */
export class FailureRouter {
  private readonly retryTimers = new Set<ReturnType<typeof setTimeout>>();
  private readonly deadLetters: DeadLetterBuffer;

  constructor(
    private readonly host: FailureRouterHost,
    deadLetterBufferSize: number,
  ) {
    this.deadLetters = new DeadLetterBuffer(deadLetterBufferSize);
  }

  getDeadLetters(): readonly DeadLetter[] {
    return this.deadLetters.snapshot();
  }

  drainDeadLetters(): DeadLetter[] {
    return this.deadLetters.drain();
  }

  /** Cancel all pending retry timers (used during shutdown). */
  cancelRetries(): void {
    for (const timer of this.retryTimers) {
      clearTimeout(timer);
    }
    this.retryTimers.clear();
  }

  handleFailure(error: unknown, envelope: Envelope, sub: Subscription): void {
    const opts = sub.definition.options;

    if (envelope.attempt < opts.maxRetries) {
      this.scheduleRetry(envelope, sub, error);
    } else {
      this.sendToDlq(envelope, opts, error);
    }
  }

  /**
   * The raw thrown value travels all the way down (never pre-stringified), so
   * every record carries the logger's structured `error` field instead of error
   * text interpolated into a message.
   */
  private scheduleRetry(envelope: Envelope, sub: Subscription, error: unknown): void {
    const opts = sub.definition.options;
    const delay = calculateBackoff(envelope.attempt, opts.maxBackoff);

    logRetryScheduled(envelope, delay, opts.maxRetries, error);

    // Telemetry: retry counter
    incCounter(`events.handle.${envelope.topic}.retry`);

    const timer = setTimeout(() => {
      this.retryTimers.delete(timer);
      if (!this.host.isRunning()) return;
      const retryEnvelope: Envelope = { ...envelope, attempt: envelope.attempt + 1 };
      // Re-enqueue can throw (e.g. queue full with overflow:'throw'). This runs
      // in a bare timer callback, so a throw would become an uncaught exception
      // that crashes the worker. Catch it and route the message to the DLQ
      // (or log-and-drop when no DLQ is configured) instead of escaping.
      try {
        this.host.enqueue(retryEnvelope, sub);
      } catch (enqueueError) {
        // The re-delivery could not be queued: `retry_enqueue_failed`, not
        // `retries_exhausted` — retries may well remain, the queue refused this
        // one.
        this.sendToDlq(retryEnvelope, opts, enqueueError, DROP_RETRY_ENQUEUE_FAILED);
      }
    }, delay);
    this.retryTimers.add(timer);
  }

  private sendToDlq(
    envelope: Envelope,
    opts: ResolvedHandlerOptions,
    error: unknown,
    dropReason: DropReason = DROP_RETRIES_EXHAUSTED,
  ): void {
    if (!opts.dlq) {
      // No DLQ is configured, so the message is lost. `attempt` is the delivery's
      // TRUE final count — never attempt + 1.
      logDropped(envelope, dropReason, error);
      return;
    }

    // Telemetry: DLQ counter
    incCounter(`events.handle.${envelope.topic}.dlq`);

    const dlqTopicName = `${envelope.topic}.dlq`;
    const dlqSubs = this.host.getSubscriptions(dlqTopicName);
    if (!dlqSubs || dlqSubs.length === 0) {
      // dlq:true but nobody is listening on `<topic>.dlq`. Don't silently drop
      // the message after a log line — preserve the full envelope and error in
      // a bounded in-memory buffer and emit a structured record so operators
      // (and tests) can inspect or replay what was lost.
      this.deadLetters.capture(envelope, error, dlqTopicName);
      return;
    }

    const dlqEnvelope: Envelope = {
      ...envelope,
      topic: dlqTopicName,
      attempt: 1,
      attributes: {
        ...envelope.attributes,
        'dlq.original_topic': envelope.topic,
        'dlq.original_attempt': String(envelope.attempt),
        'dlq.error': errorText(error),
      },
    };

    const targets = this.host.selectTargets(dlqTopicName, dlqSubs, dlqEnvelope);
    if (targets.length === 0) {
      // `<topic>.dlq` has subscribers but an attribute filter matched none of
      // them, so nothing will ever receive the message: that is a DROP. Claiming
      // a dead-letter here would point an operator at a DLQ the message never
      // reached.
      logDropped(envelope, DROP_DLQ_NO_SUBSCRIBER, error, { dlqTopic: dlqTopicName });
      return;
    }

    let accepted = 0;
    let enqueueError: unknown;
    for (const dlqSub of targets) {
      try {
        this.host.enqueue(dlqEnvelope, dlqSub);
        accepted++;
      } catch (thrown) {
        enqueueError ??= thrown;
      }
    }
    if (accepted === 0) {
      // Nothing took it, so the message is lost: ONE message-level drop record,
      // never one per refusing subscriber — the record describes the message's
      // fate, not each attempt to hand it over. (Go's memory broker mirrors this
      // exactly; see `sendToDLQ`.)
      logDropped(envelope, DROP_DLQ_ENQUEUE_FAILED, enqueueError, { dlqTopic: dlqTopicName });
      return;
    }
    // Exactly one dead-letter record per dead-letter (never one per `.dlq`
    // subscriber), emitted only once the dead-letter has actually been accepted,
    // and never alongside a drop: a message at least one subscriber accepted is
    // not lost, so a drop record here would be false.
    logDeadLettered(envelope, dlqTopicName, error);
  }
}

function calculateBackoff(attempt: number, maxBackoff: number): number {
  // Exponential: 1s, 2s, 4s, 8s, 16s, 32s, 60s, 60s, ...
  const base = 1000;
  const delay = Math.min(base * 2 ** (attempt - 1), maxBackoff);
  // Add jitter (0-25%)
  const jitter = delay * Math.random() * 0.25;
  return Math.floor(delay + jitter);
}
