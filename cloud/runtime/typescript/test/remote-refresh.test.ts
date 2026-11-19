// biome-ignore-all lint/suspicious/noConsole: Captures JsonSink output written through console.log.
import { afterEach, beforeEach, describe, expect, mock } from 'bun:test';
import { resetConfigLoader, resetDefaultLogger, resetLoggerConfig } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import {
  discoverRemoteSource,
  RemoteConfigSource,
  remoteConfigRefreshSettledForTest,
  resetRemoteConfigSourceCacheForTest,
} from '../src/runtime/remote-config-source';
import { setRemoteRefreshClockForTest } from '../src/runtime/remote-refresh';
import {
  discoverRemoteSecretsSource,
  RemoteSecretsSource,
  remoteSecretsRefreshSettledForTest,
  resetRemoteSecretsSourceCacheForTest,
} from '../src/runtime/remote-secrets-source';
import {
  asyncFetch,
  resetSyncFetchForTest,
  setSyncFetchForTest,
  type SyncFetchRequest,
} from '../src/runtime/sync-fetch';
import { CONFIG_SERVER_TOKEN_ENV_NAMES, GcpMetadataTokenSource, type TokenSource } from '../src/runtime/token-source';

const feature = 'cloud/typescript-runtime-destination';
const requirement = 'remote-sources-refresh-on-a-period';

/** A resolve server whose answer carries its current version under `key`. */
function versionedServer(field: 'config' | 'secrets') {
  const server = { version: 1, status: 200, calls: 0, requests: [] as SyncFetchRequest[] };
  setSyncFetchForTest((request) => {
    server.calls += 1;
    server.requests.push(request);
    if (server.status !== 200) {
      return { status: server.status, body: '' };
    }
    return {
      status: 200,
      body: JSON.stringify({ [field]: { key: `v${server.version}` }, resolved: true, schemaMatch: true }),
    };
  });
  return server;
}

function keyOf(tree: Record<string, unknown> | undefined): unknown {
  return tree?.['key'];
}

