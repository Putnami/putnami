import { afterEach, describe, expect, it } from 'bun:test';
import { HttpResponse } from '@putnami/application';
import { resetConfigLoader } from '@putnami/runtime';
import { buildBootstrap } from '../src/server/http/bootstrap';
import { isRenderedPage } from '../src/server/http/page-view.middleware';
import { countryOf, primaryLanguage, userIdOf } from '../src/server/http/rows';
import { ANALYTICS_VISITOR_SLOT, attachPendingCookies, resolveVisitor } from '../src/server/http/visitor';
import { forget, resetConsentWarning } from '../src/server/identity/identified-cookie';
import { analyticsRuntime, NOT_INSTALLED_MESSAGE, resolveAppName, setAnalyticsRuntime } from '../src/server/runtime';
import { fakeContext } from './utils/context';
import { createFakeSink } from './utils/fake-sink';
import { testRuntime } from './utils/runtime';

const CHROME_UA =
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36';

afterEach(() => {
  setAnalyticsRuntime(undefined);
  resetConsentWarning();
  process.env['CONFIG_DATA'] = undefined;
  resetConfigLoader();
});

describe('the runtime slot', () => {
  it('throws until a plugin publishes one', () => {
    expect(() => analyticsRuntime()).toThrow(NOT_INSTALLED_MESSAGE);
  });

  it('answers with what the plugin published', () => {
    const rt = testRuntime({ sink: createFakeSink().sink });
    setAnalyticsRuntime(rt);

    expect(analyticsRuntime()).toBe(rt);
  });

  it('keys the rate limiter with the same resolver the visitor hash uses', () => {
    const rt = testRuntime({ sink: createFakeSink().sink });
    const ctx = fakeContext({ headers: { 'x-forwarded-for': '203.0.113.7' } });

    // No trusted proxy is configured, so the forwarded header is ignored and
    // both consumers see the same non-spoofable key.
    expect(rt.keyGenerator({ req: ctx.req, headers: ctx.headers })).toBe(rt.clientIp(ctx));
  });
});

describe('resolveAppName', () => {
  it('always names the application', () => {
    expect(resolveAppName().length).toBeGreaterThan(0);
  });
});

describe('buildBootstrap', () => {
  it('carries the seven public members and sorts the declared names', () => {
    const rt = testRuntime({ sink: createFakeSink().sink, events: { zeta: {}, alpha: {} } });

    const bootstrap = buildBootstrap(rt, '01920000-0000-7000-8000-000000000001', '/docs/[...page]');

    expect(bootstrap.declared).toEqual(['alpha', 'zeta']);
    expect(bootstrap.route).toBe('/docs/[...page]');
    expect(bootstrap.endpoint).toBe('/_putnami/analytics/events');
    expect(bootstrap.version).toBe(rt.version);
    expect(Object.keys(bootstrap)).toHaveLength(7);
  });
});

describe('resolveVisitor', () => {
  it('derives the identity once per request', async () => {
    const rt = testRuntime({ sink: createFakeSink().sink });
    const ctx = fakeContext({ headers: { 'user-agent': CHROME_UA } });

    const first = await resolveVisitor(ctx, rt, CHROME_UA);
    const second = await resolveVisitor(ctx, rt, CHROME_UA);

    expect(second).toBe(first);
    expect((ctx as unknown as Record<string, unknown>)[ANALYTICS_VISITOR_SLOT]).toBe(first);
  });

  it('queues the minted cookie instead of returning it', async () => {
    const rt = testRuntime({ sink: createFakeSink().sink, config: { mode: 'identified' }, consent: () => true });
    const ctx = fakeContext({ headers: { 'user-agent': CHROME_UA } });

    const identity = await resolveVisitor(ctx, rt, CHROME_UA);
    const response = attachPendingCookies(ctx, new HttpResponse(undefined, { status: 202 }));

    expect(identity.visitorKind).toBe('cookie');
    expect(response.getHeaders('Set-Cookie')).toHaveLength(1);
    // Drained: a second layer attaching the same response adds nothing.
    expect(attachPendingCookies(ctx, response).getHeaders('Set-Cookie')).toHaveLength(1);
  });

  it('carries a forget() erasure onto the response', () => {
    process.env['CONFIG_DATA'] = JSON.stringify({ analytics: { cookieName: '_pa' } });
    resetConfigLoader();
    const ctx = fakeContext();

    forget(ctx);
    const response = attachPendingCookies(ctx, new HttpResponse(undefined, { status: 200 }));

    expect(response.getHeader('Set-Cookie')).toContain('Max-Age=0');
  });
});

describe('the row helpers', () => {
  it('reads only a valid primary language tag', () => {
    expect(primaryLanguage(fakeContext({ headers: { 'accept-language': 'fr-FR,fr;q=0.9' } }))).toBe('fr-FR');
    expect(primaryLanguage(fakeContext({ headers: { 'accept-language': '*' } }))).toBeNull();
    expect(primaryLanguage(fakeContext())).toBeNull();
  });

  it('reads the country only from the configured header, and only when valid', () => {
    const off = testRuntime({ sink: createFakeSink().sink });
    const on = testRuntime({ sink: createFakeSink().sink, config: { countryHeader: 'cf-ipcountry' } });

    expect(countryOf(fakeContext({ headers: { 'cf-ipcountry': 'fr' } }), off)).toBeNull();
    expect(countryOf(fakeContext({ headers: { 'cf-ipcountry': 'fr' } }), on)).toBe('FR');
    expect(countryOf(fakeContext({ headers: { 'cf-ipcountry': '12' } }), on)).toBeNull();
    expect(countryOf(fakeContext(), on)).toBeNull();
  });

  it('reads the authenticated subject, or null', () => {
    expect(userIdOf(fakeContext({ user: { sub: 'user-1' } }))).toBe('user-1');
    expect(userIdOf(fakeContext({ user: {} }))).toBeNull();
    expect(userIdOf(fakeContext())).toBeNull();
  });
});

describe('isRenderedPage', () => {
  it('accepts an HTML success whatever the header casing', () => {
    expect(isRenderedPage(new HttpResponse('x', { headers: { 'content-type': 'text/html;charset=utf-8' } }))).toBe(
      true,
    );
    expect(isRenderedPage(new HttpResponse('x', { headers: { 'Content-Type': 'TEXT/HTML' } }))).toBe(true);
  });

  it('rejects anything that is not a rendered page', () => {
    expect(isRenderedPage(new HttpResponse('x', { headers: { 'content-type': 'application/json' } }))).toBe(false);
    expect(isRenderedPage(new HttpResponse('x'))).toBe(false);
    expect(isRenderedPage(new HttpResponse('x', { status: 500, headers: { 'content-type': 'text/html' } }))).toBe(
      false,
    );
  });
});
