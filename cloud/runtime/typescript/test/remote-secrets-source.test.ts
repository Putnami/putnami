// biome-ignore-all lint/suspicious/noConsole: Captures JsonSink output written through console.log.
import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { resetConfigLoader } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import {
  discoverRemoteSecretsSource,
  RemoteSecretsSource,
  resetRemoteSecretsSourceCacheForTest,
} from '../src/runtime/remote-secrets-source';
import { setSyncFetchForTest, type SyncFetchRequest } from '../src/runtime/sync-fetch';
import { resetDefaultLogger } from '@putnami/runtime';
import { resetLoggerConfig } from '@putnami/runtime';

describe('RemoteSecretsSource', () => {
  const originalConsoleLog = console.log;
  const remoteEnvNames = [
    'CONFIG_SERVER_URL',
    'CONFIG_SERVER_TOKEN',
    'CONFIG_SERVER_TIMEOUT',
    'CONFIG_SERVER_RETRY_BUDGET',
    'CONFIG_SERVER_REQUIRED',
    'APP_ENV',
    'K_SERVICE',
    'NODE_ENV',
  ] as const;
  let envSnapshot: Record<(typeof remoteEnvNames)[number], string | undefined>;

  beforeEach(() => {
    envSnapshot = Object.fromEntries(remoteEnvNames.map((name) => [name, process.env[name]])) as typeof envSnapshot;
    for (const name of remoteEnvNames) {
      delete process.env[name];
    }
    setSyncFetchForTest(undefined);
    resetConfigLoader();
    resetRemoteSecretsSourceCacheForTest();
    resetLoggerConfig();
    resetDefaultLogger();
  });

  afterEach(() => {
    for (const name of remoteEnvNames) {
      const value = envSnapshot[name];
      if (value === undefined) delete process.env[name];
      else process.env[name] = value;
    }
    setSyncFetchForTest(undefined);
    console.log = originalConsoleLog;
    resetConfigLoader();
    resetRemoteSecretsSourceCacheForTest();
    resetLoggerConfig();
    resetDefaultLogger();
  });

  specTest(
    'hits /api/secrets/resolve with bearer token and schemaHash',
    {
      feature: 'cloud/typescript-runtime-destination',
      requirement: 'secrets-source-resolves-remote-secrets',
      check: 'a-resolve-posts-the-bearer-and-schema-hash',
    },
    () => {
      let captured: SyncFetchRequest | undefined;
      setSyncFetchForTest((request) => {
        captured = request;
        return {
          status: 200,
          body: JSON.stringify({
            secrets: { database: { password: 'shh' } },
            resolved: true,
            schemaMatch: true,
          }),
        };
      });

      const source = new RemoteSecretsSource({
        serverUrl: 'https://config.putnami.test',
        appName: 'task-api',
        environment: 'production',
        schemaHash: 'sha256:abcdef0123456789',
        token: 'test-token',
      });

      expect(source.name).toBe('secrets-server');
      expect(source.priority).toBe(55);
      expect(source.load()).toEqual({ database: { password: 'shh' } });

      expect(captured?.url).toBe('https://config.putnami.test/api/secrets/resolve');
      expect(captured?.method).toBe('POST');
      expect(captured?.headers?.Authorization).toBe('Bearer test-token');
      expect(captured?.body).toContain('sha256:abcdef0123456789');
    },
  );

  specTest(
    'returns undefined when not resolved',
    {
      feature: 'cloud/typescript-runtime-destination',
      requirement: 'secrets-source-resolves-remote-secrets',
      check: 'an-unresolved-response-yields-no-secrets',
    },
    () => {
      setSyncFetchForTest(() => ({
        status: 200,
        body: JSON.stringify({ secrets: {}, resolved: false, schemaMatch: false }),
      }));

      const source = new RemoteSecretsSource({
        serverUrl: 'https://config.putnami.test',
        appName: 'unknown',
        environment: 'local',
      });
      expect(source.load()).toBeUndefined();
    },
  );

  specTest(
    'caches across calls',
    {
      feature: 'cloud/typescript-runtime-destination',
      requirement: 'secrets-source-resolves-remote-secrets',
      check: 'later-loads-reuse-the-first-resolve',
    },
    () => {
      let calls = 0;
      setSyncFetchForTest(() => {
        calls += 1;
        return {
          status: 200,
          body: JSON.stringify({ secrets: {}, resolved: true, schemaMatch: true }),
        };
      });

      const source = new RemoteSecretsSource({
        serverUrl: 'https://config.putnami.test',
        appName: 'task-api',
        environment: 'production',
      });
      source.load();
      source.load();
      expect(calls).toBe(1);
    },
  );

  specTest(
    'retries required remote secrets until the server is ready',
    {
      feature: 'cloud/typescript-runtime-destination',
      requirement: 'required-sources-retry-within-a-budget',
      check: 'a-required-secrets-source-retries-until-ready',
    },
    () => {
      let calls = 0;
      setSyncFetchForTest(() => {
        calls += 1;
        if (calls === 1) {
          throw new Error('connection refused');
        }
        return {
          status: 200,
          body: JSON.stringify({
            secrets: { database: { password: 'ready' } },
            resolved: true,
            schemaMatch: true,
          }),
        };
      });

      const source = new RemoteSecretsSource({
        serverUrl: 'https://config.putnami.test',
        appName: 'task-api',
        environment: 'production',
        timeout: 50,
        retryBudget: 500,
        required: true,
      });

      expect(source.load()).toEqual({ database: { password: 'ready' } });
      expect(calls).toBe(2);
    },
  );

  specTest(
    'does not retry terminal HTTP status responses',
    {
      feature: 'cloud/typescript-runtime-destination',
      requirement: 'required-sources-retry-within-a-budget',
      check: 'a-terminal-secrets-status-fails-without-a-retry',
    },
    () => {
      let calls = 0;
      let captured: SyncFetchRequest | undefined;
      setSyncFetchForTest((request) => {
        calls += 1;
        captured = request;
        return { status: 404, body: 'not found' };
      });

      const source = new RemoteSecretsSource({
        serverUrl: 'https://config.putnami.test',
        appName: 'task-api',
        environment: 'production',
        timeout: 50,
        retryBudget: 500,
        required: true,
      });

      expect(() => source.load()).toThrow(/secrets-server returned non-200: status 404/);
      expect(calls).toBe(1);
      expect(captured?.throwOnHttpError).toBe(false);
    },
  );

  specTest(
    'discoverRemoteSecretsSource reads timeout and retry budget env vars',
    {
      feature: 'cloud/typescript-runtime-destination',
      requirement: 'required-sources-retry-within-a-budget',
      check: 'a-zero-retry-budget-allows-one-attempt',
    },
    () => {
      process.env.CONFIG_SERVER_URL = 'https://config.putnami.test';
      process.env.CONFIG_SERVER_TOKEN = 'discovered-token';
      process.env.CONFIG_SERVER_TIMEOUT = '2s';
      process.env.CONFIG_SERVER_RETRY_BUDGET = '0ms';
      process.env.APP_ENV = 'prod';

      let captured: SyncFetchRequest | undefined;
      let calls = 0;
      setSyncFetchForTest((request) => {
        calls += 1;
        captured = request;
        throw new Error('network down');
      });

      const source = discoverRemoteSecretsSource();
      expect(source).toBeDefined();
      expect(() => source?.load()).toThrow(/secrets-server fetch failed: network down/);
      expect(captured?.timeoutMs).toBe(2000);
      expect(calls).toBe(1);
    },
  );

  it('discoverRemoteSecretsSource returns undefined when CONFIG_SERVER_URL is unset', () => {
    process.env.CONFIG_SERVER_URL = undefined;
    expect(discoverRemoteSecretsSource()).toBeUndefined();
  });

  it('treats blank and orchestration sentinel CONFIG_SERVER_URL values as unset', () => {
    for (const value of ['', '   ', 'undefined', ' UnDeFiNeD ', 'null']) {
      process.env.CONFIG_SERVER_URL = value;
      expect(discoverRemoteSecretsSource()).toBeUndefined();
    }
  });

  it('discoverRemoteSecretsSource skips exact config resolve URLs', () => {
    process.env.CONFIG_SERVER_URL =
      'https://api.putnami.test/api/configs/resolve?appName=auth%2Fserver&environment=prod&secretsMode=reveal';
    expect(discoverRemoteSecretsSource()).toBeUndefined();
  });

  specTest(
    'treats NODE_ENV=production as a required remote secrets source',
    {
      feature: 'cloud/typescript-runtime-destination',
      requirement: 'required-sources-stop-startup',
      check: 'production-makes-the-secrets-source-required',
    },
    () => {
      let calls = 0;
      setSyncFetchForTest(() => {
        calls += 1;
        throw new Error('network down');
      });
      process.env.CONFIG_SERVER_URL = 'https://config.putnami.test';
      process.env.CONFIG_SERVER_TOKEN = 'discovered-token';
      process.env.CONFIG_SERVER_RETRY_BUDGET = '0';
      process.env.NODE_ENV = 'production';

      const source = discoverRemoteSecretsSource();
      expect(source).toBeDefined();
      expect(() => source?.load()).toThrow(/secrets-server fetch failed: network down/);
      expect(calls).toBe(1);
    },
  );
});