describe('remote source refresh', () => {
  const originalConsoleLog = console.log;
  const envNames = [
    'CONFIG_SERVER_URL',
    ...CONFIG_SERVER_TOKEN_ENV_NAMES,
    'GOOGLE_CLOUD_PROJECT',
    'K_SERVICE',
    'CONFIG_SERVER_REFRESH',
    'CONFIG_VERSION',
    'CONFIG_SERVER_REQUIRED',
  ] as const;
  let envSnapshot: Record<string, string | undefined>;
  let now = 0;

  beforeEach(() => {
    envSnapshot = Object.fromEntries(envNames.map((name) => [name, process.env[name]]));
    for (const name of envNames) {
      delete process.env[name];
    }
    now = Date.UTC(2026, 10, 19, 12);
    setRemoteRefreshClockForTest(() => now);
    resetConfigLoader();
    resetRemoteConfigSourceCacheForTest();
    resetRemoteSecretsSourceCacheForTest();
    resetLoggerConfig();
    resetDefaultLogger();
  });

  afterEach(async () => {
    await remoteConfigRefreshSettledForTest();
    await remoteSecretsRefreshSettledForTest();
    for (const name of envNames) {
      const value = envSnapshot[name];
      if (value === undefined) delete process.env[name];
      else process.env[name] = value;
    }
    setRemoteRefreshClockForTest(undefined);
    setSyncFetchForTest(undefined);
    console.log = originalConsoleLog;
    resetConfigLoader();
    resetRemoteConfigSourceCacheForTest();
    resetRemoteSecretsSourceCacheForTest();
    resetLoggerConfig();
    resetDefaultLogger();
  });

  specTest(
    'keeps the first load without a refresh period',
    { feature, requirement, check: 'refresh-off-keeps-the-first-load' },
    async () => {
      const server = versionedServer('config');
      const source = new RemoteConfigSource({ serverUrl: 'https://config.putnami.test', appName: 'app' });

      expect(keyOf(source.load())).toBe('v1');
      server.version = 2;
      now += 24 * 3_600_000;
      expect(keyOf(source.load())).toBe('v1');
      await remoteConfigRefreshSettledForTest();
      expect(keyOf(source.load())).toBe('v1');
      expect(server.calls).toBe(1);
    },
  );

  specTest(
    'returns the held tree once the period has passed and refreshes it in the background',
    { feature, requirement, check: 'a-stale-load-returns-the-held-tree-and-refreshes' },
    async () => {
      const server = versionedServer('config');
      const options = { serverUrl: 'https://config.putnami.test', appName: 'app', token: 't1', refreshPeriod: 60_000 };
      const source = new RemoteConfigSource({ ...options });

      expect(keyOf(source.load())).toBe('v1');
      server.version = 2;
      now += 59_000;
      expect(keyOf(source.load())).toBe('v1');
      await remoteConfigRefreshSettledForTest();
      expect(server.calls).toBe(1);

      now += 1000;
      const peer = new RemoteConfigSource({ ...options, token: 't2' });
      expect(keyOf(source.load())).toBe('v1');
      expect(keyOf(peer.load())).toBe('v1');
      await remoteConfigRefreshSettledForTest();
      expect(server.calls).toBe(2);
      expect(server.requests[1]?.headers?.['Authorization']).toBe('Bearer t1');

      expect(keyOf(source.load())).toBe('v2');
      expect(keyOf(peer.load())).toBe('v2');
      await remoteConfigRefreshSettledForTest();
      expect(server.calls).toBe(2);
    },
  );

  specTest(
    'keeps the held tree when a refresh fails, warning once for each new failure',
    { feature, requirement, check: 'a-failed-refresh-keeps-the-held-tree' },
    async () => {
      const lines: string[] = [];
      console.log = mock((line?: unknown) => {
        lines.push(String(line ?? ''));
      }) as unknown as typeof console.log;
      const server = versionedServer('config');
      const source = new RemoteConfigSource({
        serverUrl: 'https://config.putnami.test',
        appName: 'app',
        required: true,
        retryBudget: 30_000,
        snapshotUri: 'gs://snapshots/never-read',
        refreshPeriod: 60_000,
      });
      expect(keyOf(source.load())).toBe('v1');

      for (const status of [503, 403, 403]) {
        server.status = status;
        now += 60_000;
        expect(keyOf(source.load())).toBe('v1');
        await remoteConfigRefreshSettledForTest();
        expect(keyOf(source.load())).toBe('v1');
      }
      expect(server.calls).toBe(4);
      expect(lines.filter((line) => line.includes('remote source refresh failed')).length).toBe(2);

      setSyncFetchForTest(() => ({ status: 200, body: 'not json' }));
      now += 60_000;
      source.load();
      await remoteConfigRefreshSettledForTest();
      expect(keyOf(source.load())).toBe('v1');

      versionedServer('config').version = 3;
      now += 60_000;
      source.load();
      await remoteConfigRefreshSettledForTest();
      expect(keyOf(source.load())).toBe('v3');
    },
  );

  specTest(
    'never refreshes a pinned config',
    { feature, requirement, check: 'a-pinned-config-is-never-refreshed' },
    async () => {
      const server = versionedServer('config');
      const source = new RemoteConfigSource({
        serverUrl: 'https://config.putnami.test',
        appName: 'app',
        version: 'rev-7',
        pinned: true,
        refreshPeriod: 60_000,
      });
      expect(keyOf(source.load())).toBe('v1');
      server.version = 2;
      now += 3_600_000;
      source.load();
      await remoteConfigRefreshSettledForTest();
      expect(keyOf(source.load())).toBe('v1');
      expect(server.calls).toBe(1);
    },
  );

  specTest(
    'refreshes the secrets source the same way',
    { feature, requirement, check: 'the-secrets-source-refreshes' },
    async () => {
      const server = versionedServer('secrets');
      const source = new RemoteSecretsSource({
        serverUrl: 'https://config.putnami.test',
        appName: 'app',
        refreshPeriod: 60_000,
      });
      expect(keyOf(source.load())).toBe('v1');
      server.version = 2;
      now += 60_000;
      expect(keyOf(source.load())).toBe('v1');
      await remoteSecretsRefreshSettledForTest();
      expect(keyOf(source.load())).toBe('v2');

      server.status = 404;
      now += 60_000;
      source.load();
      await remoteSecretsRefreshSettledForTest();
      expect(keyOf(source.load())).toBe('v2');

      setSyncFetchForTest(() => ({ status: 200, body: JSON.stringify({ resolved: false }) }));
      now += 60_000;
      source.load();
      await remoteSecretsRefreshSettledForTest();
      expect(keyOf(source.load())).toBe('v2');
    },
  );

  specTest(
    'reads the refresh period from CONFIG_SERVER_REFRESH',
    { feature, requirement, check: 'discovery-reads-the-refresh-period' },
    async () => {
      process.env['CONFIG_SERVER_URL'] = 'https://config.putnami.test';
      process.env['CONFIG_SERVER_TOKEN'] = 'token';
      const server = versionedServer('config');

      process.env['CONFIG_SERVER_REFRESH'] = '90s';
      expect(keyOf(discoverRemoteSource()?.load())).toBe('v1');
      server.version = 2;
      now += 90_000;
      discoverRemoteSource()?.load();
      await remoteConfigRefreshSettledForTest();
      expect(keyOf(discoverRemoteSource()?.load())).toBe('v2');
      expect(discoverRemoteSecretsSource()).toBeDefined();

      resetRemoteConfigSourceCacheForTest();
      process.env['CONFIG_SERVER_REFRESH'] = 'soon';
      const unset = discoverRemoteSource();
      expect(keyOf(unset?.load())).toBe('v2');
      server.version = 3;
      now += 3_600_000;
      unset?.load();
      await remoteConfigRefreshSettledForTest();
      expect(keyOf(unset?.load())).toBe('v2');
    },
  );

  specTest(
    'sends a background request without blocking the event loop',
    { feature, requirement, check: 'a-stale-load-returns-the-held-tree-and-refreshes' },
    async () => {
      resetSyncFetchForTest();
      const body = JSON.stringify({ ok: true });
      const response = await asyncFetch({ url: `data:application/json,${encodeURIComponent(body)}` });
      expect(response).toEqual({ status: 200, body });
    },
  );

  specTest(
    'resolves the bearer of a refresh without the synchronous token path',
    { feature, requirement, check: 'a-stale-load-returns-the-held-tree-and-refreshes' },
    async () => {
      const server = versionedServer('config');
      const calls = { sync: 0, async: 0 };
      const tokenSource: TokenSource = {
        token: () => {
          calls.sync += 1;
          return 'sync-token';
        },
        tokenAsync: async () => {
          calls.async += 1;
          return 'async-token';
        },
      };
      const config = new RemoteConfigSource({
        serverUrl: 'https://config.putnami.test',
        appName: 'app',
        tokenSource,
        refreshPeriod: 60_000,
      });
      config.load();
      expect(calls).toEqual({ sync: 1, async: 0 });

      now += 60_000;
      config.load();
      await remoteConfigRefreshSettledForTest();
      expect(calls).toEqual({ sync: 1, async: 1 });
      expect(server.requests[1]?.headers?.['Authorization']).toBe('Bearer async-token');

      versionedServer('secrets');
      const secrets = new RemoteSecretsSource({
        serverUrl: 'https://config.putnami.test',
        appName: 'app',
        tokenSource: { token: () => undefined },
        token: 'static-token',
        refreshPeriod: 60_000,
      });
      secrets.load();
      now += 60_000;
      secrets.load();
      await remoteSecretsRefreshSettledForTest();
      expect(keyOf(secrets.load())).toBe('v1');
    },
  );

  specTest(
    'mints a metadata token without blocking the event loop',
    { feature, requirement, check: 'a-stale-load-returns-the-held-tree-and-refreshes' },
    async () => {
      let requests = 0;
      setSyncFetchForTest((request) => {
        requests += 1;
        expect(request.headers?.['Metadata-Flavor']).toBe('Google');
        return { status: 200, body: requests === 1 ? ' minted-token \n' : '' };
      });
      const source = new GcpMetadataTokenSource('https://config.putnami.test');
      expect(await source.tokenAsync()).toBe('minted-token');
      expect(await source.tokenAsync()).toBe('minted-token');
      expect(requests).toBe(1);

      const empty = new GcpMetadataTokenSource('https://other.putnami.test');
      expect(await empty.tokenAsync()).toBeUndefined();

      setSyncFetchForTest(() => {
        throw new Error('metadata unreachable');
      });
      expect(await new GcpMetadataTokenSource('https://third.putnami.test').tokenAsync()).toBeUndefined();
    },
  );

  specTest(
    'keeps the held tree when a refresh answers no tree',
    { feature, requirement, check: 'a-failed-refresh-keeps-the-held-tree' },
    async () => {
      versionedServer('config');
      const source = new RemoteConfigSource({
        serverUrl: 'https://config.putnami.test',
        appName: 'app',
        refreshPeriod: 60_000,
      });
      expect(keyOf(source.load())).toBe('v1');
      setSyncFetchForTest(() => ({ status: 200, body: JSON.stringify({ resolved: true, schemaMatch: true }) }));
      now += 60_000;
      source.load();
      await remoteConfigRefreshSettledForTest();
      expect(keyOf(source.load())).toBe('v1');
    },
  );

  specTest(
    'never refreshes a config its exact URL pins',
    { feature, requirement, check: 'a-pinned-config-is-never-refreshed' },
    async () => {
      const server = versionedServer('config');
      const source = new RemoteConfigSource({
        url: 'https://config.putnami.test/api/configs/resolve?appName=app&version=rev-7&pinned=true',
        appName: 'app',
        refreshPeriod: 60_000,
      });
      source.load();
      server.version = 2;
      now += 3_600_000;
      source.load();
      await remoteConfigRefreshSettledForTest();
      expect(keyOf(source.load())).toBe('v1');
      expect(server.calls).toBe(1);
    },
  );

  specTest(
    'reads a zero refresh period without a warning',
    { feature, requirement, check: 'discovery-reads-the-refresh-period' },
    () => {
      const lines: string[] = [];
      console.log = mock((line?: unknown) => {
        lines.push(String(line ?? ''));
      }) as unknown as typeof console.log;
      process.env['CONFIG_SERVER_URL'] = 'https://config.putnami.test';
      process.env['CONFIG_SERVER_TOKEN'] = 'token';
      process.env['CONFIG_SERVER_REFRESH'] = '0';
      discoverRemoteSource();
      expect(lines.filter((line) => line.includes('invalid config-server duration')).length).toBe(0);
    },
  );
});
