// Simple in-memory event store — the "fetch on demand" side of the pattern.
// Handlers record events as they process them; the GET /events endpoint queries this store.

interface StoredEvent {
  topic: string;
  payload: unknown;
  timestamp: string;
}

const events: StoredEvent[] = [];

export function record(topic: string, payload: unknown) {
  events.push({ topic, payload, timestamp: new Date().toISOString() });
}

export function getEvents() {
  return [...events];
}

export function clearEvents() {
  events.length = 0;
}
