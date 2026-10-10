import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { resetSyncFetchForTest, syncFetch } from '../src/runtime/sync-fetch';

describe('syncFetch', () => {
  // Test files share one process: start from the real worker whatever an
  // earlier file left installed.
  beforeEach(() => {
    resetSyncFetchForTest();
  });

  afterEach(() => {
    resetSyncFetchForTest();
  });

  it('reuses the synchronous fetch worker across small bootstrap requests', () => {
    // Reset again inside the synchronous body: the hook above can run while an
    // interleaved test from another file still has its mock installed. Nothing
    // can install a mock between this line and the fetches below.
    resetSyncFetchForTest();
    const body = encodeURIComponent(JSON.stringify({ ok: true }));
    const url = `data:application/json,${body}`;

    expect(JSON.parse(syncFetch({ url }).body)).toEqual({ ok: true });
    const rssAfterFirstFetch = process.memoryUsage().rss;

    for (let i = 0; i < 5; i += 1) {
      expect(JSON.parse(syncFetch({ url }).body)).toEqual({ ok: true });
    }

    const rssGrowth = process.memoryUsage().rss - rssAfterFirstFetch;
    expect(rssGrowth).toBeLessThan(32 * 1024 * 1024);
  });

  it('fails boundedly for unsuccessful fetches', () => {
    resetSyncFetchForTest();
    expect(() => syncFetch({ url: 'http://127.0.0.1:1', timeoutMs: 50 })).toThrow();
  });
});
