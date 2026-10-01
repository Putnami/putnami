// biome-ignore-all lint/suspicious/noConsole: the tracker warns on an undeclared action; these tests intercept it
import { afterEach, beforeEach, describe, expect, it, mock } from 'bun:test';
import { queueKey } from '../src/client/queue';
import { installDataTrack } from '../src/client/react/data-track';
import { useTrack } from '../src/client/react/use-track';
import { flush, installTracker, track, uninstallTracker, utmFromSearch, wireReferrer } from '../src/client/tracker';
import type { ClientBootstrap, WireEvent } from '../src/client/wire';
import { buildBootstrap } from '../src/server/http/bootstrap';
import { UUID_V7_RE } from '../src/server/sanitize/vocabulary';
import { FakeElement, type FakeBrowser, fakeBootstrap, installFakeBrowser } from './utils/fake-browser';

/** Reads the queue the tracker persisted, which is what a batch is built from. */
function queued(fake: FakeBrowser, boot: ClientBootstrap = fakeBootstrap()): WireEvent[] {
  const raw = fake.localStorage.entries.get(queueKey(boot));
  return raw ? (JSON.parse(raw) as WireEvent[]) : [];
}

describe('installTracker', () => {
  let fake: FakeBrowser;

  beforeEach(() => {
    fake = installFakeBrowser({
      pathname: '/docs/routing',
      search: '?utm_source=newsletter&utm_campaign=summer&utm_medium=&q=secret',
      referrer: 'https://news.example.com/story?token=abcd#frag',
      innerWidth: 800,
      language: 'fr-FR',
      beacon: true,
    });
  });

  afterEach(() => {
    uninstallTracker();
    fake.uninstall();
  });

  it('re-sends the server page view under the server event id, enriched', () => {
    const boot = fakeBootstrap();
    installTracker(boot);

    const [event] = queued(fake);
    // The row already exists server-side. Reusing its id makes this an
    // enrichment through ON CONFLICT; a fresh id would double-count the page.
    expect(event?.eventId).toBe(boot.pv);
    expect(event?.name).toBe('page_view');
    expect(event?.page?.route).toBe('/docs/[slug]');
    expect(event?.page?.path).toBe('/docs/routing');
    // The referrer keeps origin + path and loses the query: a search string
    // can carry the identifiers this package exists not to collect.
    expect(event?.page?.referrer).toBe('https://news.example.com/story');
    // Only the five campaign keys are read, and an empty one is not an
    // attribution. `q=secret` is never looked at.
    expect(event?.page?.utm).toEqual({ source: 'newsletter', campaign: 'summer' });
    expect(event?.viewportClass).toBe('md');
    expect(event?.language).toBe('fr-FR');
    expect(event?.sessionId).toMatch(UUID_V7_RE);
    expect(event?.seq).toBe(0);
    expect(event?.clientTs).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/);
  });

  it('accepts the bootstrap the server actually builds', () => {
    const runtime = {
      endpoint: '/_putnami/analytics/events',
      app: 'demo',
      env: 'test',
      version: '1.2.3',
      declared: new Set(['signup_click']),
    } as Parameters<typeof buildBootstrap>[0];
    // Structural pin: the browser copy of the type and the server's producer
    // are the same shape, so a field added on one side breaks the build.
    const server: ClientBootstrap = buildBootstrap(runtime, '0192f0c0-1234-7abc-8def-0123456789ab', '/docs/[slug]');

    installTracker(server);

    expect(queued(fake)[0]?.eventId).toBe(server.pv);
    expect(server.declared).toEqual(['signup_click']);
  });

  it('installs once, so a page carrying the script twice is not counted twice', () => {
    installTracker(fakeBootstrap());
    installTracker(fakeBootstrap({ pv: '0192f0c0-9999-7abc-8def-0123456789ab' }));

    expect(queued(fake)).toHaveLength(1);
  });

  it('never flushes another path-mounted application queue to its endpoint', async () => {
    const docs = fakeBootstrap({
      app: 'docs',
      endpoint: '/docs/_putnami/analytics/events',
      pv: '0192f0c0-1000-7abc-8def-0123456789ab',
    });
    const shop = fakeBootstrap({
      app: 'shop',
      endpoint: '/shop/_putnami/analytics/events',
      pv: '0192f0c0-2000-7abc-8def-0123456789ab',
    });

    installTracker(docs);
    uninstallTracker();
    installTracker(shop);
    await flush({ unload: false });

    expect(fake.fetchCalls.map((call) => call.url)).toEqual([shop.endpoint]);
    expect(queued(fake, docs).map((event) => event.eventId)).toEqual([docs.pv]);
    expect(queued(fake, shop)).toHaveLength(0);
  });

  it('drops a route sentinel rather than losing the whole page view', () => {
    // `__unmatched__` fails the protocol's route regex; sending it would drop
    // the event instead of the route alone.
    installTracker(fakeBootstrap({ route: '__unmatched__' }));

    const [event] = queued(fake);
    expect(event?.page?.route).toBeUndefined();
    expect(event?.page?.path).toBe('/docs/routing');
  });
});

