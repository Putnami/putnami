import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { encodeBatch, PROTOCOL_VERSION, send } from '../src/client/transport';
import type { WireEvent } from '../src/client/wire';
import { type FakeBrowser, installFakeBrowser } from './utils/fake-browser';

const ENDPOINT = '/_putnami/analytics/events';

const EVENTS: WireEvent[] = [
  {
    eventId: '0192f0c0-2222-7aaa-8bbb-000000000001',
    name: 'page_view',
    clientTs: '2026-09-02T10:00:00.000Z',
    seq: 0,
    sessionId: '0192f0c0-2222-7aaa-8bbb-000000000002',
    page: { path: '/docs' },
  },
];

describe('encodeBatch', () => {
  it('writes the envelope the ingest route parses', () => {
    const body = JSON.parse(encodeBatch(EVENTS, Date.UTC(2026, 8, 2, 10, 0, 0)));

    expect(body.protocolVersion).toBe(PROTOCOL_VERSION);
    // The timestamp regex of the protocol: milliseconds and a literal Z.
    expect(body.sentAt).toBe('2026-09-02T10:00:00.000Z');
    expect(body.events).toHaveLength(1);
  });
});

describe('send', () => {
  let fake: FakeBrowser;

  afterEach(() => {
    fake.uninstall();
  });

  describe('on unload', () => {
    beforeEach(() => {
      fake = installFakeBrowser({ beacon: true });
    });

    it('beacons a text/plain blob and never touches fetch', async () => {
      const outcome = await send(ENDPOINT, EVENTS, { unload: true });

      expect(outcome).toBe('pending');
      expect(fake.fetchCalls).toHaveLength(0);
      expect(fake.beaconCalls).toHaveLength(1);
      const [call] = fake.beaconCalls;
      expect(call?.url).toBe(ENDPOINT);
      // text/plain is CORS-safelisted, so the beacon is a simple request and
      // the closing document never has to survive a preflight.
      expect(call?.type.startsWith('text/plain')).toBe(true);
      expect(JSON.parse(await (call as { blob: Blob }).blob.text()).events).toHaveLength(1);
    });

    it('falls back to fetch when the browser refuses the beacon', async () => {
      fake.beaconResult = false;

      const outcome = await send(ENDPOINT, EVENTS, { unload: true });

      expect(outcome).toBe('ok');
      expect(fake.beaconCalls).toHaveLength(1);
      expect(fake.fetchCalls).toHaveLength(1);
    });
  });

  describe('over fetch', () => {
    beforeEach(() => {
      fake = installFakeBrowser();
    });

    it('keeps the request alive and same-origin', async () => {
      await send(ENDPOINT, EVENTS, { unload: false });

      const [call] = fake.fetchCalls;
      expect(call?.init.method).toBe('POST');
      expect(call?.init.headers).toEqual({ 'Content-Type': 'application/json' });
      // keepalive: an in-flight batch survives the navigation that triggered it.
      expect(call?.init.keepalive).toBe(true);
      // same-origin: the route is same-origin and unauthenticated; sending
      // credentials anywhere else would be a leak.
      expect(call?.init.credentials).toBe('same-origin');
    });

    it('uses fetch on unload too when the browser has no sendBeacon', async () => {
      expect(await send(ENDPOINT, EVENTS, { unload: true })).toBe('ok');
      expect(fake.fetchCalls).toHaveLength(1);
    });

    it.each([
      [202, 'ok'],
      [200, 'ok'],
      [400, 'drop'],
      [404, 'drop'],
      [413, 'drop'],
      [429, 'retry'],
      [500, 'retry'],
      [503, 'retry'],
    ])('answers %p with %p', async (status, expected) => {
      fake.responses.push(status as number);

      expect(await send(ENDPOINT, EVENTS, { unload: false })).toBe(expected);
    });

    it('retries a network error', async () => {
      fake.responses.push(new Error('offline'));

      // Unreachable is not "rejected": the ids make a later success idempotent.
      expect(await send(ENDPOINT, EVENTS, { unload: false })).toBe('retry');
    });
  });
});
