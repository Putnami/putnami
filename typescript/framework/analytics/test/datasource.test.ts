import { afterEach, describe, expect, it } from 'bun:test';
import { resetConfigLoader } from '@putnami/runtime';
import { resolveAnalyticsDatasource } from '../src/server/datasource';

function setConfig(tree: Record<string, unknown>): void {
  process.env.CONFIG_DATA = JSON.stringify(tree);
  resetConfigLoader();
}

afterEach(() => {
  delete process.env.CONFIG_DATA;
  delete process.env.DATABASE_BINDINGS;
  resetConfigLoader();
});

describe('resolveAnalyticsDatasource', () => {
  it('short-circuits on "default" without reading any config', () => {
    setConfig({});

    expect(resolveAnalyticsDatasource('default')).toBe('default');
  });

  it('keeps the name when the database config declares it', () => {
    setConfig({ database: { analytics: { host: 'localhost', database: 'analytics' } } });

    expect(resolveAnalyticsDatasource('analytics')).toBe('analytics');
  });

  it('keeps the name when a merged binding declares it', () => {
    setConfig({
      database: {
        databases: {
          analytics: { engine: 'postgres', schema: 'analytics', connection: { host: 'h', database: 'a' } },
        },
      },
    });

    expect(resolveAnalyticsDatasource('analytics')).toBe('analytics');
  });

  it('keeps the name when DATABASE_BINDINGS declares it', () => {
    setConfig({});
    process.env.DATABASE_BINDINGS = JSON.stringify({
      protocolVersion: 1,
      databases: { analytics: { engine: 'postgres', schema: 'analytics', connection: { host: 'h', database: 'a' } } },
    });

    expect(resolveAnalyticsDatasource('analytics')).toBe('analytics');
  });

  it('falls back to default when the name is declared nowhere', () => {
    setConfig({ database: { host: 'localhost' } });

    expect(resolveAnalyticsDatasource('analytics')).toBe('default');
  });

  it('falls back to default when DATABASE_BINDINGS is not valid JSON', () => {
    setConfig({});
    process.env.DATABASE_BINDINGS = 'not-json';

    expect(resolveAnalyticsDatasource('analytics')).toBe('default');
  });
});
