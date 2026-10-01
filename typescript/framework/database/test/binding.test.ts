import { afterEach, describe, expect, it } from 'bun:test';
import { resetConfigLoader } from '@putnami/runtime';
import {
  ENV_DATABASE_BINDINGS,
  parseDatabaseBinding,
  resolveBindingEntry,
  resolveDatabaseBinding,
  setDatabaseBindingOverride,
} from '../src/postgres/binding';
import { __setIdentityResolverForTests, applyConnectionParams, buildOptions } from '../src/postgres/options';

const baseConfig = {
  host: 'localhost',
  port: 5432,
  database: 'app',
  ssl: false,
  poolSize: 10,
  debug: false,
  maxRowLimit: 10_000,
  slowQueryThresholdMs: 0,
  statementTimeoutMs: 30_000,
  idleInTransactionTimeoutMs: 300_000,
  connectTimeout: 10,
  idleTimeout: 60,
  maxLifetime: 0,
};

function binding(databases: Record<string, unknown>): string {
  return JSON.stringify({ protocolVersion: 1, databases });
}

afterEach(() => {
  __setIdentityResolverForTests(null);
  delete process.env[ENV_DATABASE_BINDINGS];
});

describe('parseDatabaseBinding', () => {
  it('accepts a valid multi-datasource binding', () => {
    const doc = parseDatabaseBinding(
      binding({
        auth: { engine: 'postgres', schema: 'iam', connection: { host: 'h', database: 'auth' } },
        billing: { engine: 'postgres', schema: 'billing', connection: { instance: 'p:r:b', database: 'billing' } },
      }),
    );
    expect(Object.keys(doc.databases ?? {}).sort()).toEqual(['auth', 'billing']);
  });

  it('rejects an unsupported protocolVersion', () => {
    expect(() => parseDatabaseBinding(JSON.stringify({ protocolVersion: 2, databases: {} }))).toThrow();
  });

  it('rejects a connection with more than one transport strategy', () => {
    expect(() =>
      parseDatabaseBinding(
        binding({ auth: { engine: 'postgres', schema: 'iam', connection: { dsn: 'postgres://x/a', host: 'h' } } }),
      ),
    ).toThrow();
  });

  it('rejects a dsn connection carrying ssl', () => {
    expect(() =>
      parseDatabaseBinding(
        binding({ auth: { engine: 'postgres', schema: 'iam', connection: { dsn: 'postgres://x/a', ssl: true } } }),
      ),
    ).toThrow();
  });

  it('rejects a non-postgres engine', () => {
    expect(() =>
      parseDatabaseBinding(
        binding({ auth: { engine: 'mysql', schema: 'iam', connection: { host: 'h', database: 'a' } } }),
      ),
    ).toThrow();
  });

  it('rejects a missing schema', () => {
    expect(() =>
      parseDatabaseBinding(binding({ auth: { engine: 'postgres', connection: { host: 'h', database: 'a' } } })),
    ).toThrow();
  });

  it('rejects a non-canonical datasource name', () => {
    expect(() =>
      parseDatabaseBinding(
        binding({ 'Not A Name': { engine: 'postgres', schema: 'iam', connection: { host: 'h', database: 'a' } } }),
      ),
    ).toThrow();
  });
});

