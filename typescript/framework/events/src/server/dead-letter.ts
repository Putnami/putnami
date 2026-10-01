import { DROP_DLQ_NO_SUBSCRIBER, errorText, logDropped } from '../transport/delivery-logging';
import type { Envelope } from '../transport/transport';

// ---------------------------------------------------------------------------
// Dead-letter capture
// ---------------------------------------------------------------------------

/**
 * A captured dead-letter: a message that exhausted retries (or could not be
 * re-enqueued) and had no `<topic>.dlq` subscriber to receive it. Preserves the
 * full envelope and failure cause so operators can inspect or replay it instead
 * of losing everything but a log line.
 */
export interface DeadLetter {
  /** The original envelope at the point it was dead-lettered. */
  readonly envelope: Envelope;
  /** The failure message that caused the message to be dead-lettered. */
  readonly error: string;
  /** Why the message could not be delivered to a DLQ subscriber. */
  readonly reason: 'no-dlq-subscriber';
  /** When the message was dead-lettered (epoch milliseconds). */
  readonly deadLetteredAt: number;
}

/**
 * Bounded in-memory buffer of dead-letters dropped because no `<topic>.dlq`
 * subscriber existed. Oldest entries are evicted past the configured cap.
 */
export class DeadLetterBuffer {
  private readonly entries: DeadLetter[] = [];
  private readonly maxSize: number;

  constructor(maxSize: number) {
    this.maxSize = Math.max(0, maxSize);
  }

  /** Snapshot the captured dead-letters. */
  snapshot(): readonly DeadLetter[] {
    return [...this.entries];
  }

  /** Drain (return and clear) the captured dead-letters. */
  drain(): DeadLetter[] {
    return this.entries.splice(0, this.entries.length);
  }

  /**
   * Record a dead-letter that could not be routed to a DLQ subscriber. Emits the
   * contract's `message dropped` record (reason `dlq_no_subscriber`) — NOT
   * `message dead-lettered`: with `dlq:true` but no `<topic>.dlq` subscriber the
   * message reaches no consumer, and an operator must never be pointed at a DLQ
   * the message never got to. The full envelope and failure text stay
   * retrievable through the bounded buffer.
   */
  capture(envelope: Envelope, error: unknown, dlqTopic = `${envelope.topic}.dlq`): void {
    logDropped(envelope, DROP_DLQ_NO_SUBSCRIBER, error, { dlqTopic });

    if (this.maxSize === 0) {
      return;
    }

    this.entries.push({
      envelope,
      error: errorText(error),
      reason: 'no-dlq-subscriber',
      deadLetteredAt: Date.now(),
    });
    if (this.entries.length > this.maxSize) {
      this.entries.splice(0, this.entries.length - this.maxSize);
    }
  }
}
