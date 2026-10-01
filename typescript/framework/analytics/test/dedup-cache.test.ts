import { describe, expect, it } from 'bun:test';
import { createDedupCache, DEDUP_CAPACITY } from '../src/server/sink/dedup-cache';
import { pageViewRow } from './utils/fixtures';

describe('createDedupCache', () => {
  it('drops a row this instance already wrote', () => {
    const cache = createDedupCache();
    const row = pageViewRow();

    cache.remember([row.eventId]);

    expect(cache.filter([row])).toEqual([]);
  });

  it('lets an engagement re-send through', () => {
    const cache = createDedupCache();
    const row = pageViewRow({ engagementMs: 4200 });

    cache.remember([row.eventId]);

    // The re-send repeats the event id on purpose; dropping it here would lose
    // the only value it carries, and the database takes the GREATEST anyway.
    expect(cache.filter([row])).toEqual([row]);
  });

  it('evicts the oldest id past capacity', () => {
    const cache = createDedupCache(2);
    const first = pageViewRow({ eventId: 'a' });
    const second = pageViewRow({ eventId: 'b' });
    const third = pageViewRow({ eventId: 'c' });

    cache.remember([first.eventId, second.eventId, third.eventId]);

    expect(cache.filter([first, second, third])).toEqual([first]);
  });

  it('moves a repeated id to the end of the eviction order', () => {
    const cache = createDedupCache(2);
    const first = pageViewRow({ eventId: 'a' });
    const second = pageViewRow({ eventId: 'b' });
    const third = pageViewRow({ eventId: 'c' });

    cache.remember([first.eventId, second.eventId]);
    cache.remember([first.eventId, third.eventId]);

    expect(cache.filter([first, second, third])).toEqual([second]);
  });

  it('remembers ten thousand ids by default', () => {
    expect(DEDUP_CAPACITY).toBe(10_000);
  });
});
