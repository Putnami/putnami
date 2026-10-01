import { describe, expect, it } from 'bun:test';
import { runIslands, scheduleHydration } from '../../src/client/island/hydrate';

describe('scheduleHydration', () => {
  it('runs immediately for the load strategy', () => {
    let ran = false;
    scheduleHydration({} as Element, 'load', undefined, () => {
      ran = true;
    });
    expect(ran).toBe(true);
  });

  it('runs for the idle strategy (setTimeout fallback)', async () => {
    const ran = await new Promise<boolean>((resolve) => {
      scheduleHydration({} as Element, 'idle', undefined, () => resolve(true));
    });
    expect(ran).toBe(true);
  });

  it('falls back to immediate when IntersectionObserver is unavailable', () => {
    // bun's test runtime has no IntersectionObserver, so visible hydrates eagerly.
    let ran = false;
    scheduleHydration({} as Element, 'visible', undefined, () => {
      ran = true;
    });
    expect(ran).toBe(true);
  });

  it('falls back to immediate when matchMedia is unavailable', () => {
    let ran = false;
    scheduleHydration({} as Element, 'media', '(min-width: 600px)', () => {
      ran = true;
    });
    expect(ran).toBe(true);
  });

  it('returns a teardown function', () => {
    const teardown = scheduleHydration({} as Element, 'load', undefined, () => {});
    expect(typeof teardown).toBe('function');
    expect(() => teardown()).not.toThrow();
  });
});

describe('runIslands', () => {
  it('is a no-op without a document', async () => {
    // No `document` in the bun test runtime.
    await expect(runIslands({})).resolves.toBeUndefined();
  });
});
