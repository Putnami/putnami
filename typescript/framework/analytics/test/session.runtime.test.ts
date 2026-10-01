import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { createSession, SESSION_IDLE_MS, SESSION_KEY } from '../src/client/session';
import { UUID_V7_RE } from '../src/server/sanitize/vocabulary';
import { type FakeBrowser, installFakeBrowser } from './utils/fake-browser';

const NOON = Date.UTC(2026, 8, 2, 12, 0, 0);

describe('createSession', () => {
  let fake: FakeBrowser;

  beforeEach(() => {
    fake = installFakeBrowser();
  });

  afterEach(() => {
    fake.uninstall();
  });

  it('mints a session id the protocol accepts when storage is empty', () => {
    const stamp = createSession().next(NOON);

    expect(stamp.sessionId).toMatch(UUID_V7_RE);
    expect(stamp.seq).toBe(0);
    expect(JSON.parse(fake.sessionStorage.entries.get(SESSION_KEY) as string)).toEqual({
      id: stamp.sessionId,
      lastActivity: NOON,
      seq: 0,
    });
  });

  it('increments seq inside the session', () => {
    const session = createSession();
    const first = session.next(NOON);
    const second = session.next(NOON + 1000);
    const third = session.next(NOON + 2000);

    expect([first.seq, second.seq, third.seq]).toEqual([0, 1, 2]);
    expect(second.sessionId).toBe(first.sessionId);
    expect(third.sessionId).toBe(first.sessionId);
  });

  it('rotates after thirty idle minutes', () => {
    const session = createSession();
    const first = session.next(NOON);

    expect(session.next(NOON + SESSION_IDLE_MS).sessionId).toBe(first.sessionId);
    const rotated = session.next(NOON + SESSION_IDLE_MS + SESSION_IDLE_MS + 1);

    expect(rotated.sessionId).not.toBe(first.sessionId);
    expect(rotated.seq).toBe(0);
  });

  it('rotates across a UTC day boundary even when the visitor never paused', () => {
    const session = createSession();
    const beforeMidnight = session.next(Date.UTC(2026, 8, 2, 23, 59, 30));
    // One minute later, but a different UTC day: the daily visitor hash rotates
    // at midnight too, and a session straddling it would be the one record able
    // to link the two days.
    const afterMidnight = session.next(Date.UTC(2026, 8, 3, 0, 0, 30));

    expect(afterMidnight.sessionId).not.toBe(beforeMidnight.sessionId);
    expect(afterMidnight.seq).toBe(0);
  });

  it('reads a session a previous page left behind', () => {
    const first = createSession().next(NOON);
    const resumed = createSession().next(NOON + 1000);

    expect(resumed.sessionId).toBe(first.sessionId);
    expect(resumed.seq).toBe(1);
  });

  it('falls back to memory when sessionStorage throws', () => {
    fake.sessionStorage.breaks();
    const session = createSession();

    const first = session.next(NOON);
    const second = session.next(NOON + 1000);

    // Private mode degrades analytics; it never breaks the page.
    expect(second.sessionId).toBe(first.sessionId);
    expect(second.seq).toBe(1);
    expect(fake.sessionStorage.entries.size).toBe(0);
  });

  it('ignores a corrupted stored session', () => {
    fake.sessionStorage.entries.set(SESSION_KEY, 'not json');
    expect(createSession().next(NOON).seq).toBe(0);

    fake.sessionStorage.entries.set(SESSION_KEY, JSON.stringify({ id: 7, lastActivity: NOON, seq: 4 }));
    expect(createSession().next(NOON).seq).toBe(0);
  });
});
