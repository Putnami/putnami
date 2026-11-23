import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/spectest';
import { resetConfigLoader } from '@putnami/runtime';
import type { SQL } from 'bun';
import type { ConnectionOptions } from '../src/postgres/options';
import { effectiveConfig } from '../src/datasource-config';
import { closeAllDatabases, database } from '../src/factory';
import {
  ENV_DATABASE_BINDINGS,
  ENV_DATABASE_URL,
  parseDatabaseUrl,
  resolveDatabaseUrl,
  setDatabaseBindingOverride,
} from '../src/postgres/binding';
import { __setPhysicalPoolFactoryForTests } from '../src/postgres/physical-pool';
import { setPrimaryDatasource } from '../src/primary-datasource';

type Options = ConnectionOptions;

/** The options of every physical pool the factory opened, in order. */
const opened: Options[] = [];

function setConfig(database: unknown): void {
  process.env.CONFIG_DATA = JSON.stringify({ database });
  resetConfigLoader();
}

let previousOverride: string | undefined;

beforeEach(() => {
  opened.length = 0;
  previousOverride = setDatabaseBindingOverride(undefined);
  __setPhysicalPoolFactoryForTests((options) => {
    opened.push(options);
    const sql = Object.assign(() => undefined, { end: async () => {}, options });
    return sql as unknown as SQL;
  });
});

afterEach(async () => {
  await closeAllDatabases();
  __setPhysicalPoolFactoryForTests(undefined);
  setDatabaseBindingOverride(previousOverride);
  setPrimaryDatasource(undefined);
  delete process.env[ENV_DATABASE_URL];
  delete process.env[ENV_DATABASE_BINDINGS];
  delete process.env.CONFIG_DATA;
  resetConfigLoader();
});

describe('parseDatabaseUrl', () => {
  it('decomposes a postgres URL into the connection fields', () => {
    const resolved = parseDatabaseUrl(
      'postgres://app:s%40cret@db.example.com:6543/tasks?sslmode=disable&application_name=api&pool_max_conns=4',
      ENV_DATABASE_URL,
    );
    expect(resolved.config).toEqual({
      host: 'db.example.com',
      port: 6543,
      database: 'tasks',
      user: 'app',
      password: 's@cret',
      ssl: false,
      poolSize: 4,
    });
    expect(resolved.connectionParams).toEqual({ application_name: 'api' });
    expect(resolved.origin).toBe(ENV_DATABASE_URL);
  });

  it('accepts the postgresql scheme and the Cloud SQL socket form', () => {
    const resolved = parseDatabaseUrl('postgresql:///tasks?host=/cloudsql/p:r:i', 'database.default.url');
    expect(resolved.config.host).toBe('/cloudsql/p:r:i');
    expect(resolved.config.database).toBe('tasks');
  });

  specTest(
    'refuses a value that is not a postgres URL, naming its origin and never repeating it',
    {
      feature: 'typescript/relational-persistence',
      requirement: 'connection-url',
      check: 'a-malformed-url-names-its-origin-never-its-value',
    },
    () => {
      for (const value of ['localhost:5432/tasks', 'mysql://u:hunter2@h/db', 'postgres://u:hunter2@[bad/db']) {
        let message = '';
        try {
          parseDatabaseUrl(value, ENV_DATABASE_URL);
        } catch (err) {
          message = (err as Error).message;
        }
        expect(message).toContain('DATABASE_URL is not a valid postgres URL');
        expect(message).not.toContain('hunter2');
      }
    },
  );
});

describe('resolveDatabaseUrl', () => {
  it('reads the url field of the datasource section', () => {
    setConfig({ auth: { url: 'postgres://u:p@auth-db:5432/auth' } });
    const resolved = resolveDatabaseUrl('auth', false);
    expect(resolved?.origin).toBe('database.auth.url');
    expect(resolved?.config.host).toBe('auth-db');
  });

  it('falls back to DATABASE_URL for the primary datasource only', () => {
    process.env[ENV_DATABASE_URL] = 'postgres://u:p@env-db:5432/app';
    expect(resolveDatabaseUrl('default', true)?.config.host).toBe('env-db');
    expect(resolveDatabaseUrl('auth', false)).toBeUndefined();
  });

  it('prefers the url field over DATABASE_URL', () => {
    setConfig({ default: { url: 'postgres://u:p@cfg-db:5432/app' } });
    process.env[ENV_DATABASE_URL] = 'postgres://u:p@env-db:5432/app';
    expect(resolveDatabaseUrl('default', true)?.config.host).toBe('cfg-db');
  });

  it('prefers the url passed to database() over both', () => {
    setConfig({ default: { url: 'postgres://u:p@cfg-db:5432/app' } });
    expect(resolveDatabaseUrl('default', true, 'postgres://u:p@arg-db:5432/app')?.config.host).toBe('arg-db');
  });

  it('returns undefined when nothing configures a URL', () => {
    setConfig({ default: { host: 'localhost', database: 'app' } });
    expect(resolveDatabaseUrl('default', true)).toBeUndefined();
  });

  it('refuses a url field that is not a string', () => {
    setConfig({ default: { url: 5432 } });
    expect(() => resolveDatabaseUrl('default', true)).toThrow('database.default.url must be a string postgres URL');
  });
});