describe('resolveBindingEntry', () => {
  it('resolves a structured TCP datasource with ssl and physical database', () => {
    const doc = parseDatabaseBinding(
      binding({
        auth: {
          engine: 'postgres',
          schema: 'iam',
          connection: {
            host: 'db.internal',
            port: 5432,
            database: 'auth_db',
            user: 'app',
            password: 's3cret',
            ssl: true,
          },
        },
      }),
    );
    const resolved = resolveBindingEntry(doc, 'auth');
    expect(resolved.searchPath).toBe('iam');
    expect(resolved.config).toEqual({
      host: 'db.internal',
      port: 5432,
      database: 'auth_db',
      user: 'app',
      password: 's3cret',
      ssl: true,
    });
  });

  it('resolves a Cloud SQL instance to a /cloudsql socket host', () => {
    const doc = parseDatabaseBinding(
      binding({
        billing: {
          engine: 'postgres',
          schema: 'billing',
          connection: { instance: 'my-project:us-central1:billing-db', database: 'billing_db', user: 'app' },
        },
      }),
    );
    const resolved = resolveBindingEntry(doc, 'billing');
    expect(resolved.config.host).toBe('/cloudsql/my-project:us-central1:billing-db');
    expect(resolved.config.database).toBe('billing_db');
    expect(resolved.config.port).toBeUndefined();
  });

  it('parses a DSN into structured config and maps sslmode', () => {
    const doc = parseDatabaseBinding(
      binding({
        events: {
          engine: 'postgres',
          schema: 'events',
          connection: { dsn: 'postgres://app:s3cret@10.0.0.5:5432/analytics?sslmode=require&application_name=svc' },
        },
      }),
    );
    const resolved = resolveBindingEntry(doc, 'events');
    expect(resolved.searchPath).toBe('events');
    expect(resolved.config).toEqual({
      host: '10.0.0.5',
      port: 5432,
      database: 'analytics',
      user: 'app',
      password: 's3cret',
      ssl: true,
    });
    expect(resolved.connectionParams).toEqual({ application_name: 'svc' });
  });

  it('takes the pgx pool parameters out of a DSN and maps the size', () => {
    const doc = parseDatabaseBinding(
      binding({
        primary: {
          engine: 'postgres',
          schema: 'app',
          connection: {
            dsn: 'postgres://u:p@h:5432/db?pool_max_conns=2&pool_min_conns=0&pool_max_conn_idle_time=5s&application_name=svc',
          },
        },
      }),
    );
    const resolved = resolveBindingEntry(doc, 'primary');
    expect(resolved.config.poolSize).toBe(2);
    expect(resolved.connectionParams).toEqual({ application_name: 'svc' });
  });

  it('honors the Cloud SQL ?host= form in a DSN', () => {
    const doc = parseDatabaseBinding(
      binding({
        platform: {
          engine: 'postgres',
          schema: 'public',
          connection: { dsn: 'postgres:///platform?host=/cloudsql/p:r:i' },
        },
      }),
    );
    const resolved = resolveBindingEntry(doc, 'platform');
    expect(resolved.config.host).toBe('/cloudsql/p:r:i');
    expect(resolved.config.database).toBe('platform');
  });

  it('maps sslmode=disable to ssl:false', () => {
    const doc = parseDatabaseBinding(
      binding({
        a: { engine: 'postgres', schema: 's', connection: { dsn: 'postgres://u:p@h:5432/a?sslmode=disable' } },
      }),
    );
    expect(resolveBindingEntry(doc, 'a').config.ssl).toBe(false);
  });

  it('fails for a datasource absent from the binding, listing the available ones', () => {
    const doc = parseDatabaseBinding(
      binding({
        auth: { engine: 'postgres', schema: 'iam', connection: { host: 'h', database: 'auth' } },
        billing: { engine: 'postgres', schema: 'b', connection: { host: 'h', database: 'billing' } },
      }),
    );
    expect(() => resolveBindingEntry(doc, 'ledger')).toThrow(/ledger.*auth.*billing|auth.*billing/);
  });
});

describe('resolveDatabaseBinding (env)', () => {
  it('returns undefined when DATABASE_BINDINGS is unset', () => {
    expect(resolveDatabaseBinding('auth', undefined)).toBeUndefined();
    expect(resolveDatabaseBinding('auth', '   ')).toBeUndefined();
  });

  it('reads the injected binding from the environment', () => {
    process.env[ENV_DATABASE_BINDINGS] = binding({
      auth: {
        engine: 'postgres',
        schema: 'iam',
        connection: { host: 'db', port: 5432, database: 'auth', user: 'u', password: 'p', ssl: false },
      },
    });
    const resolved = resolveDatabaseBinding('auth');
    expect(resolved?.searchPath).toBe('iam');
    expect(resolved?.config.host).toBe('db');
  });
});

