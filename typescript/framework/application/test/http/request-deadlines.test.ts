import { describe, expect, it } from 'bun:test';
import { RequestDeadlines } from '../../src/http/request-deadlines';

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

describe('RequestDeadlines', () => {
  it('expires the requests still queued at their deadline, in arrival order', async () => {
    // a and b are due at 200 ms, c at 300 ms; the checks sit 50 ms from each.
    const deadlines = new RequestDeadlines(200);
    const expired: string[] = [];
    deadlines.add(() => expired.push('a'));
    const b = deadlines.add(() => expired.push('b'));
    await sleep(100);
    deadlines.add(() => expired.push('c'));
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
    const entries = Array.from({ length: 100 }, () => deadlines.add(() => expired++));
    for (const entry of entries) deadlines.remove(entry);
    deadlines.remove(entries[0]);
    expect(deadlines.size).toBe(0);
    await sleep(60);
    expect(expired).toBe(0);
  });

  it('arms again for a request queued after the previous ones expired', async () => {
    const deadlines = new RequestDeadlines(20);
    const expired: number[] = [];
    deadlines.add(() => expired.push(1));
    await sleep(80);
    deadlines.add(() => expired.push(2));
    expect(deadlines.size).toBe(1);
    await sleep(80);
    expect(expired).toEqual([1, 2]);
  });

  it('keeps the requests behind a removed head on their own deadline', async () => {
    // The head is due at 200 ms, the tail at 300 ms.
    const deadlines = new RequestDeadlines(200);
    const expired: string[] = [];
    const head = deadlines.add(() => expired.push('head'));
    await sleep(100);
    deadlines.add(() => expired.push('tail'));
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
    deadlines.add(() => {
      throw new Error('expiry failed');
    });
    await new Promise((resolve) => setTimeout(resolve, 40));
    deadlines.add(() => expired.push('next'));
    await new Promise((resolve) => setTimeout(resolve, 80));
    expect(expired).toEqual(['next']);
  });
});
