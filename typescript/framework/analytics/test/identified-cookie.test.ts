import { afterEach, describe, expect, it } from 'bun:test';
import type { HttpRequestContext } from '@putnami/application';
import { resetConfigLoader, useConfig } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { AnalyticsConfig, type AnalyticsConfigValues } from '../src/server/analytics.config';
import {
  type AnalyticsRuntime,
  forget,
  mintId,
  optOutSignal,
  parseCookie,
  resetConsentWarning,
  resolveIdentifiedVisitor,
  serializeCookie,
  sign,
  takePendingCookies,
} from '../src/server/identity/identified-cookie';

const FEATURE = 'typescript/web-analytics-collection';
const CONSENT = 'identified-mode-needs-consent';
const OPT_OUT = 'opt-out-signals-force-cookieless';
const SECRET = 'a-server-side-key-of-at-least-32-chars';

function analyticsConfig(overrides: Record<string, unknown> = {}): AnalyticsConfigValues {
  process.env.CONFIG_DATA = JSON.stringify({ analytics: { mode: 'identified', ...overrides } });
  resetConfigLoader();
  return useConfig(AnalyticsConfig);
}

function context(headers: Record<string, string> = {}, secure = false): HttpRequestContext {
  return { headers: new Headers(headers), secured: () => secure } as unknown as HttpRequestContext;
}

function runtime(
  config: AnalyticsConfigValues,
  consent?: (ctx: HttpRequestContext) => boolean | Promise<boolean>,
): AnalyticsRuntime {
  return { config, secret: SECRET, consent };
}

afterEach(() => {
  delete process.env.CONFIG_DATA;
  resetConfigLoader();
  resetConsentWarning();
});

describe('parseCookie', () => {
  it('accepts a freshly minted id and signature', () => {
    const id = mintId();
    const header = `other=1; _pa=${id}.${sign(SECRET, id)}; last=2`;

    expect(parseCookie(header, '_pa', SECRET)).toBe(id);
  });

  specTest(
    'ignores a forged signature',
    { feature: FEATURE, requirement: CONSENT, check: 'a-forged-cookie-signature-is-ignored' },
    () => {
      const id = mintId();
      const signature = sign(SECRET, id);
      const tampered = `${signature.slice(0, -1)}${signature.endsWith('A') ? 'B' : 'A'}`;

      expect(parseCookie(`_pa=${id}.${tampered}`, '_pa', SECRET)).toBeUndefined();
      expect(parseCookie(`_pa=${id}.${signature.slice(0, 10)}`, '_pa', SECRET)).toBeUndefined();
      expect(parseCookie(`_pa=${id}.${signature}x`, '_pa', SECRET)).toBeUndefined();
      expect(
        parseCookie(`_pa=${id}.${sign('another-secret-of-at-least-32-chars!', id)}`, '_pa', SECRET),
      ).toBeUndefined();
    },
  );

  it('rejects a value with no separator, an empty id, and an absent header', () => {
    const id = mintId();

    expect(parseCookie(`_pa=${id}${sign(SECRET, id)}`, '_pa', SECRET)).toBeUndefined();
    expect(parseCookie(`_pa=.${sign(SECRET, '')}`, '_pa', SECRET)).toBeUndefined();
    expect(parseCookie('_pa', '_pa', SECRET)).toBeUndefined();
    expect(parseCookie(`_other=${id}.${sign(SECRET, id)}`, '_pa', SECRET)).toBeUndefined();
    expect(parseCookie(null, '_pa', SECRET)).toBeUndefined();
    expect(parseCookie('', '_pa', SECRET)).toBeUndefined();
  });
});

describe('serializeCookie', () => {
  it('renders the documented attributes, and Secure only over TLS', () => {
    expect(serializeCookie('_pa', 'value', 33_696_000, false)).toBe(
      '_pa=value; Path=/; Max-Age=33696000; HttpOnly; SameSite=Lax',
    );
    expect(serializeCookie('_pa', 'value', 0, true)).toBe(
      '_pa=value; Path=/; Max-Age=0; HttpOnly; SameSite=Lax; Secure',
    );
  });
});

describe('optOutSignal', () => {
  it('honours each signal only when its option is on', () => {
    const respectBoth = analyticsConfig();
    const respectNeither = analyticsConfig({ respectGpc: false, respectDnt: false });

    expect(optOutSignal(context({ 'Sec-GPC': '1' }), respectBoth)).toBe(true);
    expect(optOutSignal(context({ DNT: '1' }), respectBoth)).toBe(true);
    expect(optOutSignal(context({ DNT: '0' }), respectBoth)).toBe(false);
    expect(optOutSignal(context(), respectBoth)).toBe(false);
    expect(optOutSignal(context({ 'Sec-GPC': '1', DNT: '1' }), respectNeither)).toBe(false);
  });
});

