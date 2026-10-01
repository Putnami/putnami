import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { createQueue, QUEUE_CAPACITY, QUEUE_KEY_PREFIX, queueKey } from '../src/client/queue';
import type { WireEvent } from '../src/client/wire';
import { type FakeBrowser, installFakeBrowser } from './utils/fake-browser';

const SESSION = '0192f0c0-1111-7aaa-8bbb-000000000001';
const KEY = queueKey({ app: 'demo', endpoint: '/_putnami/analytics/events' });

function pageView(eventId: string, engagementMs?: number): WireEvent {
  const event: WireEvent = {
    eventId,
    name: 'page_view',
    clientTs: '2026-09-02T10:00:00.000Z',
    seq: 0,
    sessionId: SESSION,
    page: { path: '/' },
  };
  if (engagementMs !== undefined) {
    event.engagementMs = engagementMs;
  }
  return event;
}

function id(index: number): string {
  return `0192f0c0-0000-7000-8000-${String(index).padStart(12, '0')}`;
}

describe('createQueue', () => {
  let fake: FakeBrowser;

  beforeEach(() => {
    fake = installFakeBrowser();
  });

  afterEach(() => {
    fake.uninstall();
  });

  it('survives the page: a new queue reads what the previous one persisted', () => {
    const first = createQueue(KEY);
    first.push(pageView(id(1)));
    first.push(pageView(id(2)));

    // The whole point of localStorage: a beacon that lost the race with the
    // tab closing is retried by the next page, not lost.
    const second = createQueue(KEY);

    expect(second.size()).toBe(2);
    expect(second.take(10).map((event) => event.eventId)).toEqual([id(1), id(2)]);
    expect(fake.localStorage.entries.has(KEY)).toBe(true);
  });

  it('isolates durable events by application and endpoint', () => {
    const docsKey = queueKey({ app: 'docs', endpoint: '/docs/_putnami/analytics/events' });
    const otherAppKey = queueKey({ app: 'shop', endpoint: '/docs/_putnami/analytics/events' });
    const otherEndpointKey = queueKey({ app: 'docs', endpoint: '/shop/_putnami/analytics/events' });
    const docs = createQueue(docsKey);
    const shop = createQueue(otherEndpointKey);

    docs.push(pageView(id(1)));
    shop.push(pageView(id(2)));

    expect(docs.take(10).map((event) => event.eventId)).toEqual([id(1)]);
    expect(shop.take(10).map((event) => event.eventId)).toEqual([id(2)]);
    expect(new Set([docsKey, otherAppKey, otherEndpointKey]).size).toBe(3);
  });

  it('does not let an app claim events from the legacy origin-wide key', () => {
    fake.localStorage.entries.set(QUEUE_KEY_PREFIX, JSON.stringify([pageView(id(1))]));

    expect(createQueue(KEY).size()).toBe(0);
    expect(fake.localStorage.entries.has(QUEUE_KEY_PREFIX)).toBe(true);
  });

  it('replaces the entry that already carries the id instead of appending', () => {
    const queue = createQueue(KEY);
    queue.push(pageView(id(1)));
    queue.push(pageView(id(2)));

    // An engagement re-send is the same event with a larger engagementMs.
    queue.push(pageView(id(1), 4200));

    expect(queue.size()).toBe(2);
    const [first, second] = queue.take(10);
    expect(first?.eventId).toBe(id(1));
    expect(first?.engagementMs).toBe(4200);
    expect(second?.eventId).toBe(id(2));
  });

  it('drops the oldest event once it is over capacity', () => {
    const queue = createQueue(KEY);
    for (let index = 0; index < QUEUE_CAPACITY + 5; index++) {
      queue.push(pageView(id(index)));
    }

    expect(queue.size()).toBe(QUEUE_CAPACITY);
    const kept = queue.take(QUEUE_CAPACITY).map((event) => event.eventId);
    expect(kept[0]).toBe(id(5));
    expect(kept[kept.length - 1]).toBe(id(QUEUE_CAPACITY + 4));
  });

  it('takes in insertion order without removing, and acks by id', () => {
    const queue = createQueue(KEY);
    for (let index = 0; index < 4; index++) {
      queue.push(pageView(id(index)));
    }

    const batch = queue.take(2);
    expect(batch.map((event) => event.eventId)).toEqual([id(0), id(1)]);
    expect(queue.size()).toBe(4);

    queue.ack(batch.map((event) => event.eventId));

    expect(queue.take(10).map((event) => event.eventId)).toEqual([id(2), id(3)]);
  });

  it('falls back to memory when storage throws, and keeps the page working', () => {
    fake.localStorage.breaks();
    const queue = createQueue(KEY);

    queue.push(pageView(id(1)));
    queue.push(pageView(id(2)));

    expect(queue.size()).toBe(2);
    expect(fake.localStorage.entries.size).toBe(0);
  });

  it('ignores a corrupted or foreign payload rather than throwing', () => {
    fake.localStorage.entries.set(KEY, '{"not":"an array"}');
    expect(createQueue(KEY).size()).toBe(0);

    fake.localStorage.entries.set(KEY, 'not json at all');
    expect(createQueue(KEY).size()).toBe(0);

    fake.localStorage.entries.set(KEY, JSON.stringify([{ eventId: 1 }, pageView(id(3))]));
    expect(
      createQueue(KEY)
        .take(10)
        .map((event) => event.eventId),
    ).toEqual([id(3)]);
  });
});
