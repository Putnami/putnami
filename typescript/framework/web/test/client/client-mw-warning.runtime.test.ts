import { afterAll, beforeAll, describe, expect, it, spyOn } from 'bun:test';
import { __resetClientMiddlewareWarning, page } from '../../src/client/page';

// The "warn once" flag in client/page is module-scoped and latches on the first
// server-only method call, so it warns at most once for the lifetime of the module.
// That latch is process-global and shared across test files in the same `bun test`
// run, so we reset it here to assert the "exactly once" behavior independently of
// which other file tripped it earlier (test-file order is not guaranteed).
describe('client middleware server-only warning', () => {
  let warnSpy: ReturnType<typeof spyOn<Console, 'warn'>>;

  beforeAll(() => {
    __resetClientMiddlewareWarning();
    warnSpy = spyOn(console, 'warn').mockImplementation(() => {});
  });

  afterAll(() => {
    warnSpy.mockRestore();
  });

  it('warns exactly once across many no-op middleware calls and includes the reason', () => {
    page()
      .cors({ origin: 'https://example.com' })
      .rateLimit({ max: 100 })
      .cache({ maxAge: 60 })
      .use(() => undefined)
      .render(() => null);

    // Subsequent builders must not re-warn.
    page()
      .rateLimit()
      .render(() => null);

    expect(warnSpy).toHaveBeenCalledTimes(1);
    expect(String(warnSpy.mock.calls[0]?.[0])).toContain('server-only');
  });
});
