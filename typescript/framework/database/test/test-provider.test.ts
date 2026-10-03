import { describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import type { DatabaseConnectionSpec, DatabaseTestBinding } from '../src/postgres/binding';
import { parseTestBinding } from '../src/postgres/binding';
import { SQLSource } from '../src/migrations/sql-source';
import {
  assignTemplates,
  bundleDigest,
  connectionDatabase,
  effectiveMode,
  isolationOf,
  pgIdent,
  planDatabases,
  provision,
  reuseTemplates,
  runtimeBinding,
  selectDatasources,
  setConnectionDatabase,
  templateName,
  TestProviderSkip,
} from '../src/test-provider';

function tcp(database: string): DatabaseConnectionSpec {
  return { host: 'h', port: 5432, database, user: 'postgres', password: 'postgres' };
}

function tb(databases: Record<string, unknown>, policy: Partial<DatabaseTestBinding> = {}): DatabaseTestBinding {
  return { protocolVersion: 1, databases: databases as DatabaseTestBinding['databases'], ...policy };
}

const fixedSuffix = (...values: string[]): (() => string) => {
  let i = 0;
  return () => values[i++ % values.length];
};

describe('parseTestBinding', () => {
  it('parses policy + datasources', () => {
    const doc = parseTestBinding(
      JSON.stringify({
        protocolVersion: 1,
        mode: 'require',
        isolation: 'database',
        reuse: 'bundle-template',
        applyMigrations: true,
        databases: { auth: { engine: 'postgres', schema: 'iam', connection: { host: 'h', database: 'auth' } } },
      }),
    );
    expect(doc.mode).toBe('require');
    expect(doc.isolation).toBe('database');
    expect(doc.reuse).toBe('bundle-template');
    expect(doc.applyMigrations).toBe(true);
  });

  it('rejects an invalid mode/isolation/reuse', () => {
    expect(() => parseTestBinding(JSON.stringify({ protocolVersion: 1, mode: 'maybe', databases: {} }))).toThrow();
    expect(() => parseTestBinding(JSON.stringify({ protocolVersion: 1, isolation: 'table', databases: {} }))).toThrow();
    expect(() => parseTestBinding(JSON.stringify({ protocolVersion: 1, reuse: 'always', databases: {} }))).toThrow();
  });

  it('rejects an invalid datasource entry', () => {
    expect(() =>
      parseTestBinding(JSON.stringify({ protocolVersion: 1, databases: { auth: { engine: 'mysql' } } })),
    ).toThrow();
  });
});

describe('policy helpers', () => {
  it('defaults isolation to database and mode to require', () => {
    expect(isolationOf({ protocolVersion: 1 })).toBe('database');
    expect(isolationOf({ protocolVersion: 1, isolation: 'schema' })).toBe('schema');
    expect(effectiveMode(undefined)).toBe('require');
    expect(effectiveMode({ protocolVersion: 1, mode: 'skip' })).toBe('skip');
  });

  it('decides reuse only for apply + bundle-template + database isolation', () => {
    expect(reuseTemplates({ protocolVersion: 1, reuse: 'bundle-template' }, true)).toBe(true);
    expect(reuseTemplates({ protocolVersion: 1, reuse: 'bundle-template' }, false)).toBe(false);
    expect(reuseTemplates({ protocolVersion: 1, reuse: 'none' }, true)).toBe(false);
    expect(reuseTemplates({ protocolVersion: 1, reuse: 'bundle-template', isolation: 'schema' }, true)).toBe(false);
  });
});

describe('templateName', () => {
  it('keys by base + datasource + 12-char digest, deterministically', () => {
    expect(templateName('auth', 'auth', 'abcdef0123456789')).toMatch(/^auth_tmpl_auth_[0-9a-f]{8}_abcdef012345$/);
    expect(templateName('auth', 'auth', 'abcdef0123456789')).toBe(templateName('auth', 'auth', 'abcdef0123456789'));
    expect(templateName('auth', 'auth', 'ffffffffffffffff')).not.toBe(templateName('auth', 'auth', 'abcdef0123456789'));
  });

  it('separates datasources sharing one maintenance database and digest', () => {
    // Two datasources on the same maintenance database must
    // never name the same template, or one clones the other's (empty) one.
    expect(templateName('postgres', 'default', 'abcdef0123456789')).not.toBe(
      templateName('postgres', 'identity', 'abcdef0123456789'),
    );
  });

  it('sanitizes the datasource into the identifier', () => {
    const name = templateName('auth', 'Identity-Svc.v2', 'abcdef0123456789');
    expect(name).toContain('_tmpl_identity_svc_v2_');
    expect(name).toMatch(/^[a-z0-9_]+$/);
  });

  it('stays within 63 bytes and collision-free for long bases and datasources', () => {
    const longBaseA = `${'x'.repeat(99)}a`;
    const longBaseB = `${'x'.repeat(99)}b`;
    const longDsA = `${'d'.repeat(99)}a`;
    const longDsB = `${'d'.repeat(99)}b`;
    const digest = 'abcdef0123456789';
    const names = [
      templateName(longBaseA, longDsA, digest),
      templateName(longBaseB, longDsA, digest),
      templateName(longBaseA, longDsB, digest),
      templateName(longBaseB, longDsB, digest),
    ];
    for (const n of names) {
      expect(n.length).toBeLessThanOrEqual(63);
      expect(n.endsWith('_abcdef012345')).toBe(true);
    }
    // Clamping the readable parts must not merge distinct (base, datasource) pairs.
    expect(new Set(names).size).toBe(names.length);
  });
});

describe('assignTemplates', () => {
  const src = (datasource: string, sql: string) =>
    new SQLSource({
      namespace: 'iam',
      datasource: { name: datasource, schema: 'auth' },
      definitions: [{ name: '001_init', sql }],
    });

  // The binding a CI runner injects: every declared fleet
  // datasource plus the conventional `default`, all on the image-local
  // maintenance database.
  const issueDatabases = {
    default: { engine: 'postgres', schema: 'public', connection: tcp('postgres') },
    identity: { engine: 'postgres', schema: 'auth', connection: tcp('postgres') },
  };
  const issueBinding = tb(issueDatabases, {
    isolation: 'database',
    reuse: 'bundle-template',
    applyMigrations: true,
  });

  const plansFor = (binding: DatabaseTestBinding, sources: SQLSource[], apply = true) =>
    assignTemplates(planDatabases(binding, fixedSuffix('aaa', 'bbb')), binding, sources, apply);

  it('gives a datasource with no migrations no template at all', () => {
    const sources = [src('identity', 'CREATE TABLE oauth_clients ()')];
    const plans = plansFor(issueBinding, sources);
    // `default` sorts first and is provisioned first — under the old key it
    // created an empty `postgres_tmpl_<digest>` that `identity` then cloned.
    expect(plans.map((p) => p.name)).toEqual(['default', 'identity']);
    expect(plans[0].template).toBeUndefined();
    expect(plans[1].template).toMatch(/^postgres_tmpl_identity_[0-9a-f]{8}_[0-9a-f]{12}$/);

    // The plans left without a template are exactly the ones no source targets,
    // so provision's fresh apply over them is a no-op — never a skipped migration.
    const fresh = plans.filter((p) => !p.template).map((p) => p.name);
    expect(fresh).toEqual(['default']);
    expect(sources.some((s) => fresh.includes(s.datasource))).toBe(false);
  });

  it('gives each migrated datasource its own template on one maintenance database', () => {
    const plans = plansFor(issueBinding, [
      src('default', 'CREATE TABLE jobs ()'),
      src('identity', 'CREATE TABLE oauth_clients ()'),
    ]);
    expect(plans[0].template).toBeDefined();
    expect(plans[1].template).toBeDefined();
    expect(plans[0].template).not.toBe(plans[1].template);
  });

  it('assigns no template without bundle-template reuse or without applying', () => {
    const sources = [src('identity', 'CREATE TABLE oauth_clients ()')];
    const noReuse = tb(issueDatabases, { isolation: 'database', reuse: 'none', applyMigrations: true });
    expect(plansFor(noReuse, sources).every((p) => p.template === undefined)).toBe(true);
    expect(plansFor(issueBinding, sources, false).every((p) => p.template === undefined)).toBe(true);
  });
});

describe('planDatabases', () => {
  it('plans database isolation sorted, with isolated names from the connection database', () => {
    const plans = planDatabases(
      tb(
        {
          billing: { engine: 'postgres', schema: 'billing', connection: tcp('billing') },
          auth: { engine: 'postgres', schema: 'iam', connection: tcp('auth') },
        },
        { isolation: 'database' },
      ),
      fixedSuffix('aaa', 'bbb'),
    );
    expect(plans.map((p) => p.name)).toEqual(['auth', 'billing']);
    expect(plans[0].isoDB).toBe('auth_t_aaa');
    expect(plans[1].isoDB).toBe('billing_t_bbb');
    expect(plans[0].isoSchema).toBe('iam');
  });

  it('plans schema isolation with an isolated schema and no isolated database', () => {
    const plans = planDatabases(
      tb({ auth: { engine: 'postgres', schema: 'iam', connection: tcp('auth') } }, { isolation: 'schema' }),
      fixedSuffix('zzz'),
    );
    expect(plans[0].isoSchema).toBe('iam_t_zzz');
    expect(plans[0].isoDB).toBeUndefined();
  });

  it('rejects unsupported engine / missing connection', () => {
    expect(() => planDatabases(tb({ a: { engine: 'mysql', connection: tcp('a') } }), fixedSuffix('s'))).toThrow();
    expect(() => planDatabases(tb({ a: { engine: 'postgres', schema: 's' } }), fixedSuffix('s'))).toThrow();
  });

  it('threads keepDatabases into every plan so teardown skips the drop', () => {
    const kept = planDatabases(
      tb({ auth: { engine: 'postgres', schema: 'iam', connection: tcp('auth') } }, { keepDatabases: true }),
      fixedSuffix('aaa'),
    );
    expect(kept[0].keep).toBe(true);
    const dropped = planDatabases(
      tb({ auth: { engine: 'postgres', schema: 'iam', connection: tcp('auth') } }),
      fixedSuffix('aaa'),
    );
    expect(dropped[0].keep).toBeFalsy();
  });
});

describe('runtimeBinding', () => {
  it('swaps the database for database isolation, preserving the owning schema', () => {
    const binding = runtimeBinding([
      {
        name: 'auth',
        isolation: 'database',
        entry: { engine: 'postgres', schema: 'iam', connection: tcp('auth') },
        isoDB: 'auth_t_x',
        isoSchema: 'iam',
      },
    ]);
    expect(binding.databases?.auth.connection?.database).toBe('auth_t_x');
    expect(binding.databases?.auth.schema).toBe('iam');
    expect(binding.protocolVersion).toBe(1);
  });

  it('uses the isolated schema as search_path for schema isolation', () => {
    const binding = runtimeBinding([
      {
        name: 'auth',
        isolation: 'schema',
        entry: { engine: 'postgres', schema: 'iam', connection: tcp('auth') },
        isoSchema: 'iam_t_x',
      },
    ]);
    expect(binding.databases?.auth.schema).toBe('iam_t_x');
    expect(binding.databases?.auth.connection?.database).toBe('auth');
  });

  it('does not mutate the input connection', () => {
    const conn = tcp('auth');
    runtimeBinding([
      {
        name: 'auth',
        isolation: 'database',
        entry: { engine: 'postgres', schema: 'iam', connection: conn },
        isoDB: 'auth_t_x',
      },
    ]);
    expect(conn.database).toBe('auth');
  });

  it('rewrites the database in a DSN connection', () => {
    const binding = runtimeBinding([
      {
        name: 'events',
        isolation: 'database',
        entry: {
          engine: 'postgres',
          schema: 'events',
          connection: { dsn: 'postgres://u:p@h:5432/events?sslmode=disable' },
        },
        isoDB: 'events_t_x',
      },
    ]);
    const dsn = binding.databases?.events.connection?.dsn ?? '';
    expect(dsn).toContain('/events_t_x');
    expect(dsn).toContain('sslmode=disable');
  });
});

describe('connection database helpers', () => {
  it('reads and rewrites structured and DSN databases', () => {
    expect(connectionDatabase(tcp('auth'))).toBe('auth');
    expect(connectionDatabase({ dsn: 'postgres://u:p@h:5432/events' })).toBe('events');

    const structured = tcp('auth');
    setConnectionDatabase(structured, 'auth_t_x');
    expect(structured.database).toBe('auth_t_x');

    const dsn: DatabaseConnectionSpec = { dsn: 'postgres://u:p@h:5432/events' };
    setConnectionDatabase(dsn, 'events_t_x');
    expect(dsn.dsn).toContain('/events_t_x');
  });
});

describe('pgIdent', () => {
  it('sanitizes and clamps to 63 bytes keeping the tail', () => {
    expect(pgIdent('Auth-DB', 't', 'abc123')).toBe('auth_db_t_abc123');
    const long = pgIdent('x'.repeat(100), 't', 'abcdef');
    expect(long.length).toBeLessThanOrEqual(63);
    expect(long.endsWith('_t_abcdef')).toBe(true);
  });
});

describe('bundleDigest', () => {
  it('is stable for the same sources and changes with the migrations', () => {
    const src = (sql: string) =>
      new SQLSource({
        namespace: 'iam',
        datasource: { name: 'auth', schema: 'iam' },
        definitions: [{ name: '001_init', sql }],
      });
    const a = bundleDigest([src('CREATE TABLE a ()')]);
    expect(bundleDigest([src('CREATE TABLE a ()')])).toBe(a);
    expect(bundleDigest([src('CREATE TABLE b ()')])).not.toBe(a);
  });
});

describe('provision (no live database)', () => {
  it('throws TestProviderSkip when mode=skip and no datasources', async () => {
    await expect(provision({ binding: { protocolVersion: 1, mode: 'skip' } })).rejects.toBeInstanceOf(TestProviderSkip);
  });

  it('fails loudly when mode=require and no datasources', async () => {
    await expect(provision({ binding: { protocolVersion: 1, mode: 'require' } })).rejects.toThrow(
      /DATABASE_TEST_BINDINGS/,
    );
  });
});

// The datasource-selection corpus both test providers run. Its Go twin is
// go/framework/database/testprovider/datasources_conformance_test.go.
const DATASOURCES_CORPUS = join(__dirname, '../../../../protocols/database/conformance/test-provider-datasources.json');

interface DatasourcesCase {
  id: string;
  mode?: DatabaseTestBinding['mode'];
  datasources?: string[];
  expect: { datasources?: string[]; error?: 'skip' | 'require'; names?: string[] };
}

describe('datasource selection corpus', () => {
  const corpus = JSON.parse(readFileSync(DATASOURCES_CORPUS, 'utf8')) as {
    binding: unknown;
    cases: DatasourcesCase[];
  };

  it('has cases', () => {
    expect(corpus.cases.length).toBeGreaterThan(0);
  });

  for (const c of corpus.cases) {
    it(c.id, async () => {
      const binding = parseTestBinding(JSON.stringify(corpus.binding));
      if (c.mode) {
        binding.mode = c.mode;
      }

      if (c.expect.error) {
        const err = await provision({ binding, datasources: c.datasources }).then(
          () => undefined,
          (e: unknown) => e as Error,
        );
        expect(err).toBeInstanceOf(Error);
        expect(err instanceof TestProviderSkip).toBe(c.expect.error === 'skip');
        const list = (c.expect.names ?? []).map((n) => JSON.stringify(n)).join(', ');
        expect(err?.message).toContain(`has no datasource ${list}`);
        return;
      }

      const plans = planDatabases(selectDatasources(binding, c.datasources), fixedSuffix('s'));
      expect(plans.map((p) => p.name)).toEqual(c.expect.datasources ?? []);
      expect(Object.keys(runtimeBinding(plans).databases ?? {}).sort()).toEqual(c.expect.datasources ?? []);
    });
  }
});

describe('selectDatasources', () => {
  it("returns the binding itself without a list and never mutates the caller's binding", () => {
    const binding = tb(
      {
        auth: { engine: 'postgres', connection: tcp('auth') },
        billing: { engine: 'postgres', connection: tcp('billing') },
      },
      { mode: 'require' },
    );
    expect(selectDatasources(binding)).toBe(binding);
    expect(selectDatasources(binding, [])).toBe(binding);
    const selected = selectDatasources(binding, ['auth']);
    expect(Object.keys(selected.databases ?? {})).toEqual(['auth']);
    expect(selected.mode).toBe('require');
    expect(Object.keys(binding.databases ?? {})).toEqual(['auth', 'billing']);
  });

  // provision narrows the binding before it plans. The unselected "broken" entry
  // has no connection, so planning it fails before any connection opens. The
  // selected entry points at a closed local port, so a narrowed run fails later,
  // while connecting to it.
  it('is applied by provision before it plans', async () => {
    const binding = tb(
      {
        auth: {
          engine: 'postgres',
          connection: { host: '127.0.0.1', port: 1, database: 'auth', user: 'u', password: 'p' },
        },
        broken: { engine: 'postgres' },
      },
      { keepDatabases: true },
    );
    await expect(provision({ binding })).rejects.toThrow(/"broken" has no connection/);
    const err = await provision({ binding, datasources: ['auth'] }).then(
      () => undefined,
      (e: unknown) => e as Error,
    );
    expect(err).toBeInstanceOf(Error);
    expect(err?.message).not.toContain('broken');
    expect(err?.message).toMatch(/ECONNREFUSED/);
  });

  it('does not take an inherited property for a datasource', () => {
    const binding = tb({ auth: { engine: 'postgres', connection: tcp('auth') } });
    expect(() => selectDatasources(binding, ['toString'])).toThrow(/has no datasource "toString"/);
  });
});
