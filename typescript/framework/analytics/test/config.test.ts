import { afterEach, describe, expect, it } from 'bun:test';
import { resetConfigLoader, useConfig } from '@putnami/runtime';
import { AnalyticsConfig, MISSING_SECRET_MESSAGE, resolveSecret } from '../src/server/analytics.config';

function setConfig(tree: Record<string, unknown>): void {
  process.env.CONFIG_DATA = JSON.stringify(tree);
  resetConfigLoader();
}

afterEach(() => {
  delete process.env.CONFIG_DATA;
  resetConfigLoader();
});

describe('AnalyticsConfig', () => {
  it('resolves the documented defaults with no analytics section', () => {
    setConfig({});
    const config = useConfig(AnalyticsConfig);

    expect(config.enabled).toBe(true);
    expect(config.mode).toBe('cookieless');
    expect(config.datasource).toBe('analytics');
    expect(config.secret).toBeUndefined();
    expect(config.serverPageViews).toBe(true);
    expect(config.respectGpc).toBe(true);
    expect(config.respectDnt).toBe(true);
    expect(config.countryHeader).toBeUndefined();
    expect(config.trustedProxies).toBeUndefined();
    expect(config.retentionRawDays).toBe(90);
    expect(config.retentionAggregateDays).toBe(760);
    expect(config.retentionMode).toBe('sweep');
    expect(config.maxPathKeysPerDay).toBe(2000);
    expect(config.rateLimitPerMinute).toBe(120);
    expect(config.cookieName).toBe('_pa');
    expect(config.cookieMaxAgeDays).toBe(390);
  });

  it('reads the analytics section over the defaults', () => {
    setConfig({
      analytics: {
        enabled: false,
        mode: 'identified',
        datasource: 'default',
        retentionRawDays: 30,
        trustedProxies: ['10.0.0.1'],
        countryHeader: 'cf-ipcountry',
      },
    });
    const config = useConfig(AnalyticsConfig);

    expect(config.enabled).toBe(false);
    expect(config.mode).toBe('identified');
    expect(config.datasource).toBe('default');
    expect(config.retentionRawDays).toBe(30);
    expect(config.trustedProxies).toEqual(['10.0.0.1']);
    expect(config.countryHeader).toBe('cf-ipcountry');
  });

  it('rejects a mode outside the closed set', () => {
    setConfig({ analytics: { mode: 'pseudonymous' } });

    expect(() => useConfig(AnalyticsConfig)).toThrow();
  });
});

describe('resolveSecret', () => {
  it('prefers analytics.secret', () => {
    setConfig({
      analytics: { secret: 'a'.repeat(32) },
      session: { cookieSecret: 'b'.repeat(32) },
    });

    expect(resolveSecret(useConfig(AnalyticsConfig))).toBe('a'.repeat(32));
  });

  it('falls back to session.cookieSecret', () => {
    setConfig({ session: { cookieSecret: 'b'.repeat(32) } });

    expect(resolveSecret(useConfig(AnalyticsConfig))).toBe('b'.repeat(32));
  });

  it('fails closed when neither secret is configured', () => {
    setConfig({});

    expect(() => resolveSecret(useConfig(AnalyticsConfig))).toThrow(MISSING_SECRET_MESSAGE);
  });

  it('fails closed when the session section is absent entirely', () => {
    setConfig({ database: { host: 'localhost' } });

    expect(() => resolveSecret(useConfig(AnalyticsConfig))).toThrow(MISSING_SECRET_MESSAGE);
  });
});