describe('resolveIdentifiedVisitor', () => {
  specTest(
    'mints nothing before consent',
    { feature: FEATURE, requirement: CONSENT, check: 'no-cookie-before-consent' },
    async () => {
      const config = analyticsConfig();

      expect(
        await resolveIdentifiedVisitor(
          context(),
          runtime(config, () => false),
        ),
      ).toBeUndefined();
      expect(await resolveIdentifiedVisitor(context(), runtime(config))).toBeUndefined();
      expect(
        await resolveIdentifiedVisitor(
          context(),
          runtime(config, () => {
            throw new Error('consent store unreachable');
          }),
        ),
      ).toBeUndefined();
      expect(
        await resolveIdentifiedVisitor(
          context(),
          runtime(analyticsConfig({ mode: 'cookieless' }), () => true),
        ),
      ).toBeUndefined();
    },
  );

  specTest(
    'mints a signed cookie once consent is given',
    { feature: FEATURE, requirement: CONSENT, check: 'a-signed-cookie-is-minted-after-consent' },
    async () => {
      const config = analyticsConfig();
      const decision = await resolveIdentifiedVisitor(
        context(),
        runtime(config, async () => true),
      );

      expect(decision?.visitorId).toMatch(/^[A-Za-z0-9_-]{22}$/);
      expect(decision?.setCookie).toBe(
        serializeCookie('_pa', `${decision?.visitorId}.${sign(SECRET, decision?.visitorId ?? '')}`, 33_696_000, false),
      );

      const secured = await resolveIdentifiedVisitor(
        context({}, true),
        runtime(config, () => true),
      );
      expect(secured?.setCookie).toContain('; Secure');
    },
  );

  it('reuses a valid cookie and re-mints a forged one', async () => {
    const config = analyticsConfig();
    const id = mintId();
    const valid = context({ Cookie: `_pa=${id}.${sign(SECRET, id)}` });

    const reused = await resolveIdentifiedVisitor(
      valid,
      runtime(config, () => true),
    );
    expect(reused).toEqual({ visitorId: id });

    const forged = context({ Cookie: `_pa=${id}.not-a-signature` });
    const reminted = await resolveIdentifiedVisitor(
      forged,
      runtime(config, () => true),
    );
    expect(reminted?.visitorId).not.toBe(id);
    expect(reminted?.setCookie).toBeDefined();
  });

  specTest(
    'stays cookieless under Sec-GPC',
    { feature: FEATURE, requirement: OPT_OUT, check: 'sec-gpc-forces-cookieless' },
    async () => {
      const consenting = runtime(analyticsConfig(), () => true);

      expect(await resolveIdentifiedVisitor(context({ 'Sec-GPC': '1' }), consenting)).toBeUndefined();
      expect(await resolveIdentifiedVisitor(context(), consenting)).toBeDefined();
    },
  );

  specTest(
    'stays cookieless under DNT when the option respects it',
    { feature: FEATURE, requirement: OPT_OUT, check: 'dnt-forces-cookieless-when-respected' },
    async () => {
      const respecting = runtime(analyticsConfig(), () => true);
      expect(await resolveIdentifiedVisitor(context({ DNT: '1' }), respecting)).toBeUndefined();

      const ignoring = runtime(analyticsConfig({ respectDnt: false }), () => true);
      expect(await resolveIdentifiedVisitor(context({ DNT: '1' }), ignoring)).toBeDefined();
    },
  );
});

describe('forget', () => {
  it('queues a deletion with Max-Age=0', () => {
    analyticsConfig({ cookieName: '_pa' });
    const ctx = context();

    forget(ctx);

    expect(takePendingCookies(ctx)).toEqual(['_pa=; Path=/; Max-Age=0; HttpOnly; SameSite=Lax']);
    expect(takePendingCookies(ctx)).toEqual([]);
  });

  it('keeps every queued cookie in order', () => {
    analyticsConfig({ cookieName: '_analytics' });
    const ctx = context({}, true);

    forget(ctx);
    forget(ctx);

    const pending = takePendingCookies(ctx);
    expect(pending).toHaveLength(2);
    expect(pending[0]).toBe('_analytics=; Path=/; Max-Age=0; HttpOnly; SameSite=Lax; Secure');
  });
});
