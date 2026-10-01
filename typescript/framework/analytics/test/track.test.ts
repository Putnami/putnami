import { afterEach, describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { flushAnalytics, setAnalyticsRuntime } from '../src/server/runtime';
import { track } from '../src/server/track';
import { fakeContext } from './utils/context';
import { createFakeSink } from './utils/fake-sink';
import { testRuntime } from './utils/runtime';

const FEATURE = 'typescript/web-analytics-collection';
const UNDECLARED = 'undeclared-actions-are-dropped';
const EVENTS = { signup_click: { plan: String, seats: Number, trial: Boolean } };
const CHROME_UA =
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36';

function install(sinkFailure?: Error) {
  const sink = createFakeSink(sinkFailure);
  const rt = testRuntime({ sink: sink.sink, events: EVENTS });
  setAnalyticsRuntime(rt);
  return sink;
}

afterEach(() => {
  setAnalyticsRuntime(undefined);
});

describe('track', () => {
  it('writes one server action row', async () => {
    const sink = install();
    const ctx = fakeContext({
      method: 'POST',
      url: 'http://localhost/signup',
      headers: { 'user-agent': CHROME_UA },
      route: '/signup',
      user: { sub: 'user-42' },
    });

    await track(ctx, 'signup_click', { plan: 'pro', seats: 3, trial: false });
    await flushAnalytics();

    expect(sink.rows).toHaveLength(1);
    const row = sink.rows[0]!;
    expect(row.name).toBe('action');
    expect(row.source).toBe('server');
    expect(row.actionName).toBe('signup_click');
    expect(row.props).toEqual({ plan: 'pro', seats: 3, trial: false });
    expect(row.route).toBe('__none__');
    expect(row.path).toBeNull();
    expect(row.sessionId).toBeNull();
    expect(row.userId).toBe('user-42');
    expect(row.visitorKind).toBe('daily');
    expect(row.visitorId).toHaveLength(22);
  });

  specTest(
    'throws synchronously on an undeclared name',
    { feature: FEATURE, requirement: UNDECLARED, check: 'server-track-throws-on-an-undeclared-name' },
    () => {
      const sink = install();
      const ctx = fakeContext({ headers: { 'user-agent': CHROME_UA } });

      // Synchronously, before any await: an undeclared name is a developer
      // mistake, and a rejected promise nobody awaited would swallow it.
      expect(() => track(ctx, 'never_declared', { plan: 'pro' })).toThrow(
        'analytics: action "never_declared" is not declared (declareEvents)',
      );
      expect(sink.batches).toHaveLength(0);
    },
  );

  it('keeps only declared properties of the declared type', async () => {
    const sink = install();
    const ctx = fakeContext({ headers: { 'user-agent': CHROME_UA } });

    await track(ctx, 'signup_click', {
      plan: 'pro',
      seats: 'three' as unknown as number,
      email: 'someone@example.com' as unknown as string,
    });
    await flushAnalytics();

    expect(sink.rows[0]?.props).toEqual({ plan: 'pro' });
  });

  it('resolves as soon as the row is queued, whatever the database does', async () => {
    const sink = install(new Error('connection refused'));
    const ctx = fakeContext({ headers: { 'user-agent': CHROME_UA } });

    // `track()` is called from application code on a response path, so it
    // resolves on acceptance into the queue. A failure there is the sink's to
    // count: turning an endpoint into a 500 because a measurement could not be
    // stored is exactly the coupling this slice removes.
    await expect(track(ctx, 'signup_click')).resolves.toBeUndefined();
    expect(sink.batches).toHaveLength(0);

    await flushAnalytics();

    expect(sink.batches).toHaveLength(1);
    expect(sink.rows).toHaveLength(0);
  });

  it('records nothing and does not throw when analytics is absent or switched off', async () => {
    // `analytics.enabled: false` is a documented operator switch, and composing
    // without the plugin is legitimate in a test or a stripped build. Throwing
    // here would turn every endpoint that measures into a 500 the moment
    // someone flips a boolean the documentation calls an off switch.
    setAnalyticsRuntime(undefined);
    const ctx = fakeContext({ headers: { 'user-agent': CHROME_UA } });

    await expect(track(ctx, 'signup_click')).resolves.toBeUndefined();
    // An undeclared name is equally silent: with no runtime there is no
    // declaration set to check it against.
    await expect(track(ctx, 'never_declared')).resolves.toBeUndefined();
  });
});
