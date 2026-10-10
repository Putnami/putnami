// biome-ignore-all lint/suspicious/noConsole: Captures JsonSink output written through console.log.
import { afterEach, beforeEach, describe, expect, it, mock } from 'bun:test';
import { resetConfigLoader } from '@putnami/runtime';
import {
  discoverRemoteSource,
  RemoteConfigError,
  RemoteConfigSource,
  resetRemoteConfigSourceCacheForTest,
} from '../src/runtime/remote-config-source';
import { setSyncFetchForTest, type SyncFetchRequest } from '../src/runtime/sync-fetch';
import { resetDefaultLogger } from '@putnami/runtime';
import { resetLoggerConfig } from '@putnami/runtime';

describe('RemoteConfigSource', () => {
  const originalConsoleLog = console.log;
  const remoteEnvNames = [
    'CONFIG_SERVER_URL',
    'CONFIG_SERVER_TOKEN',
    'CONFIG_SERVER_AUDIENCE',
    'CONFIG_SERVER_REQUIRED',
    'CONFIG_SERVER_TIMEOUT',
    'CONFIG_SERVER_RETRY_BUDGET',
    'APP_NAME',
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
    resetRemoteConfigSourceCacheForTest();
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
    resetRemoteConfigSourceCacheForTest();
    resetLoggerConfig();
    resetDefaultLogger();
  });

  it('logs schema mismatch and server warnings without dropping the resolved config', () => {
    const lines: string[] = [];
    console.log = mock((line?: unknown) => {
      lines.push(String(line ?? ''));
    }) as unknown as typeof console.log;

    setSyncFetchForTest(() => ({
      status: 200,
      body: JSON.stringify({
        config: { server: { port: 3000 } },
        resolved: true,
        schemaMatch: false,
        warnings: ['missing required field: database.url'],
      }),
    }));

    const source = new RemoteConfigSource({
      serverUrl: 'https://config.putnami.test',
      appName: 'task-api',
      environment: 'production',
    });

    expect(source.load()).toEqual({ server: { port: 3000 } });

    const entries = lines.map((line) => JSON.parse(line) as Record<string, unknown>);
    expect(entries).toHaveLength(2);
    expect(entries[0]).toMatchObject({
      severity: 'WARNING',
      logger: 'config-server',
      message: 'config-server schema mismatch',
      serverUrl: 'https://config.putnami.test',
      appName: 'task-api',
      environment: 'production',
    });
    expect(entries[1]).toMatchObject({
      severity: 'WARNING',
      logger: 'config-server',
      message: 'config-server warning',
      serverUrl: 'https://config.putnami.test',
      appName: 'task-api',
      environment: 'production',
      warning: 'missing required field: database.url',
    });
  });

  it('passes Authorization: Bearer <token> to the in-process fetch', () => {
    let captured: SyncFetchRequest | undefined;
    setSyncFetchForTest((request) => {
      captured = request;
      return {
        status: 200,
        body: JSON.stringify({ config: {}, resolved: true, schemaMatch: true }),
      };
    });

    const source = new RemoteConfigSource({
      serverUrl: 'https://config.putnami.test',
      appName: 'task-api',
      environment: 'production',
      token: 'test-token',
    });
    source.load();

    expect(captured?.url).toBe('https://config.putnami.test/api/configs/resolve');
    expect(captured?.method).toBe('POST');
    expect(captured?.headers?.Authorization).toBe('Bearer test-token');
  });

  it('TokenSource takes precedence over the static token field', () => {
    // The resolver is the seam; the static token is a convenience for direct
    // callers.
    let captured: SyncFetchRequest | undefined;
    setSyncFetchForTest((request) => {
      captured = request;
      return {
        status: 200,
        body: JSON.stringify({ config: {}, resolved: true, schemaMatch: true }),
      };
    });

    new RemoteConfigSource({
      serverUrl: 'https://config.putnami.test',
      appName: 'task-api',
      environment: 'production',
      token: 'from-static-field',
      tokenSource: { token: () => 'from-token-source' },
    }).load();

    expect(captured?.headers?.Authorization).toBe('Bearer from-token-source');
  });

  it('omits the Authorization header when no token is configured', () => {
    let captured: SyncFetchRequest | undefined;
    setSyncFetchForTest((request) => {
      captured = request;
      return {
        status: 200,
        body: JSON.stringify({ config: {}, resolved: true, schemaMatch: true }),
      };
    });

    new RemoteConfigSource({
      serverUrl: 'https://config.putnami.test',
      appName: 'task-api',
      environment: 'production',
    }).load();

    expect(captured?.headers?.Authorization).toBeUndefined();
  });

  it('discoverRemoteSource forwards CONFIG_SERVER_TOKEN to the bearer header', () => {
    // Same identity must reach /api/configs/resolve as reaches /api/secrets/resolve.
    let captured: SyncFetchRequest | undefined;
    setSyncFetchForTest((request) => {
      captured = request;
      return {
        status: 200,
        body: JSON.stringify({ config: {}, resolved: true, schemaMatch: true }),
      };
    });

    const original = {
      url: process.env.CONFIG_SERVER_URL,
      token: process.env.CONFIG_SERVER_TOKEN,
      app: process.env.APP_NAME,
    };
    process.env.CONFIG_SERVER_URL = 'https://config.putnami.test';
    process.env.CONFIG_SERVER_TOKEN = 'discovered-token';
    process.env.APP_NAME = 'task-api';
    try {
      const source = discoverRemoteSource();
      expect(source).toBeDefined();
      source?.load();
      expect(captured?.headers?.Authorization).toBe('Bearer discovered-token');
    } finally {
      process.env.CONFIG_SERVER_URL = original.url;
      process.env.CONFIG_SERVER_TOKEN = original.token;
      process.env.APP_NAME = original.app;
    }
  });

  it('fetches an exact CONFIG_SERVER_URL with GET and preserves its query string', () => {
    let captured: SyncFetchRequest | undefined;
    setSyncFetchForTest((request) => {
      captured = request;
      return {
        status: 200,
        body: JSON.stringify({
          config: { google: { clientId: 'id', clientSecret: 'secret' } },
          resolved: true,
          schemaMatch: true,
          secretsApplied: true,
        }),
      };
    });

    const exactURL =
      'https://api.putnami.test/api/configs/resolve?appName=auth%2Fserver&environment=prod&secretsMode=reveal';
    const source = new RemoteConfigSource({
      url: exactURL,
      appName: 'auth/server',
      environment: 'prod',
    });

    expect(source.load()).toEqual({ google: { clientId: 'id', clientSecret: 'secret' } });
    expect(captured?.url).toBe(exactURL);
    expect(captured?.method).toBe('GET');
    expect(captured?.body).toBeUndefined();
  });

  it('retries a stale raw config URL through CONFIG_SERVER_AUDIENCE after a 404', () => {
    const staleURL =
      'https://config-api-abc123-ew.a.run.app/api/configs/resolve?appName=orders%2Fapi&environment=prod&secretsMode=reveal';
    const expectedFallback =
      'https://api.putnami.test/api/configs/resolve?appName=orders%2Fapi&environment=prod&secretsMode=reveal';
    const requests: SyncFetchRequest[] = [];
    setSyncFetchForTest((request) => {
      requests.push(request);
      if (request.url === staleURL) {
        return { status: 404, body: 'not found' };
      }
      if (request.url === expectedFallback) {
        return {
          status: 200,
          body: JSON.stringify({ config: { database: { name: 'marketing' } }, resolved: true, schemaMatch: true }),
        };
      }
      throw new Error(`unexpected request: ${request.url}`);
    });
    process.env.CONFIG_SERVER_URL = staleURL;
    process.env.CONFIG_SERVER_AUDIENCE = 'https://api.putnami.test';
    process.env.CONFIG_SERVER_TOKEN = 'discovered-token';
    process.env.APP_NAME = 'orders/api';

    expect(discoverRemoteSource()?.load()).toEqual({ database: { name: 'marketing' } });
    expect(requests).toHaveLength(2);
    expect(requests.map((request) => request.url)).toEqual([staleURL, expectedFallback]);
    expect(requests.map((request) => request.method)).toEqual(['GET', 'GET']);
    expect(requests[1]?.headers?.Authorization).toBe('Bearer discovered-token');
  });

  it('appends version and pinned to the exact-URL GET query', () => {
    let captured: SyncFetchRequest | undefined;
    setSyncFetchForTest((request) => {
      captured = request;
      return { status: 200, body: JSON.stringify({ config: { a: 1 }, resolved: true, schemaMatch: true }) };
    });

    new RemoteConfigSource({
      url: 'https://api.putnami.test/api/configs/resolve?appName=auth&environment=prod&secretsMode=reveal',
      appName: 'auth',
      environment: 'prod',
      version: 'rel_pin1',
      pinned: true,
    }).load();

    expect(captured?.method).toBe('GET');
    const q = new URL(captured?.url ?? '').searchParams;
    expect(q.get('version')).toBe('rel_pin1');
    expect(q.get('pinned')).toBe('true');
  });

  it('carries version and pinned in the POST body', () => {
    let captured: SyncFetchRequest | undefined;
    setSyncFetchForTest((request) => {
      captured = request;
      return { status: 200, body: JSON.stringify({ config: { a: 1 }, resolved: true, schemaMatch: true }) };
    });

    new RemoteConfigSource({
      serverUrl: 'https://config.putnami.test',
      appName: 'auth',
      environment: 'prod',
      version: 'rel_pin2',
      pinned: true,
    }).load();

    expect(captured?.method).toBe('POST');
    const body = JSON.parse(captured?.body ?? '{}');
    expect(body.version).toBe('rel_pin2');
    expect(body.pinned).toBe(true);
  });

  it('a bare version without pinned stays additive (no pinned sent)', () => {
    let captured: SyncFetchRequest | undefined;
    setSyncFetchForTest((request) => {
      captured = request;
      return { status: 200, body: JSON.stringify({ config: { a: 1 }, resolved: true, schemaMatch: true }) };
    });

    new RemoteConfigSource({
      serverUrl: 'https://config.putnami.test',
      appName: 'auth',
      environment: 'prod',
      version: '1.2.3',
    }).load();

    expect(JSON.parse(captured?.body ?? '{}').pinned).toBeUndefined();
  });

  it('discoverRemoteSource pins from CONFIG_VERSION only, never APP_VERSION', () => {
    // APP_VERSION carries the build semver at runtime; a pinned resolve is
    // exclusive + fail-loud, so only the deliberately-stamped CONFIG_VERSION may
    // become the resolve version. APP_VERSION alone must leave the request
    // unpinned.
    let captured: SyncFetchRequest | undefined;
    setSyncFetchForTest((request) => {
      captured = request;
      return { status: 200, body: JSON.stringify({ config: {}, resolved: true, schemaMatch: true }) };
    });

    const original = {
      url: process.env.CONFIG_SERVER_URL,
      app: process.env.APP_NAME,
      configVersion: process.env.CONFIG_VERSION,
      appVersion: process.env.APP_VERSION,
    };
    process.env.CONFIG_SERVER_URL = 'https://config.putnami.test';
    process.env.APP_NAME = 'auth';
    process.env.APP_VERSION = '1.2.3';
    try {
      process.env.CONFIG_VERSION = 'rel_pin';
      discoverRemoteSource()?.load();
      {
        const body = JSON.parse(captured?.body ?? '{}');
        expect(body.version).toBe('rel_pin');
        expect(body.pinned).toBe(true);
      }

      delete process.env.CONFIG_VERSION;
      captured = undefined;
      resetRemoteConfigSourceCacheForTest();
      discoverRemoteSource()?.load();
      {
        const body = JSON.parse(captured?.body ?? '{}');
        expect(body.version).toBeUndefined();
        expect(body.pinned).toBeUndefined();
      }
    } finally {
      process.env.CONFIG_SERVER_URL = original.url;
      process.env.APP_NAME = original.app;
      if (original.configVersion === undefined) delete process.env.CONFIG_VERSION;
      else process.env.CONFIG_VERSION = original.configVersion;
      if (original.appVersion === undefined) delete process.env.APP_VERSION;
      else process.env.APP_VERSION = original.appVersion;
    }
  });

  it('discoverRemoteSource uses the exact CONFIG_SERVER_URL contract', () => {
    let captured: SyncFetchRequest | undefined;
    setSyncFetchForTest((request) => {
      captured = request;
      return {
        status: 200,
        body: JSON.stringify({ config: { google: { clientId: 'id' } }, resolved: true, schemaMatch: true }),
      };
    });

    const exactURL =
      'https://api.putnami.test/api/configs/resolve?appName=auth%2Fserver&environment=prod&secretsMode=reveal';
    process.env.CONFIG_SERVER_URL = exactURL;
    process.env.CONFIG_SERVER_TOKEN = 'discovered-token';
    process.env.APP_NAME = 'auth/server';

    const source = discoverRemoteSource();
    expect(source).toBeDefined();
    expect(source?.load()).toEqual({ google: { clientId: 'id' } });
    expect(captured?.url).toBe(exactURL);
    expect(captured?.method).toBe('GET');
  });

  it('treats blank and orchestration sentinel CONFIG_SERVER_URL values as unset', () => {
    for (const value of ['', '   ', 'undefined', ' UnDeFiNeD ', 'null']) {
      process.env.CONFIG_SERVER_URL = value;
      expect(discoverRemoteSource()).toBeUndefined();
    }
  });

  it('treats an empty schema-matching unresolved response as empty config', () => {
    setSyncFetchForTest(() => ({
      status: 200,
      body: JSON.stringify({ config: {}, resolved: false, schemaMatch: true }),
    }));

    const source = new RemoteConfigSource({
      url: 'https://api.putnami.test/api/configs/resolve?appName=docs&environment=prod&secretsMode=reveal',
      appName: 'docs',
      environment: 'prod',
      required: true,
    });

    expect(source.load()).toEqual({});
  });

  it('preserves unresolved-response diagnostics for required config, including cache hits', () => {
    let calls = 0;
    setSyncFetchForTest(() => {
      calls += 1;
      return {
        status: 200,
        body: JSON.stringify({
          config: {},
          resolved: false,
          schemaMatch: true,
          warnings: ['missing required field: database.url'],
        }),
      };
    });

    const source = () =>
      new RemoteConfigSource({
        serverUrl: 'https://config.putnami.test',
        appName: 'task-api',
        environment: 'production',
        token: 'operator-token',
        required: true,
      });

    const errors: unknown[] = [];
    for (const candidate of [source(), source()]) {
      try {
        candidate.load();
      } catch (error) {
        errors.push(error);
      }
    }

    expect(errors).toHaveLength(2);
    for (const error of errors) {
      expect(error).toBeInstanceOf(RemoteConfigError);
      expect((error as RemoteConfigError).message).toBe('config-server did not resolve config');
      expect((error as RemoteConfigError).url).toBe('https://config.putnami.test/api/configs/resolve');
      expect((error as RemoteConfigError).method).toBe('POST');
      expect((error as RemoteConfigError).statusCode).toBe(200);
      expect((error as RemoteConfigError).authPresent).toBe(true);
      expect((error as RemoteConfigError).authMode).toBe('bearer');
      expect((error as RemoteConfigError).appName).toBe('task-api');
      expect((error as RemoteConfigError).environment).toBe('production');
    }
    expect(calls).toBe(1);
  });

  it('retries required remote config until the server is ready', () => {
    let calls = 0;
    setSyncFetchForTest(() => {
      calls += 1;
      if (calls === 1) {
        throw new Error('connection refused');
      }
      return {
        status: 200,
        body: JSON.stringify({ config: { server: { port: 4100 } }, resolved: true, schemaMatch: true }),
      };
    });

    const source = new RemoteConfigSource({
      serverUrl: 'https://config.putnami.test',
      appName: 'task-api',
      environment: 'production',
      timeout: 50,
      retryBudget: 500,
      required: true,
    });

    expect(source.load()).toEqual({ server: { port: 4100 } });
    expect(calls).toBe(2);
  });

  it('does not retry terminal HTTP status responses', () => {
    let calls = 0;
    let captured: SyncFetchRequest | undefined;
    setSyncFetchForTest((request) => {
      calls += 1;
      captured = request;
      return { status: 401, body: 'unauthorized' };
    });

    const source = new RemoteConfigSource({
      serverUrl: 'https://config.putnami.test',
      appName: 'task-api',
      environment: 'production',
      timeout: 50,
      retryBudget: 500,
      required: true,
    });

    let thrown: unknown;
    try {
      source.load();
    } catch (error) {
      thrown = error;
    }

    expect(thrown).toBeInstanceOf(RemoteConfigError);
    expect((thrown as RemoteConfigError).message).toBe('config-server returned non-200: status 401');
    expect((thrown as RemoteConfigError).cause).toBeInstanceOf(Error);
    expect(((thrown as RemoteConfigError).cause as Error).message).toBe('status 401');
    expect((thrown as RemoteConfigError).url).toBe('https://config.putnami.test/api/configs/resolve');
    expect((thrown as RemoteConfigError).method).toBe('POST');
    expect((thrown as RemoteConfigError).statusCode).toBe(401);
    expect((thrown as RemoteConfigError).authPresent).toBe(false);
    expect((thrown as RemoteConfigError).authMode).toBe('none');
    expect((thrown as RemoteConfigError).appName).toBe('task-api');
    expect((thrown as RemoteConfigError).environment).toBe('production');
    expect(calls).toBe(1);
    expect(captured?.throwOnHttpError).toBe(false);
  });

  it('discoverRemoteSource reads timeout and retry budget env vars', () => {
    process.env.CONFIG_SERVER_URL = 'https://config.putnami.test';
    process.env.CONFIG_SERVER_TOKEN = 'discovered-token';
    process.env.CONFIG_SERVER_TIMEOUT = '1500ms';
    process.env.CONFIG_SERVER_RETRY_BUDGET = '2s';

    let captured: SyncFetchRequest | undefined;
    setSyncFetchForTest((request) => {
      captured = request;
      return {
        status: 200,
        body: JSON.stringify({ config: {}, resolved: true, schemaMatch: true }),
      };
    });

    const source = discoverRemoteSource();
    expect(source).toBeDefined();
    source?.load();
    expect(captured?.timeoutMs).toBe(1500);
  });

  it('throws on remote fetch failure when discovered in Cloud Run', () => {
    let calls = 0;
    setSyncFetchForTest(() => {
      calls += 1;
      throw new Error('network down');
    });
    process.env.CONFIG_SERVER_URL =
      'https://api.putnami.test/api/configs/resolve?appName=auth%2Fserver&environment=prod&secretsMode=reveal';
    process.env.CONFIG_SERVER_TOKEN = 'discovered-token';
    process.env.CONFIG_SERVER_RETRY_BUDGET = '0';
    process.env.K_SERVICE = 'auth-server';

    const source = discoverRemoteSource();
    expect(source).toBeDefined();
    expect(() => source?.load()).toThrow(/config-server fetch failed: network down/);
    expect(calls).toBe(1);
  });

  it('treats NODE_ENV=production as a required remote source', () => {
    let calls = 0;
    setSyncFetchForTest(() => {
      calls += 1;
      throw new Error('network down');
    });
    process.env.CONFIG_SERVER_URL =
      'https://api.putnami.test/api/configs/resolve?appName=auth%2Fserver&environment=prod&secretsMode=reveal';
    process.env.CONFIG_SERVER_TOKEN = 'discovered-token';
    process.env.CONFIG_SERVER_RETRY_BUDGET = '0';
    process.env.NODE_ENV = 'production';

    const source = discoverRemoteSource();
    expect(source).toBeDefined();
    expect(() => source?.load()).toThrow(/config-server fetch failed: network down/);
    expect(calls).toBe(1);
  });

  it('resolves environment=production against a canonical-prod server (server normalizes)', () => {
    // The server canonicalizes environment naming at its boundary
    // (trim + lowercase + production → prod); the client sends whatever it
    // was configured with, verbatim. This pins that a TS workload started
    // with APP_ENV=production keeps config access against a canonical-prod
    // environment without any client release.
    const canonicalEnv = (env: string): string => {
      const normalized = env.trim().toLowerCase();
      return normalized === 'production' ? 'prod' : normalized;
    };
    let sentEnvironment: string | undefined;
    setSyncFetchForTest((request) => {
      const body = JSON.parse(request.body ?? '{}') as { environment?: string };
      sentEnvironment = body.environment;
      if (canonicalEnv(body.environment ?? '') !== 'prod') {
        return { status: 403, body: JSON.stringify({ error: 'forbidden' }) };
      }
      return {
        status: 200,
        body: JSON.stringify({
          config: { server: { host: 'prod-host' } },
          resolved: true,
          schemaMatch: true,
        }),
      };
    });

    const source = new RemoteConfigSource({
      serverUrl: 'https://config.putnami.test',
      appName: 'task-api',
      environment: 'production',
    });
    expect(source.load()).toEqual({ server: { host: 'prod-host' } });
    // No client-side canonicalization: the wire carries the configured
    // spelling and the server owns the alias mapping.
    expect(sentEnvironment).toBe('production');
  });
});