describe('client navigation', () => {
  let fake: FakeBrowser;

  beforeEach(() => {
    fake = installFakeBrowser({ pathname: '/', innerWidth: 1400, beacon: true });
    installTracker(fakeBootstrap());
  });

  afterEach(() => {
    uninstallTracker();
    fake.uninstall();
  });

  it('closes the previous view with engagement under its own id and opens a new one', () => {
    const boot = fakeBootstrap();
    fake.clock = 2500;

    // onNavigation never fires for the initial load — the bootstrap is the
    // initial view — so the first event here is the second page of the visit.
    fake.navigate({ pathname: '/pricing', search: '', hash: '', route: '/pricing', previous: '/' });

    const events = queued(fake);
    expect(events).toHaveLength(2);

    const [closed, opened] = events;
    // Same id as the initial view: the sink takes GREATEST on engagement_ms
    // and moves no counter, so this is an update rather than a second page.
    expect(closed?.eventId).toBe(boot.pv);
    expect(closed?.engagementMs).toBe(2500);

    expect(opened?.eventId).toMatch(UUID_V7_RE);
    expect(opened?.eventId).not.toBe(boot.pv);
    expect(opened?.page?.path).toBe('/pricing');
    expect(opened?.page?.route).toBe('/pricing');
    expect(opened?.page?.referrer).toBe('/');
    expect(opened?.engagementMs).toBeUndefined();
  });

  it('keeps the enrichment of a view across its engagement re-send', () => {
    uninstallTracker();
    fake.uninstall();
    fake = installFakeBrowser({
      pathname: '/landing',
      search: '?utm_source=ads',
      referrer: 'https://partner.example.com/',
      beacon: true,
    });
    installTracker(fakeBootstrap());

    fake.navigate({ pathname: '/pricing', search: '', hash: '', route: '/pricing', previous: '/landing' });

    // The queue replaces by eventId. A re-send that dropped the referrer would
    // overwrite the queued initial view and the attribution would never ship.
    const [closed] = queued(fake);
    expect(closed?.page?.referrer).toBe('https://partner.example.com/');
    expect(closed?.page?.utm).toEqual({ source: 'ads' });
  });

  it('restarts the engagement clock on the new view', () => {
    fake.clock = 4000;
    fake.navigate({ pathname: '/a', search: '', hash: '', route: '/a', previous: '/' });
    fake.clock = 6000;
    fake.navigate({ pathname: '/b', search: '', hash: '', route: '/b', previous: '/a' });

    const events = queued(fake);
    expect(events).toHaveLength(3);
    expect(events[1]?.engagementMs).toBe(2000);
  });

  it('ignores an event carrying no detail', () => {
    fake.window.dispatchEvent({ type: 'putnami:navigation' });

    expect(queued(fake)).toHaveLength(1);
  });
});

describe('track', () => {
  let fake: FakeBrowser;
  let warn: ReturnType<typeof mock>;
  let originalWarn: typeof console.warn;

  beforeEach(() => {
    fake = installFakeBrowser({ beacon: true });
    originalWarn = console.warn;
    warn = mock(() => undefined);
    console.warn = warn as unknown as typeof console.warn;
    installTracker(fakeBootstrap());
  });

  afterEach(() => {
    console.warn = originalWarn;
    uninstallTracker();
    fake.uninstall();
  });

  it('queues a declared action with its properties', () => {
    track('signup_click', { plan: 'pro', seats: 3, trial: true });

    const action = queued(fake)[1];
    expect(action?.name).toBe('action');
    expect(action?.action).toBe('signup_click');
    expect(action?.props).toEqual({ plan: 'pro', seats: 3, trial: true });
    expect(action?.eventId).toMatch(UUID_V7_RE);
    expect(action?.seq).toBe(1);
  });

  it('warns and queues nothing for an undeclared name', () => {
    track('exfiltrate', { plan: 'pro' });

    // The server drops it as unknown_action without telling anyone, so the
    // warning is the only place a developer learns about the typo.
    expect(warn).toHaveBeenCalledTimes(1);
    expect(queued(fake)).toHaveLength(1);
  });

  it('drops properties the protocol would reject instead of the event', () => {
    track('search', { 'Bad-Key': 'x', query_length: Number.NaN, ok: 'yes' });

    expect(queued(fake)[1]?.props).toEqual({ ok: 'yes' });
  });

  it('bounds a long property value in bytes', () => {
    track('search', { note: 'я'.repeat(200) });

    expect(queued(fake)[1]?.props).toEqual({ note: 'я'.repeat(128) });
  });

  it('is a no-op once the tracker is uninstalled', () => {
    uninstallTracker();
    track('signup_click');

    expect(queued(fake)).toHaveLength(1);
  });

  it('exposes track on window for inline and non-module callers', () => {
    const exposed = (fake.window as unknown as { __putnamiAnalytics?: { track: typeof track } }).__putnamiAnalytics;
    exposed?.track('signup_click', { plan: 'pro' });

    expect(queued(fake)[1]?.action).toBe('signup_click');
  });
});

