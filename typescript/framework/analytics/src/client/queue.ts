import type { ClientBootstrap, WireEvent } from './wire';

/** Prefix of every app-owned queue in `localStorage` (body §D.15). */
export const QUEUE_KEY_PREFIX = 'putnami.analytics.queue';

/** The queue never holds more than this many events. */
export const QUEUE_CAPACITY = 200;

/** The persistent, bounded outbox of the tracker. */
export interface Queue {
  /** Appends an event, or replaces the entry that already carries its id. */
  push(event: WireEvent): void;
  /** Returns the first `max` events in insertion order, without removing them. */
  take(max: number): WireEvent[];
  /** Removes the events the server accepted (or that will never be accepted). */
  ack(ids: string[]): void;
  /** How many events are waiting. */
  size(): number;
}

/**
 * Derives the stable storage owner of one tracker bootstrap.
 *
 * `localStorage` is origin-wide, while Putnami applications can be mounted at
 * different paths on one origin. Both the application and its ingest endpoint
 * therefore participate in the key so one app can never deliver another
 * app's durable events to its own endpoint.
 *
 * A key that is exactly {@link QUEUE_KEY_PREFIX} is never read: its events
 * carry no trustworthy owner, and letting the first app loaded on an origin
 * claim them would disclose one app's events to another.
 */
export function queueKey(boot: Pick<ClientBootstrap, 'app' | 'endpoint'>): string {
  return `${QUEUE_KEY_PREFIX}:${encodeURIComponent(boot.app)}:${encodeURIComponent(boot.endpoint)}`;
}

function isWireEvent(value: unknown): value is WireEvent {
  const event = value as Partial<WireEvent> | null;
  return typeof event?.eventId === 'string' && typeof event.name === 'string';
}

/**
 * Creates the persistent client queue (body §D.15).
 *
 * It lives in `localStorage` so that events written just before an unload are
 * retried on the next page, which is the only way a beacon that lost the race
 * with the tab closing ever reaches the server.
 *
 * Two rules carry the semantics:
 *
 * - `push` **replaces** an entry with the same `eventId` instead of appending.
 *   An engagement re-send is the same event with a larger `engagementMs`, so
 *   queueing both would send the stale one first and waste a slot.
 * - Over capacity the **oldest** event is dropped. Analytics are lossy under
 *   pressure by design; dropping the newest would mean losing the page the
 *   visitor is on rather than one they left long ago.
 *
 * Any `localStorage` failure (private mode, quota) switches the queue to
 * memory for the page lifetime.
 *
 * @param key - The app-and-endpoint-scoped `localStorage` key.
 * @param capacity - The largest number of events kept.
 * @returns The queue, preloaded with whatever a previous page left behind.
 */
export function createQueue(key: string, capacity: number = QUEUE_CAPACITY): Queue {
  let inMemory = false;
  let events: WireEvent[] = [];

  try {
    const raw = localStorage.getItem(key);
    const parsed: unknown = raw ? JSON.parse(raw) : [];
    if (Array.isArray(parsed)) {
      events = parsed.filter(isWireEvent).slice(-capacity);
    }
  } catch {
    inMemory = true;
  }

  const persist = (): void => {
    if (inMemory) {
      return;
    }
    try {
      localStorage.setItem(key, JSON.stringify(events));
    } catch {
      inMemory = true;
    }
  };

  return {
    push(event: WireEvent): void {
      const at = events.findIndex((queued) => queued.eventId === event.eventId);
      if (at >= 0) {
        events[at] = event;
      } else {
        events.push(event);
      }
      while (events.length > capacity) {
        events.shift();
      }
      persist();
    },
    take(max: number): WireEvent[] {
      return events.slice(0, max);
    },
    ack(ids: string[]): void {
      const acked = new Set(ids);
      events = events.filter((queued) => !acked.has(queued.eventId));
      persist();
    },
    size(): number {
      return events.length;
    },
  };
}
