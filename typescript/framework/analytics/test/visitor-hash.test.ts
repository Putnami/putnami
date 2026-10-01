import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { dayKey, utcDay, visitorHash, VISITOR_ID_LENGTH } from '../src/server/identity/visitor-hash';

const FEATURE = 'typescript/web-analytics-collection';
const REQUIREMENT = 'no-network-address-or-raw-user-agent-is-persisted';
const EMITS_22 = 'the-visitor-hash-consumes-the-ip-and-emits-22-chars';

const BASE = {
  secret: 'a-server-side-key-of-at-least-32-chars',
  ts: new Date('2026-09-02T10:00:00.000Z'),
  app: 'demo',
  clientIp: '203.0.113.7',
  uaFamily: 'chrome/macos',
};

describe('visitorHash', () => {
  specTest(
    'consumes the client IP and emits 22 base64url characters',
    { feature: FEATURE, requirement: REQUIREMENT, check: EMITS_22 },
    () => {
      const id = visitorHash(BASE);

      expect(id).toHaveLength(VISITOR_ID_LENGTH);
      expect(id).toMatch(/^[A-Za-z0-9_-]{22}$/);
      // The IP is an input, never an output: it changes the id and appears
      // nowhere in it.
      expect(id).not.toContain('203');
      expect(visitorHash({ ...BASE, clientIp: '203.0.113.8' })).not.toBe(id);
    },
  );

  specTest(
    'rotates at UTC midnight',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'the-hash-rotates-at-utc-midnight' },
    () => {
      const lateOnTheSecond = visitorHash({ ...BASE, ts: new Date('2026-09-02T23:59:59.999Z') });
      const justAfterMidnight = visitorHash({ ...BASE, ts: new Date('2026-09-03T00:00:00.000Z') });

      expect(visitorHash({ ...BASE, ts: new Date('2026-09-02T00:00:00.000Z') })).toBe(lateOnTheSecond);
      expect(justAfterMidnight).not.toBe(lateOnTheSecond);
    },
  );

  it('is stable for the same visitor on the same day', () => {
    expect(visitorHash(BASE)).toBe(visitorHash({ ...BASE, ts: new Date('2026-09-02T18:30:00.000Z') }));
  });

  it('separates user-agent families, applications, and secrets', () => {
    const id = visitorHash(BASE);

    expect(visitorHash({ ...BASE, uaFamily: 'firefox/macos' })).not.toBe(id);
    expect(visitorHash({ ...BASE, app: 'other' })).not.toBe(id);
    expect(visitorHash({ ...BASE, secret: 'a-different-server-side-key-of-32-chars' })).not.toBe(id);
  });
});

describe('utcDay', () => {
  it('renders the UTC calendar day', () => {
    expect(utcDay(new Date('2026-09-02T23:59:59.999Z'))).toBe('2026-09-02');
    expect(utcDay(new Date('2026-09-03T00:00:00.000Z'))).toBe('2026-09-03');
  });
});

describe('dayKey', () => {
  it('derives 32 unrelated bytes per day', () => {
    const first = dayKey(BASE.secret, '2026-09-02');
    const second = dayKey(BASE.secret, '2026-09-03');

    expect(first).toHaveLength(32);
    expect(first.equals(second)).toBe(false);
    expect(dayKey(BASE.secret, '2026-09-02').equals(first)).toBe(true);
  });
});