describe('data-track', () => {
  let fake: FakeBrowser;

  beforeEach(() => {
    fake = installFakeBrowser({ beacon: true });
    installTracker(fakeBootstrap());
  });

  afterEach(() => {
    uninstallTracker();
    fake.uninstall();
  });

  it('turns a click on a marked element into a declared action', () => {
    fake.click(
      new FakeElement({
        'data-track': 'signup_click',
        'data-track-plan': 'pro',
        'data-track-seat-count': '3',
        class: 'btn',
      }),
    );

    const action = queued(fake)[1];
    expect(action?.action).toBe('signup_click');
    // Dashes become underscores so the key matches the protocol's key regex.
    expect(action?.props).toEqual({ plan: 'pro', seat_count: '3' });
  });

  it('ignores a click outside any marked element', () => {
    fake.click(new FakeElement({ class: 'btn' }));

    expect(queued(fake)).toHaveLength(1);
  });

  it('ignores a marked element with an empty name', () => {
    fake.click(new FakeElement({ 'data-track': '' }));

    expect(queued(fake)).toHaveLength(1);
  });

  it('skips a prop key the protocol would reject', () => {
    fake.click(new FakeElement({ 'data-track': 'search', 'data-track-9bad': 'x', 'data-track-': 'y' }));

    expect(queued(fake)[1]?.props).toBeUndefined();
  });

  it('stops listening once removed', () => {
    const dispose = installDataTrack(() => {
      throw new Error('should not be called');
    });
    dispose();

    fake.click(new FakeElement({ 'data-track': 'signup_click' }));

    expect(queued(fake)).toHaveLength(2);
  });
});

describe('the flush loop', () => {
  let fake: FakeBrowser;

  beforeEach(() => {
    fake = installFakeBrowser({ beacon: true });
    installTracker(fakeBootstrap());
  });

  afterEach(() => {
    uninstallTracker();
    fake.uninstall();
  });

  it('acknowledges a 202 and empties the queue', async () => {
    await flush({ unload: false });

    expect(fake.fetchCalls).toHaveLength(1);
    expect(queued(fake)).toHaveLength(0);
  });

  it('drops a batch the server will never accept', async () => {
    fake.responses.push(400);

    await flush({ unload: false });

    // Retrying a rejection forever would only overflow the queue.
    expect(queued(fake)).toHaveLength(0);
  });

  it('keeps the batch and backs off after a 503', async () => {
    fake.responses.push(503);

    await flush({ unload: false });

    expect(queued(fake)).toHaveLength(1);
    const ids = queued(fake).map((event) => event.eventId);

    // The backoff is in the future, so the next scheduled flush sends nothing
    // and the ids do not change — that is what makes a later retry idempotent.
    await flush({ unload: false });

    expect(fake.fetchCalls).toHaveLength(1);
    expect(queued(fake).map((event) => event.eventId)).toEqual(ids);
  });

  it('ignores the backoff on unload, because it is the last chance', async () => {
    fake.responses.push(503);
    await flush({ unload: false });

    await flush({ unload: true });

    expect(fake.beaconCalls).toHaveLength(1);
  });

  it('keeps a beaconed batch until a response-confirmed retry', async () => {
    await flush({ unload: true });

    expect(fake.beaconCalls).toHaveLength(1);
    expect(queued(fake)).toHaveLength(1);

    await flush({ unload: false });

    expect(fake.fetchCalls).toHaveLength(1);
    expect(queued(fake)).toHaveLength(0);
  });

  it('sends no request when the queue is empty', async () => {
    await flush({ unload: false });
    await flush({ unload: false });

    expect(fake.fetchCalls).toHaveLength(1);
  });

  it('never sends more than the wire ceiling of fifty events', async () => {
    for (let index = 0; index < 60; index++) {
      track('search', { n: index });
    }

    await flush({ unload: false });

    const sizes = fake.fetchCalls.map((call) => (JSON.parse(call.init.body as string).events as unknown[]).length);
    // 50 is the protocol maximum; a 51-event batch is rejected whole.
    expect(Math.max(...sizes)).toBe(50);
    expect(sizes.every((size) => size <= 50)).toBe(true);
  });

  it('flushes early once twenty events are queued', () => {
    for (let index = 0; index < 19; index++) {
      track('search', { n: index });
    }

    expect(fake.fetchCalls.length).toBeGreaterThan(0);
  });

  it('is a no-op when no tracker is installed', async () => {
    uninstallTracker();

    await flush({ unload: false });

    expect(fake.fetchCalls).toHaveLength(0);
  });
});

