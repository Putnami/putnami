import { afterEach, describe, expect, it } from 'bun:test';
import { type LogEntry, type LogSink, Logger, REDACTED, resetDefaultLogger, setRootLogger } from '@putnami/runtime';
import { __setIdentityResolverForTests, __setTokenFetcherForTests, buildOptions } from '../src/postgres/options';

const baseConfig = {
  host: 'localhost',
  port: 5432,
  database: 'app',
  ssl: false,
  poolSize: 10,
  debug: false,
  slowQueryThresholdMs: 0,
  statementTimeoutMs: 30_000,
  idleInTransactionTimeoutMs: 300_000,
  connectTimeout: 10,
  idleTimeout: 60,
  maxLifetime: 0,
};

afterEach(() => {
  __setTokenFetcherForTests(null);
  __setIdentityResolverForTests(null);
});

describe('buildOptions', () => {
  it('forwards poolSize as max to the postgres driver options', async () => {
    const opts = await buildOptions({
      ...baseConfig,
      host: 'db.example.com',
      port: 5432,
      user: 'app',
      password: 'secret',
      poolSize: 25,
    });
    expect(opts.max).toBe(25);
  });

  it('requires SSL by default for a remote TCP host', async () => {
    const opts = await buildOptions({
      ...baseConfig,
      ssl: undefined,
      host: 'db.example.com',
      user: 'app',
      password: 'secret',
    });
    expect(opts.ssl).toBe('require');
  });

  it('skips SSL by default for loopback and unix-socket hosts', async () => {
    expect((await buildOptions({ ...baseConfig, ssl: undefined, host: 'localhost', user: 'app' })).ssl).toBeUndefined();
    expect((await buildOptions({ ...baseConfig, ssl: undefined, host: '127.0.0.1', user: 'app' })).ssl).toBeUndefined();
    expect(
      (await buildOptions({ ...baseConfig, ssl: undefined, host: '/cloudsql/p:r:i', user: 'app' })).ssl,
    ).toBeUndefined();
  });

  it('honors an explicit ssl override over the host default', async () => {
    const remoteOff = await buildOptions({
      ...baseConfig,
      host: 'db.example.com',
      ssl: false,
      user: 'app',
      password: 'x',
    });
    expect(remoteOff.ssl).toBeUndefined();
    const localOn = await buildOptions({ ...baseConfig, host: 'localhost', ssl: true, user: 'app' });
    expect(localOn.ssl).toBe('require');
  });

  it('passes through host, user, password when all are explicit', async () => {
    const opts = await buildOptions({
      ...baseConfig,
      host: 'db.example.com',
      port: 6543,
      user: 'app',
      password: 'secret',
    });
    expect(opts.host).toBe('db.example.com');
    expect(opts.port).toBe(6543);
    expect(opts.username).toBe('app');
    expect(opts.password).toBe('secret');
  });

  it('omits port when host is a Unix socket', async () => {
    __setTokenFetcherForTests(async () => 'tok');
    const opts = await buildOptions({
      ...baseConfig,
      host: '/cloudsql/p:r:i',
      user: 'sa@p.iam',
    });
    expect(opts.host).toBe('/cloudsql/p:r:i');
    expect(opts.port).toBeUndefined();
  });

  it('uses an IAM token thunk as password when password is empty AND host is a Unix socket', async () => {
    __setTokenFetcherForTests(async () => 'fake-iam-token');

    const opts = await buildOptions({
      ...baseConfig,
      host: '/cloudsql/p:r:i',
      user: 'sa@p.iam.gserviceaccount.com',
    });

    expect(opts.username).toBe('sa@p.iam');
    expect(typeof opts.password).toBe('function');
    if (typeof opts.password === 'function') {
      expect(await opts.password()).toBe('fake-iam-token');
    }
  });

  it('pins password to an empty thunk when host is TCP and password is empty (blocks PGPASSWORD env fallback)', async () => {
    let tokenCalled = 0;
    __setTokenFetcherForTests(async () => {
      tokenCalled++;
      return 'tok';
    });

    const opts = await buildOptions({
      ...baseConfig,
      host: 'localhost',
      port: 6543,
      user: 'user@example.com',
    });

    expect(opts.username).toBe('user@example.com');
    expect(typeof opts.password).toBe('function');
    if (typeof opts.password === 'function') {
      // Returns empty string — blocks postgres.js's PGPASSWORD env fallback
      // without contributing a real password to a challenge.
      expect(await opts.password()).toBe('');
    }
    expect(tokenCalled).toBe(0);
  });

  it('auto-resolves user from the identity resolver when user is empty', async () => {
    __setIdentityResolverForTests(async () => 'auto@p.iam.gserviceaccount.com');
    const opts = await buildOptions({
      ...baseConfig,
      host: 'localhost',
      port: 6543,
    });
    expect(opts.username).toBe('auto@p.iam');
  });

  it('keeps non-SA emails (dev laptop) as-is', async () => {
    __setIdentityResolverForTests(async () => 'user@example.com');
    const opts = await buildOptions({
      ...baseConfig,
      host: 'localhost',
      port: 6543,
    });
    expect(opts.username).toBe('user@example.com');
  });

  it('surfaces a descriptive error when identity resolution fails', async () => {
    __setIdentityResolverForTests(async () => {
      throw new Error('no metadata, no gcloud');
    });
    await expect(
      buildOptions({
        ...baseConfig,
        host: 'localhost',
        port: 6543,
      }),
    ).rejects.toThrow('no metadata, no gcloud');
  });

  it('emits a fresh token on each password() invocation', async () => {
    let counter = 0;
    __setTokenFetcherForTests(async () => `token-${++counter}`);

    const opts = await buildOptions({
      ...baseConfig,
      host: '/cloudsql/p:r:i',
      user: 'sa@p.iam',
    });

    if (typeof opts.password !== 'function') throw new Error('password should be a thunk');
    expect(await opts.password()).toBe('token-1');
    expect(await opts.password()).toBe('token-2');
  });

  it('wraps token-fetch errors with a descriptive message', async () => {
    __setTokenFetcherForTests(async () => {
      throw new Error('metadata server unreachable');
    });

    const opts = await buildOptions({
      ...baseConfig,
      host: '/cloudsql/p:r:i',
      user: 'sa@p.iam',
    });

    if (typeof opts.password !== 'function') throw new Error('password should be a thunk');
    await expect(opts.password()).rejects.toThrow('metadata server unreachable');
  });
});

