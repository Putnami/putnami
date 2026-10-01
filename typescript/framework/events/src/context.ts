import type { Context } from '@putnami/runtime';

/**
 * Event handler context — extends the base Context with event-specific fields.
 * Available inside handlers via `useContext()`.
 */
export type EventContext = Context & {
  /** The topic this message belongs to */
  eventTopic: string;
  /** Unique message ID */
  eventMessageId: string;
  /** Current delivery attempt (1-based) */
  eventAttempt: number;
};