describe('page lifecycle', () => {
  let fake: FakeBrowser;

  beforeEach(() => {
    fake = installFakeBrowser({ beacon: true });
    installTracker(fakeBootstrap());
  });

  afterEach(() => {
    uninstallTracker();
    fake.uninstall();
  });

  it('beacons the queue with a final engagement on pagehide', async () => {
    const boot = fakeBootstrap();
    fake.clock = 7000;

    fake.window.dispatchEvent({ type: 'pagehide' });
    await Promise.resolve();

    expect(fake.beaconCalls).toHaveLength(1);
    const body = JSON.parse(await (fake.beaconCalls[0] as { blob: Blob }).blob.text());
    expect(body.events).toHaveLength(1);
    expect(body.events[0].eventId).toBe(boot.pv);
    expect(body.events[0].engagementMs).toBe(7000);
    expect(queued(fake)).toHaveLength(1);
  });

  it('stops the engagement clock while the tab is hidden and restarts it on return', () => {
    fake.clock = 1000;
    fake.visibility('hidden');
    // Ten minutes in a background tab.
    fake.clock = 600_000;
    fake.visibility('visible');
    fake.clock = 601_500;

    fake.window.dispatchEvent({ type: 'pagehide' });

    expect(queued(fake)[0]?.engagementMs).toBe(2500);
  });

  it('removes every listener on uninstall', () => {
    expect(fake.window.count()).toBeGreaterThan(0);
    expect(fake.document.count()).toBeGreaterThan(0);

    uninstallTracker();

    expect(fake.window.count()).toBe(0);
    expect(fake.document.count()).toBe(0);
    expect((fake.window as unknown as { __putnamiAnalytics?: unknown }).__putnamiAnalytics).toBeUndefined();
  });

  it('uninstalls idempotently', () => {
    uninstallTracker();

    expect(() => uninstallTracker()).not.toThrow();
  });
});

describe('useTrack', () => {
  it('is a no-op on the server, where there is no window', () => {
    const globals = globalThis as Record<string, unknown>;
    const saved = globals['window'];
    globals['window'] = undefined;

    // Server-safe by construction: a component may call it unconditionally.
    expect(() => useTrack()('signup_click')).not.toThrow();

    globals['window'] = saved;
  });

  it('records through the installed tracker in the browser', () => {
    const fake = installFakeBrowser({ beacon: true });
    installTracker(fakeBootstrap());

    useTrack()('signup_click', { plan: 'pro' });

    expect(queued(fake)[1]?.action).toBe('signup_click');

    uninstallTracker();
    fake.uninstall();
  });

  it('does nothing on a static page, where no tracker was installed', () => {
    const fake = installFakeBrowser();

    expect(() => useTrack()('signup_click')).not.toThrow();

    fake.uninstall();
  });
});

describe('wire normalization', () => {
  it.each([
    ['https://example.com/a?b=c#d', 'https://example.com/a'],
    ['/internal/page', '/internal/page'],
    ['//evil.example.com/x', undefined],
    ['/\\evil.example.com/x', undefined],
    ['javascript:alert(1)', undefined],
    ['', undefined],
    [undefined, undefined],
  ])('normalizes the referrer %p to %p', (input, expected) => {
    expect(wireReferrer(input as string | undefined)).toBe(expected as string | undefined);
  });

  it('drops a referrer beyond the byte bound', () => {
    expect(wireReferrer(`https://example.com/${'я'.repeat(300)}`)).toBeUndefined();
  });

  it('reads only the five campaign parameters', () => {
    expect(utmFromSearch('?utm_source=a&utm_medium=b&utm_campaign=c&utm_content=d&utm_term=e&email=x')).toEqual({
      source: 'a',
      medium: 'b',
      campaign: 'c',
      content: 'd',
      term: 'e',
    });
    expect(utmFromSearch('')).toEqual({});
    expect(utmFromSearch('?utm_source=%20%20')).toEqual({});
  });
});