describe('debug query logging', () => {
  const entries: LogEntry[] = [];
  const captureSink: LogSink = {
    write(entry) {
      entries.push(entry);
    },
  };

  afterEach(() => {
    entries.length = 0;
    resetDefaultLogger();
  });

  it('redacts all parameter values, keeping query and types', async () => {
    // Route logging through a capturing sink. skipLevelCheck=true emits debug
    // entries regardless of the ambient log level.
    setRootLogger(new Logger([captureSink], undefined, {}, true));

    const opts = await buildOptions({ ...baseConfig, debug: true, user: 'app', password: 'pw' });
    expect(typeof opts.debug).toBe('function');

    // Simulate postgres.js invoking the debug hook with a query that carries a
    // plaintext secret (e.g. a password column value) in its parameters.
    (opts.debug as (c: number, q: string, p: unknown[], t: unknown[]) => void)(
      1,
      'INSERT INTO users (email, password) VALUES ($1, $2)',
      ['alice@example.com', 'super-secret-password'],
      [25, 25],
    );

    const debugEntries = entries.filter((e) => e.message === 'postgres query');
    expect(debugEntries).toHaveLength(1);
    // The structured payload is logged as the first data param.
    const data = (debugEntries[0].data?.[0] ?? {}) as {
      query?: string;
      parameters?: unknown[];
      paramTypes?: unknown[];
    };
    // Query text and types are preserved for debugging.
    expect(data.query).toContain('INSERT INTO users');
    expect(data.paramTypes).toEqual([25, 25]);
    // Every parameter value is redacted — no plaintext secret reaches the sink.
    expect(data.parameters).toEqual([REDACTED, REDACTED]);
    expect(JSON.stringify(debugEntries[0])).not.toContain('super-secret-password');
    expect(JSON.stringify(debugEntries[0])).not.toContain('alice@example.com');
  });

  it('does not install a debug hook when debug is false', async () => {
    const opts = await buildOptions({ ...baseConfig, debug: false, user: 'app', password: 'pw' });
    expect(opts.debug).toBeUndefined();
  });
});
