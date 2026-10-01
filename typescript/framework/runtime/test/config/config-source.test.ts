import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import type { ConfigSource } from '../../src/config/config-source';
import { getDefaultSources, loadGlobalConfig } from '../../src/config/config-source';
import { specTest } from '../../src/spectest';

describe('ConfigSource', () => {
  const envNames = [
    'CONFIG_DATA',
    'CONFIG_SERVER_URL',
    'CONFIG_SERVER_TOKEN',
    'PUTNAMI_CLOUD_TOKEN',
    'PUTNAMI_TOKEN',
    'K_SERVICE',
    'APP_ENV',
  ] as const;
  let envSnapshot: Record<(typeof envNames)[number], string | undefined>;

  beforeEach(() => {
    envSnapshot = Object.fromEntries(envNames.map((name) => [name, process.env[name]])) as typeof envSnapshot;
    for (const name of envNames) {
      delete process.env[name];
    }
  });

  afterEach(() => {
    for (const name of envNames) {
      const value = envSnapshot[name];
      if (value === undefined) delete process.env[name];
      else process.env[name] = value;
    }
  });

  describe('getDefaultSources', () => {
    it('returns the 5 file-and-env built-in sources by default', () => {
      const sources = getDefaultSources();
      expect(sources).toHaveLength(5);
    });

    specTest(
      'includes base, env, gen, secrets, and CONFIG_DATA sources',
      {
        feature: 'typescript/typed-configuration',
        requirement: 'source-precedence',
        check: 'the-five-built-in-sources-are-registered',
      },
      () => {
        const sources = getDefaultSources();
        const names = sources.map((s) => s.name).sort();
        expect(names).toEqual(['.gen/conf', 'CONFIG_DATA', 'conf', 'conf/.secrets', 'conf/base']);
      },
    );

    it('fails loudly when CONFIG_SERVER_URL is set and no registered source serves it', () => {
      process.env.CONFIG_SERVER_URL = 'http://127.0.0.1:1/unreachable';
      process.env.CONFIG_SERVER_TOKEN = 'must-not-be-read-by-core';
      process.env.CONFIG_DATA = JSON.stringify({ server: { port: 8080 } });

      expect(() => getDefaultSources()).toThrow(/CONFIG_SERVER_URL is set but no registered config source serves it/);

      // The one line names the gap an operator has to close: depend on the
      // extension package and activate it with a static import, because a
      // bun-compiled binary cannot resolve the optional dynamic require.
      let message = '';
      try {
        getDefaultSources();
      } catch (error) {
        message = (error as Error).message;
      }
      expect(message.split('\n')).toHaveLength(1);
      expect(message).toContain('@putnami/cloud/runtime');
      expect(message).toContain('static import');
      expect(message).toContain('bun-compiled binary');
    });

    it('stays silent in dev/test when CONFIG_SERVER_URL is unset', () => {
      process.env.CONFIG_DATA = JSON.stringify({ server: { port: 8080 } });

      const sources = getDefaultSources();

      expect(sources.map((source) => source.name).sort()).toEqual([
        '.gen/conf',
        'CONFIG_DATA',
        'conf',
        'conf/.secrets',
        'conf/base',
      ]);
      expect(loadGlobalConfig(sources)).toEqual({ server: { port: 8080 } });
    });

    specTest(
      'CONFIG_DATA sits below field-level Env (60) and above file sources',
      {
        feature: 'typescript/typed-configuration',
        requirement: 'source-precedence',
        check: 'config-data-outranks-file-sources',
      },
      () => {
        const sources = getDefaultSources();
        const env = sources.find((s) => s.name === 'CONFIG_DATA');
        const secrets = sources.find((s) => s.name === 'conf/.secrets');
        const gen = sources.find((s) => s.name === '.gen/conf');
        const conf = sources.find((s) => s.name === 'conf');
        const base = sources.find((s) => s.name === 'conf/base');

        expect(env?.priority).toBe(60);
        expect(secrets?.priority).toBe(35);
        expect(gen?.priority).toBe(30);
        expect(conf?.priority).toBe(20);
        expect(base?.priority).toBe(10);
      },
    );
  });

  describe('loadGlobalConfig', () => {
    it('returns undefined when no sources have data', () => {
      const sources: ConfigSource[] = [{ name: 'empty', priority: 10, load: () => undefined }];
      expect(loadGlobalConfig(sources)).toBeUndefined();
    });

    it('returns data from a single source', () => {
      const sources: ConfigSource[] = [
        { name: 'test', priority: 10, load: () => ({ database: { host: 'localhost' } }) },
      ];
      const result = loadGlobalConfig(sources);
      expect(result).toEqual({ database: { host: 'localhost' } });
    });

    specTest(
      'merges multiple sources by priority (higher wins)',
      {
        feature: 'typescript/typed-configuration',
        requirement: 'source-precedence',
        check: 'a-higher-priority-source-wins-the-merge',
      },
      () => {
        const sources: ConfigSource[] = [
          { name: 'low', priority: 10, load: () => ({ database: { host: 'low', port: 5432 } }) },
          { name: 'high', priority: 50, load: () => ({ database: { host: 'high' } }) },
        ];
        const result = loadGlobalConfig(sources);
        expect(result).toEqual({ database: { host: 'high', port: 5432 } });
      },
    );

    it('skips sources that return undefined', () => {
      const sources: ConfigSource[] = [
        { name: 'empty', priority: 100, load: () => undefined },
        { name: 'data', priority: 10, load: () => ({ app: { name: 'test' } }) },
      ];
      const result = loadGlobalConfig(sources);
      expect(result).toEqual({ app: { name: 'test' } });
    });

    it('deep-merges nested config', () => {
      const sources: ConfigSource[] = [
        { name: 'base', priority: 10, load: () => ({ a: { b: { c: 1, d: 2 } } }) },
        { name: 'override', priority: 20, load: () => ({ a: { b: { c: 99 } } }) },
      ];
      const result = loadGlobalConfig(sources);
      expect(result).toEqual({ a: { b: { c: 99, d: 2 } } });
    });

    specTest(
      'handles three sources with correct priority ordering',
      {
        feature: 'typescript/typed-configuration',
        requirement: 'source-precedence',
        check: 'three-sources-merge-in-priority-order',
      },
      () => {
        const sources: ConfigSource[] = [
          { name: 'mid', priority: 50, load: () => ({ server: { port: 50, host: 'mid' } }) },
          { name: 'low', priority: 10, load: () => ({ server: { port: 10, host: 'low', debug: true } }) },
          { name: 'high', priority: 90, load: () => ({ server: { port: 90 } }) },
        ];
        const result = loadGlobalConfig(sources);
        expect(result).toEqual({ server: { port: 90, host: 'mid', debug: true } });
      },
    );
  });

  describe('EnvDataSource (via CONFIG_DATA)', () => {
    it('loads from CONFIG_DATA env var', () => {
      process.env.CONFIG_DATA = JSON.stringify({ test: { value: 'from-env' } });
      const sources = getDefaultSources();
      const envSource = sources.find((s) => s.name === 'CONFIG_DATA')!;
      const data = envSource.load();
      expect(data).toEqual({ test: { value: 'from-env' } });
    });

    it('returns undefined when CONFIG_DATA is not set', () => {
      delete process.env.CONFIG_DATA;
      const sources = getDefaultSources();
      const envSource = sources.find((s) => s.name === 'CONFIG_DATA')!;
      expect(envSource.load()).toBeUndefined();
    });

    it('parses YAML from CONFIG_DATA', () => {
      process.env.CONFIG_DATA = 'database:\n  host: prod.db\n  port: 5432';
      const sources = getDefaultSources();
      const envSource = sources.find((s) => s.name === 'CONFIG_DATA')!;
      const data = envSource.load();
      expect(data).toEqual({ database: { host: 'prod.db', port: 5432 } });
    });
  });

  describe('env var interpolation', () => {
    const source = (data: Record<string, unknown>): ConfigSource[] => [
      { name: 'test', priority: 10, load: () => data },
    ];

    it('replaces ${VAR} with process.env value', () => {
      process.env.TEST_HOST = 'db.example.com';
      const result = loadGlobalConfig(source({ database: { host: '${TEST_HOST}' } }));
      expect(result).toEqual({ database: { host: 'db.example.com' } });
      delete process.env.TEST_HOST;
    });

    it('supports ${VAR:-default} fallback', () => {
      delete process.env.TEST_MISSING;
      const result = loadGlobalConfig(source({ db: { host: '${TEST_MISSING:-localhost}' } }));
      expect(result).toEqual({ db: { host: 'localhost' } });
    });

    it('leaves unresolved ${VAR} as-is when no default', () => {
      delete process.env.TEST_NOPE;
      const result = loadGlobalConfig(source({ db: { host: '${TEST_NOPE}' } }));
      expect(result).toEqual({ db: { host: '${TEST_NOPE}' } });
    });

    it('interpolates embedded references in a string', () => {
      process.env.TEST_SCHEME = 'https';
      process.env.TEST_DOMAIN = 'api.example.com';
      const result = loadGlobalConfig(source({ url: '${TEST_SCHEME}://${TEST_DOMAIN}/v1' }));
      expect(result).toEqual({ url: 'https://api.example.com/v1' });
      delete process.env.TEST_SCHEME;
      delete process.env.TEST_DOMAIN;
    });

    it('does not touch non-string values', () => {
      const result = loadGlobalConfig(source({ port: 5432, debug: true }));
      expect(result).toEqual({ port: 5432, debug: true });
    });

    it('interpolates inside arrays', () => {
      process.env.TEST_TAG = 'prod';
      const result = loadGlobalConfig(source({ tags: ['${TEST_TAG}', 'static'] }));
      expect(result).toEqual({ tags: ['prod', 'static'] });
      delete process.env.TEST_TAG;
    });
  });

  describe('Custom ConfigSource', () => {
    it('can be used in loadGlobalConfig', () => {
      const customSource: ConfigSource = {
        name: 'custom',
        priority: 100,
        load: () => ({ custom: { key: 'value' } }),
      };

      const result = loadGlobalConfig([customSource]);
      expect(result).toEqual({ custom: { key: 'value' } });
    });

    specTest(
      'custom source overrides default sources when higher priority',
      {
        feature: 'typescript/typed-configuration',
        requirement: 'source-precedence',
        check: 'a-registered-extension-source-can-outrank-the-defaults',
      },
      () => {
        process.env.CONFIG_DATA = JSON.stringify({ shared: { value: 'from-env' } });

        const customSource: ConfigSource = {
          name: 'custom',
          priority: 100, // Higher than CONFIG_DATA (60)
          load: () => ({ shared: { value: 'from-custom' } }),
        };

        const sources = [...getDefaultSources(), customSource];
        const result = loadGlobalConfig(sources);
        expect(result?.shared).toEqual({ value: 'from-custom' });
      },
    );
  });
});