describe('database() with a connection URL', () => {
  specTest(
    'connects the default datasource through DATABASE_URL, with no database config',
    {
      feature: 'typescript/relational-persistence',
      requirement: 'connection-url',
      check: 'a-connection-url-configures-the-datasource',
    },
    async () => {
      process.env[ENV_DATABASE_URL] = 'postgres://app:pw@env-db:6543/tasks?sslmode=require&application_name=api';
      await database();
      expect(opened).toHaveLength(1);
      const options = opened[0] as Options & { connection?: Record<string, unknown> };
      expect(options.host).toBe('env-db');
      expect(options.port).toBe(6543);
      expect(options.database).toBe('tasks');
      expect(options.username).toBe('app');
      expect(options.ssl).toBe('require');
      expect(options.connection?.['application_name']).toBe('api');
    },
  );

  it('lets the url win over the structured fields of its section, and keeps the tuning', async () => {
    setConfig({
      default: { url: 'postgres://app:pw@url-db:5432/tasks', host: 'localhost', database: 'other', poolSize: 3 },
    });
    await database();
    expect(opened[0]?.host).toBe('url-db');
    expect(opened[0]?.database).toBe('tasks');
    expect(opened[0]?.max).toBe(3);
  });

  it('applies DATABASE_URL to the declared primary datasource, not to the others', async () => {
    setPrimaryDatasource({ name: 'tasks' });
    process.env[ENV_DATABASE_URL] = 'postgres://app:pw@env-db:5432/tasks';
    setConfig({ auth: { host: 'auth-db', database: 'auth', user: 'app' } });
    await database();
    await database('auth');
    expect(opened.map((o) => o.host)).toEqual(['env-db', 'auth-db']);
  });

  it('gives DATABASE_BINDINGS precedence over DATABASE_URL', async () => {
    process.env[ENV_DATABASE_URL] = 'postgres://app:pw@env-db:5432/tasks';
    process.env[ENV_DATABASE_BINDINGS] = JSON.stringify({
      protocolVersion: 1,
      databases: { default: { engine: 'postgres', schema: 'public', connection: { host: 'bound-db', database: 'b' } } },
    });
    await database();
    expect(opened[0]?.host).toBe('bound-db');
  });
});

describe('the effective config of a URL-only section', () => {
  it('keeps the tuning keys the Repository reads', () => {
    setConfig({ default: { url: 'postgres://u:p@db:5432/app', maxRowLimit: 500, slowQueryThresholdMs: 200 } });
    const { conf } = effectiveConfig(undefined);
    expect(conf.database).toBe('app');
    expect(conf.maxRowLimit).toBe(500);
    expect(conf.slowQueryThresholdMs).toBe(200);
  });

  it('keeps the section port when the URL carries none', () => {
    setConfig({ default: { url: 'postgres://u:p@db/app', port: 6543 } });
    expect(effectiveConfig(undefined).conf.port).toBe(6543);
  });
});

describe('the effective config of a datasource across queries', () => {
  it('resolves once while nothing it reads changes, and again when one input does', () => {
    setConfig({ default: { url: 'postgres://u:p@db:5432/app' } });
    const first = effectiveConfig(undefined);
    expect(effectiveConfig(undefined)).toBe(first);

    setConfig({ default: { url: 'postgres://u:p@db:5432/other' }, tasks: { url: 'postgres://u:p@db:5432/tasks' } });
    const reloaded = effectiveConfig(undefined);
    expect(reloaded.conf.database).toBe('other');
    expect(effectiveConfig(undefined)).toBe(reloaded);

    process.env[ENV_DATABASE_BINDINGS] = JSON.stringify({
      protocolVersion: 1,
      databases: { default: { engine: 'postgres', schema: 'public', connection: { host: 'bound-db', database: 'b' } } },
    });
    expect(effectiveConfig(undefined).conf.host).toBe('bound-db');
    delete process.env[ENV_DATABASE_BINDINGS];

    setDatabaseBindingOverride(
      JSON.stringify({
        protocolVersion: 1,
        databases: {
          default: { engine: 'postgres', schema: 'public', connection: { host: 'pinned-db', database: 'p' } },
        },
      }),
    );
    expect(effectiveConfig(undefined).conf.host).toBe('pinned-db');
    setDatabaseBindingOverride(undefined);

    setPrimaryDatasource({ name: 'tasks' });
    expect(effectiveConfig(undefined).resolvedName).toBe('tasks');
    setPrimaryDatasource(undefined);
    expect(effectiveConfig(undefined).resolvedName).toBe('default');
  });

  it('follows DATABASE_URL when it changes', () => {
    setConfig({});
    process.env[ENV_DATABASE_URL] = 'postgres://u:p@one:5432/app';
    expect(effectiveConfig(undefined).conf.host).toBe('one');
    process.env[ENV_DATABASE_URL] = 'postgres://u:p@two:5432/app';
    expect(effectiveConfig(undefined).conf.host).toBe('two');
  });
});
