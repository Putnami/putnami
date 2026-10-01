import { describe, expect, it } from 'bun:test';
import { restoreEnv } from '@putnami/utils';
import { Default, Env, Int, Optional } from '../src';
import { getEnv, resetConfigLoader, tryContext, useConfig } from '../src/index.browser';
import { specTest } from '../src/spectest';

describe('browser entrypoint stubs', () => {
  describe('tryContext', () => {
    it('reports that server request context is unavailable', () => {
      expect(tryContext()).toBeUndefined();
    });
  });

  describe('useConfig', () => {
    specTest(
      'applies schema Default() values so typed reads are not undefined',
      {
        feature: 'typescript/typed-configuration',
        requirement: 'browser-surface',
        check: 'declared-defaults-apply-in-the-browser',
      },
      () => {
        const config = useConfig({
          schema: {
            host: Default(String, 'localhost'),
            port: Default(Int, 5432),
          },
        });

        expect(config.host).toBe('localhost');
        expect(config.port).toBe(5432);
      },
    );

    specTest(
      'lets confInit override defaults',
      {
        feature: 'typescript/typed-configuration',
        requirement: 'browser-surface',
        check: 'call-site-overrides-apply-in-the-browser',
      },
      () => {
        const config = useConfig(
          {
            schema: {
              host: Default(String, 'localhost'),
              port: Default(Int, 5432),
            },
          },
          { confInit: { port: 3306 } },
        );

        expect(config.host).toBe('localhost');
        expect(config.port).toBe(3306);
      },
    );

    specTest(
      'does not throw on missing required fields (no client config sources)',
      {
        feature: 'typescript/typed-configuration',
        requirement: 'browser-surface',
        check: 'a-missing-required-field-does-not-throw-in-the-browser',
      },
      () => {
        expect(() =>
          useConfig({
            schema: {
              required: String,
              optional: Optional(String),
            },
          }),
        ).not.toThrow();

        const config = useConfig({
          schema: {
            required: String,
          },
        });
        expect(config.required).toBeUndefined();
      },
    );

    specTest(
      'reads no server config source, not even when one is present in the process',
      {
        feature: 'typescript/typed-configuration',
        requirement: 'browser-surface',
        check: 'the-browser-entry-reads-no-server-config-source',
      },
      () => {
        // A browser bundle has no files, no CONFIG_DATA document, and no process
        // environment. Bun exposes all three to this test, which makes it the
        // only place the stub could accidentally start reading them.
        const previousConfigData = process.env['CONFIG_DATA'];
        const previousSecret = process.env['BROWSER_STUB_SECRET'];
        process.env['CONFIG_DATA'] = 'browserStub:\n  host: leaked-from-config-data\n';
        process.env['BROWSER_STUB_SECRET'] = 'leaked-from-env';
        try {
          const config = useConfig({
            schema: {
              host: Default(String, 'localhost'),
              secret: Env('BROWSER_STUB_SECRET', Default(String, 'unset')),
            },
          });

          expect(config.host).toBe('localhost');
          expect(config.secret).toBe('unset');
        } finally {
          restoreEnv('CONFIG_DATA', previousConfigData);
          restoreEnv('BROWSER_STUB_SECRET', previousSecret);
        }
      },
    );
  });

  describe('browser barrel surface', () => {
    it('exposes no server-only configuration or context symbol', async () => {
      // The browser build must not reach the file loaders, the container, or
      // the request-context storage: each pulls a Node built-in into the bundle.
      const browserModule = (await import('../src/index.browser')) as unknown as Record<string, unknown>;

      for (const symbol of [
        'ContainerContext',
        'Container',
        'provide',
        'module',
        'useLogger',
        'runInContext',
        'useContext',
        'getDefaultSources',
        'loadGlobalConfig',
        'registerSourceDiscoverer',
      ]) {
        expect(browserModule[symbol]).toBeUndefined();
      }
    });
  });

  describe('getEnv', () => {
    specTest(
      'always reports browser',
      {
        feature: 'typescript/typed-configuration',
        requirement: 'browser-surface',
        check: 'the-browser-entry-reports-the-browser-environment',
      },
      () => {
        expect(getEnv()).toBe('browser');
      },
    );
  });

  describe('resetConfigLoader', () => {
    it('is a no-op that does not throw', () => {
      expect(() => resetConfigLoader()).not.toThrow();
    });
  });
});
