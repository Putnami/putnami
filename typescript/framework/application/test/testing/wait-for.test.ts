import { describe, expect, it } from 'bun:test';
import { waitFor } from '../../src/testing/wait-for';

describe('waitFor', () => {
  it('returns value immediately when fn resolves on first call', async () => {
    const result = await waitFor(async () => 42);
    expect(result).toBe(42);
  });

  it('polls until value becomes defined', async () => {
    let calls = 0;
    const result = await waitFor(
      async () => {
        calls++;
        return calls >= 3 ? 'found' : undefined;
      },
      2000,
      10,
    );
    expect(result).toBe('found');
    expect(calls).toBe(3);
  });

  it('returns undefined after timeout', async () => {
    const result = await waitFor(async () => undefined, 100, 10);
    expect(result).toBeUndefined();
  });

  it('respects custom timeoutMs', async () => {
    const start = Date.now();
    await waitFor(async () => undefined, 200, 20);
    const elapsed = Date.now() - start;
    expect(elapsed).toBeGreaterThanOrEqual(200);
    // The ceiling only tells the custom timeout from the 4000ms default: a
    // loaded machine delays every timer, so a tight ceiling fails at random.
    expect(elapsed).toBeLessThan(4000);
  });

  it('respects custom intervalMs', async () => {
    // Count-in-a-window assertions fail on a loaded machine, where every timer
    // runs late. The gap between two polls has a floor, not a ceiling.
    const callTimes: number[] = [];
    const result = await waitFor(
      async () => {
        callTimes.push(performance.now());
        return callTimes.length >= 3 ? 'found' : undefined;
      },
      10_000,
      250,
    );
    expect(result).toBe('found');
    expect(callTimes).toHaveLength(3);
    for (let i = 1; i < callTimes.length; i++) {
      // Above the 100ms default, so an ignored intervalMs fails. Timers may
      // fire up to 1ms early.
      expect(callTimes[i] - callTimes[i - 1]).toBeGreaterThanOrEqual(249);
    }
  });

  it('returns objects and arrays', async () => {
    const obj = { key: 'value' };
    const result = await waitFor(async () => obj);
    expect(result).toEqual({ key: 'value' });
  });
});