describe('resolveDatabaseBinding (config section)', () => {
  // The managed document as the control plane's resolve overlay merges it into
  // the workload's `database` config section: protocol keys next to
  // non-colliding operator-authored keys.
  const managedSection = {
    operatorKey: 'keep-me',
    protocolVersion: 1,
    databases: {
      auth: {
        engine: 'postgres',
        schema: 'iam',
        connection: { host: 'cfg-db', port: 5432, database: 'auth_cfg', user: 'cu', password: 'cp', ssl: false },
      },
    },
  };

  function setConfigSection(database: unknown): void {
    process.env.CONFIG_DATA = JSON.stringify({ database });
    resetConfigLoader();
  }

  afterEach(() => {
    delete process.env.CONFIG_DATA;
    resetConfigLoader();
  });

  it('wins over DATABASE_BINDINGS when both are present', () => {
    setConfigSection(managedSection);
    process.env[ENV_DATABASE_BINDINGS] = binding({
      auth: { engine: 'postgres', schema: 'iam_env', connection: { host: 'env-db', database: 'auth_env' } },
    });

    const resolved = resolveDatabaseBinding('auth');
    expect(resolved?.config.host).toBe('cfg-db');
    expect(resolved?.config.database).toBe('auth_cfg');
    expect(resolved?.searchPath).toBe('iam');
  });

  it('is authoritative: a datasource missing from the config document never falls through to env', () => {
    setConfigSection({
      protocolVersion: 1,
      databases: { billing: { engine: 'postgres', schema: 'billing', connection: { dsn: 'postgres://cfg/billing' } } },
    });
    process.env[ENV_DATABASE_BINDINGS] = binding({
      auth: { engine: 'postgres', schema: 'iam', connection: { host: 'env-db', database: 'auth_env' } },
    });

    expect(() => resolveDatabaseBinding('auth')).toThrow(/no binding for datasource "auth".*billing/);
  });

  it('falls back to env when the section carries no databases key', () => {
    setConfigSection({ operatorKey: 'keep-me', default: { host: 'localhost', database: 'app' } });
    process.env[ENV_DATABASE_BINDINGS] = binding({
      auth: { engine: 'postgres', schema: 'iam', connection: { host: 'env-db', database: 'auth_env' } },
    });

    const resolved = resolveDatabaseBinding('auth');
    expect(resolved?.config.host).toBe('env-db');
  });

  it('falls back to env when the databases key is empty', () => {
    setConfigSection({ protocolVersion: 1, databases: {} });
    process.env[ENV_DATABASE_BINDINGS] = binding({
      auth: { engine: 'postgres', schema: 'iam', connection: { host: 'env-db', database: 'auth_env' } },
    });

    expect(resolveDatabaseBinding('auth')?.config.host).toBe('env-db');
  });

  it('fails loudly on a non-object databases key instead of degrading to env', () => {
    setConfigSection({ protocolVersion: 1, databases: 'not-an-object' });
    process.env[ENV_DATABASE_BINDINGS] = binding({
      auth: { engine: 'postgres', schema: 'iam', connection: { host: 'env-db', database: 'auth_env' } },
    });

    expect(() => resolveDatabaseBinding('auth')).toThrow(/malformed managed binding/);
  });

  it('applies the same protocol validation as the env transport', () => {
    setConfigSection({ ...managedSection, protocolVersion: 2 });
    expect(() => resolveDatabaseBinding('auth')).toThrow(/unsupported protocolVersion 2/);
  });

  it('lets an explicitly passed raw document bypass the config transport', () => {
    setConfigSection(managedSection);
    const raw = binding({
      auth: { engine: 'postgres', schema: 'iam_raw', connection: { host: 'raw-db', database: 'auth_raw' } },
    });

    const resolved = resolveDatabaseBinding('auth', raw);
    expect(resolved?.config.host).toBe('raw-db');
    expect(resolved?.searchPath).toBe('iam_raw');
  });

  it('a pinned override (the test provider) beats both the config section and env', () => {
    setConfigSection(managedSection);
    process.env[ENV_DATABASE_BINDINGS] = binding({
      auth: { engine: 'postgres', schema: 'iam_env', connection: { host: 'env-db', database: 'auth_env' } },
    });
    const prev = setDatabaseBindingOverride(
      binding({
        auth: { engine: 'postgres', schema: 'iam_iso', connection: { host: 'iso-db', database: 'auth_iso_4f' } },
      }),
    );
    try {
      expect(prev).toBeUndefined();
      const resolved = resolveDatabaseBinding('auth');
      expect(resolved?.config.host).toBe('iso-db');
      expect(resolved?.config.database).toBe('auth_iso_4f');
      expect(resolved?.searchPath).toBe('iam_iso');
    } finally {
      setDatabaseBindingOverride(prev);
    }

    // Cleared override → the config section is back in charge.
    expect(resolveDatabaseBinding('auth')?.config.host).toBe('cfg-db');
  });
});

