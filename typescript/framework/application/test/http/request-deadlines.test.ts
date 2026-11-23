import { describe, expect, it } from 'bun:test';
import { LinkedAbortController, type RequestDeadline, RequestDeadlines } from '../../src/http/request-deadlines';

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

/** Queues an entry that calls `expire` at its deadline. */
function add(deadlines: RequestDeadlines, expire: () => void): RequestDeadline {
  const entry: RequestDeadline = { at: 0, expire, prev: undefined, next: undefined, queued: false };
  deadlines.enqueue(entry);
  return entry;
}

describe('RequestDeadlines', () => {
  it('expires the requests still queued at their deadline, in arrival order', async () => {
    // a and b are due at 200 ms, c at 300 ms; the checks sit 50 ms from each.
    const deadlines = new RequestDeadlines(200);
    const expired: string[] = [];
    add(deadlines, () => expired.push('a'));
    const b = add(deadlines, () => expired.push('b'));
    await sleep(100);
    add(deadlines, () => expired.push('c'));
    deadlines.remove(b);

    await sleep(150);
    expect(expired).toEqual(['a']);
    await sleep(150);
    expect(expired).toEqual(['a', 'c']);
    expect(deadlines.size).toBe(0);
  });

  it('expires nothing that answered first, and removing twice does nothing', async () => {
    const deadlines = new RequestDeadlines(10);
    let expired = 0;
    const entries = Array.from({ length: 100 }, () => add(deadlines, () => expired++));
    for (const entry of entries) deadlines.remove(entry);
    deadlines.remove(entries[0]);
    expect(deadlines.size).toBe(0);
    await sleep(60);
    expect(expired).toBe(0);
  });

  it('arms again for a request queued after the previous ones expired', async () => {
    const deadlines = new RequestDeadlines(20);
    const expired: number[] = [];
    add(deadlines, () => expired.push(1));
    await sleep(80);
    add(deadlines, () => expired.push(2));
    expect(deadlines.size).toBe(1);
    await sleep(80);
    expect(expired).toEqual([1, 2]);
  });

  it('keeps the requests behind a removed head on their own deadline', async () => {
    // The head is due at 200 ms, the tail at 300 ms.
    const deadlines = new RequestDeadlines(200);
    const expired: string[] = [];
    const head = add(deadlines, () => expired.push('head'));
    await sleep(100);
    add(deadlines, () => expired.push('tail'));
    deadlines.remove(head);
    await sleep(150);
    // The head's deadline passed: the timer armed for it fired and found it gone.
    expect(expired).toEqual([]);
    await sleep(150);
    expect(expired).toEqual(['tail']);
  });

  it('keeps the requests after a deadline whose expiry threw on a timer', async () => {
    const deadlines = new RequestDeadlines(20);
    const expired: string[] = [];
    add(deadlines, () => {
      throw new Error('expiry failed');
    });
    await new Promise((resolve) => setTimeout(resolve, 40));
    add(deadlines, () => expired.push('next'));
    await new Promise((resolve) => setTimeout(resolve, 80));
    expect(expired).toEqual(['next']);
  });
});

describe('RequestDeadlines.enqueue', () => {
  it('expires an entry the caller owns, as a method of that entry', async () => {
    const deadlines = new RequestDeadlines(20);
    const expired: string[] = [];
    const entry = {
      at: 0,
      prev: undefined,
      next: undefined,
      queued: false,
      name: 'owned',
      expire(this: { name: string }) {
        expired.push(this.name);
      },
    };
    deadlines.enqueue(entry);
    expect(deadlines.size).toBe(1);
    await sleep(60);
    expect(expired).toEqual(['owned']);
    expect(deadlines.size).toBe(0);
  });
});

describe('LinkedAbortController', () => {
  it('aborts with the reason of its source', () => {
    const source = new AbortController();
    const linked = new LinkedAbortController(source.signal);
    linked.follow();
    expect(linked.signal.aborted).toBe(false);
    const reason = new DOMException('The client left', 'AbortError');
    source.abort(reason);
    expect(linked.signal.aborted).toBe(true);
    expect(linked.signal.reason).toBe(reason);
  });

  it('aborts at once when its source already aborted', () => {
    const source = new AbortController();
    source.abort('gone');
    const linked = new LinkedAbortController(source.signal);
    linked.follow();
    expect(linked.signal.reason).toBe('gone');
  });

  it('keeps its own reason when it aborts first', () => {
    const source = new AbortController();
    const linked = new LinkedAbortController(source.signal);
    linked.follow();
    const timeout = new DOMException('Request Timeout', 'TimeoutError');
    linked.abort(timeout);
    source.abort('later');
    expect(linked.signal.reason).toBe(timeout);
  });
});