describe('binding → postgres.js options', () => {
  it('applies the owning schema as the session search_path', async () => {
    const resolved = resolveDatabaseBinding(
      'auth',
      binding({
        auth: {
          engine: 'postgres',
          schema: 'iam',
          connection: { host: 'db.internal', database: 'auth_db', user: 'app', password: 'p', ssl: true },
        },
      }),
    );
    expect(resolved).toBeDefined();
    const options = await buildOptions({ ...baseConfig, ...resolved?.config });
    applyConnectionParams(options, resolved?.searchPath, resolved?.connectionParams);
    expect((options.connection as Record<string, string>)['search_path']).toBe('iam');
    expect(options.host).toBe('db.internal');
    expect(options.ssl).toBe('require');
  });

  // dbtestenv puts pool_max_conns / pool_min_conns / pool_max_conn_idle_time in
  // every test binding, for Go and TypeScript projects alike. postgres.js turns
  // `connection` into the STARTUP packet, and PostgreSQL refuses a startup
  // parameter it does not know, so leaving them there would fail every
  // connection this adapter opens with
  // `unrecognized configuration parameter "pool_max_conns"`.
  it('keeps the pgx pool parameters out of the connection startup params', async () => {
    const resolved = resolveDatabaseBinding(
      'primary',
      binding({
        primary: {
          engine: 'postgres',
          schema: 'app',
          connection: {
            host: 'db.internal',
            database: 'app_test',
            user: 'u',
            password: 'p',
            params: {
              pool_max_conns: '2',
              pool_min_conns: '0',
              pool_max_conn_idle_time: '5s',
              sslmode: 'disable',
            },
          },
        },
      }),
    );
    const options = await buildOptions({ ...baseConfig, ...resolved?.config });
    applyConnectionParams(options, resolved?.searchPath, resolved?.connectionParams);
    const connection = options.connection as Record<string, string>;
    for (const key of ['pool_max_conns', 'pool_min_conns', 'pool_max_conn_idle_time']) {
      expect(connection[key]).toBeUndefined();
    }
    // An unrelated driver option still travels, and the size the connection
    // asked for reaches postgres.js as the pool ceiling.
    expect(connection['sslmode']).toBe('disable');
    expect(options.max).toBe(2);
  });

  it('carries extra DSN params (application_name) into connection startup params', async () => {
    const resolved = resolveDatabaseBinding(
      'events',
      binding({
        events: {
          engine: 'postgres',
          schema: 'events',
          connection: { dsn: 'postgres://u:p@h:5432/db?application_name=svc' },
        },
      }),
    );
    const options = await buildOptions({ ...baseConfig, ...resolved?.config });
    applyConnectionParams(options, resolved?.searchPath, resolved?.connectionParams);
    const connection = options.connection as Record<string, string>;
    expect(connection['application_name']).toBe('svc');
    expect(connection['search_path']).toBe('events');
  });
});
