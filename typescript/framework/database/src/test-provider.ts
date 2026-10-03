import {
  collectBundleContributions,
  computeBundleDigest,
  MigrationRegistry,
  normalizeBundle,
} from '@putnami/migration';
import postgres from 'postgres';
import { closeAllDatabases } from './factory';
import { SQLRunner } from './migrations/sql-runner';
import type { SQLSource } from './migrations/sql-source';
import {
  connectionToConfig,
  type DatabaseBinding,
  type DatabaseBindingEntry,
  type DatabaseConnectionSpec,
  type DatabaseTestBinding,
  DATABASE_PROTOCOL_VERSION,
  ENV_DATABASE_BINDINGS,
  parseTestBinding,
  setDatabaseBindingOverride,
  type TestIsolation,
} from './postgres/binding';
import { applyConnectionParams, buildOptions } from './postgres/options';
import type { PostgresConfig } from './postgres/config';
import type { InferConfig } from '@putnami/runtime';
import { restoreEnv } from '@putnami/utils';
import { isolatedSuffix, pgOrphanServer, reclaimOrphans, SUFFIX_LEN } from './test-provider-reclaim';

/**
 * The cross-language test database provider (TypeScript).
 *
 * The counterpart of `go.putnami.dev/database/testprovider`: it turns a canonical
 * test binding (the same `DATABASE_TEST_BINDINGS` contract the Go provider reads)
 * into isolated, migrated databases for one or more named datasources, against an
 * externally provided Postgres selected by `mode: require`.
 *
 * `provision` creates an isolated database (or schema) per datasource, applies the
 * workload's migration sources with the existing migration machinery (a one-off
 * `MigrationRegistry` + `SQLRunner`), sets the owning schema as the session
 * `search_path`, and injects the resolved runtime binding into
 * `DATABASE_BINDINGS` so the workload's own `database(datasource)` connects to the
 * isolated database — then returns that binding and a `cleanup` that tears
 * everything down. `reuse: bundle-template` clones per-suite databases from a
 * migrated template keyed by maintenance database + datasource + bundle digest
 * (advisory-lock guarded). Docker `mode: auto` is a later slice.
 *
 * Unit tests pay no database cost: the live path runs only when
 * `DATABASE_TEST_BINDINGS` is injected.
 */

/** Environment variable carrying the canonical test binding (JSON). */
export const ENV_TEST_BINDING = 'DATABASE_TEST_BINDINGS';

const SKIP_MESSAGE = 'database test provider: no usable test binding (mode=skip)';

/** Raised (mode=skip) when no usable test binding/provider exists. */
export class TestProviderSkip extends Error {
  constructor(message = SKIP_MESSAGE) {
    super(message);
    this.name = 'TestProviderSkip';
  }
}

export interface ProvisionOptions {
  /** The test binding; when omitted it is read and validated from ENV_TEST_BINDING. */
  binding?: DatabaseTestBinding;
  /** The workload's migration sources, applied when the binding sets applyMigrations. */
  sources?: readonly SQLSource[];
  /**
   * The datasources the suite uses. When set, only those entries of the binding are
   * planned, provisioned, reclaimed and torn down, and the returned binding carries
   * only them; a name the binding lacks follows the binding's mode (see
   * selectDatasources). Omitted or empty provisions every datasource of the binding.
   */
  datasources?: readonly string[];
  /** Injects the isolated-identifier suffix; defaults to the creation time followed by random hex. */
  newSuffix?: () => string;
}

export interface ProvisionResult {
  /** Runtime binding whose datasources point at the provisioned, migrated databases. */
  binding: DatabaseBinding;
  /** Tears down the provisioned databases/schemas and restores DATABASE_BINDINGS. */
  cleanup: () => Promise<void>;
}

// Connection-tuning defaults the admin DDL connections inherit. Mirrors the
// connection-relevant defaults of PostgresConfig; the binding supplies the
// connection itself.
const ADMIN_DEFAULTS = {
  host: 'localhost',
  port: 5432,
  database: 'postgres',
  poolSize: 1,
  debug: false,
  maxRowLimit: 10_000,
  slowQueryThresholdMs: 0,
  statementTimeoutMs: 0,
  idleInTransactionTimeoutMs: 0,
  connectTimeout: 10,
  idleTimeout: 0,
  maxLifetime: 0,
} as const;

interface Plan {
  name: string;
  isolation: TestIsolation;
  entry: DatabaseBindingEntry;
  isoDB?: string; // isolated database (isolation=database)
  isoPrefix?: string; // name prefix every per-suite database of this base shares (isolation=database)
  isoSchema?: string; // owning schema applied as search_path
  template?: string; // migrated template to clone from (reuse); undefined applies fresh
  keep?: boolean; // leave the isolated database in place on teardown (binding keepDatabases)
}

function failProvider(message: string): never {
  throw new Error(`${ENV_TEST_BINDING}: ${message}`);
}

/** isolationOf returns the binding's isolation, defaulting to database. */
export function isolationOf(tb: DatabaseTestBinding): TestIsolation {
  return tb.isolation ?? 'database';
}

/** effectiveMode returns the binding's mode, defaulting to require. */
export function effectiveMode(tb: DatabaseTestBinding | undefined): 'skip' | 'require' | 'auto' {
  return tb?.mode ?? 'require';
}

/**
 * reuseTemplates reports whether to clone per-suite databases from a migrated
 * template keyed by the datasource + bundle digest: the binding opted into
 * bundle-template reuse, migrations are applied, and isolation is database.
 */
export function reuseTemplates(tb: DatabaseTestBinding, apply: boolean): boolean {
  return apply && tb.reuse === 'bundle-template' && isolationOf(tb) === 'database';
}

/** Readable datasource characters kept in a template name; the rest is folded into the discriminator. */
const TEMPLATE_DATASOURCE_SLUG = 16;

/**
 * templateName keys a migrated template database by maintenance database
 * (`base`), datasource name and bundle digest.
 *
 * The datasource is part of the key because one binding routinely points several
 * datasources at the same maintenance database while each template holds only
 * that datasource's migrations: keying by base + digest alone made them collide
 * on a single template, so whichever datasource was provisioned first (sorted by
 * name) created an empty template every later datasource then cloned.
 *
 * Readable parts are clamped to Postgres' 63-byte identifier limit, so the name
 * also carries a short stable hash of the full (base, datasource) pair —
 * truncating a long base or datasource can never make two distinct pairs share a
 * template.
 */
export function templateName(base: string, datasource: string, digest: string): string {
  const slug = sanitizeIdent(datasource).slice(0, TEMPLATE_DATASOURCE_SLUG) || 'ds';
  // Length-prefixed so no other (base, datasource) pair can hash the same way.
  const discriminator = fnv1aHex(`${base.length}:${base}:${datasource}`);
  return pgIdent(base, `tmpl_${slug}_${discriminator}`, digest.slice(0, 12));
}

/**
 * selectDatasources narrows the test binding to the named datasources, so a suite
 * that uses one datasource of a workspace-wide binding pays for that one only. It
 * returns `tb` itself when `names` is empty, and otherwise a copy that shares
 * `tb`'s policy and holds only the named entries. A name the binding lacks is
 * handled like a missing binding: mode skip throws TestProviderSkip, require and
 * auto fail and name every missing datasource.
 */
export function selectDatasources(tb: DatabaseTestBinding, names: readonly string[] = []): DatabaseTestBinding {
  if (names.length === 0) {
    return tb;
  }
  const databases: NonNullable<DatabaseTestBinding['databases']> = {};
  const missing = new Set<string>();
  for (const name of names) {
    const entry = Object.hasOwn(tb.databases ?? {}, name) ? tb.databases?.[name] : undefined;
    if (entry) {
      databases[name] = entry;
    } else {
      missing.add(name);
    }
  }
  if (missing.size > 0) {
    const list = [...missing]
      .sort()
      .map((n) => JSON.stringify(n))
      .join(', ');
    if (effectiveMode(tb) === 'skip') {
      throw new TestProviderSkip(`${SKIP_MESSAGE}: the test binding has no datasource ${list}`);
    }
    failProvider(
      `the test binding has no datasource ${list} (mode=${effectiveMode(tb)} requires every requested datasource)`,
    );
  }
  return { ...tb, databases };
}

/**
 * planDatabases turns the test binding into one plan per datasource, sorted by
 * name, allocating a unique isolated identifier for each. Pure — unit-tested
 * without a live server.
 */
export function planDatabases(tb: DatabaseTestBinding, suffix: () => string): Plan[] {
  const iso = isolationOf(tb);
  const names = Object.keys(tb.databases ?? {}).sort();
  return names.map((name) => {
    const entry = tb.databases?.[name];
    if (!entry) {
      failProvider(`datasource ${JSON.stringify(name)} has no entry`);
    }
    if (entry.engine !== 'postgres') {
      failProvider(`datasource ${JSON.stringify(name)} uses unsupported engine ${JSON.stringify(entry.engine)}`);
    }
    if (!entry.connection) {
      failProvider(`datasource ${JSON.stringify(name)} has no connection`);
    }
    const plan: Plan = { name, isolation: iso, entry, isoSchema: entry.schema, keep: tb.keepDatabases };
    if (iso === 'database') {
      const base = connectionDatabase(entry.connection);
      plan.isoDB = pgIdent(firstNonEmpty(base, name), 't', suffix());
      plan.isoPrefix = isolatedPrefix(firstNonEmpty(base, name));
    } else {
      plan.isoSchema = pgIdent(firstNonEmpty(entry.schema, name), 't', suffix());
    }
    return plan;
  });
}

/**
 * assignTemplates decides, per plan, the migrated template it clones from —
 * keyed by maintenance database + datasource + bundle digest — and returns the
 * same plans. Pure (beyond the assignment) — unit-tested without a live server.
 *
 * A plan only gets a template when the bundle actually carries migrations for
 * its datasource, decided with the very predicate `applyInto` filters with, so
 * the decision and the apply can never disagree. A datasource with no migrations
 * would otherwise create and keep an empty template that every later run clones:
 * plans without one take the fresh path instead (plain CREATE DATABASE + owning
 * schema, then `provision` migrates them in place if the bundle ever does target
 * them).
 */
export function assignTemplates(
  plans: Plan[],
  tb: DatabaseTestBinding,
  sources: readonly SQLSource[],
  apply: boolean,
): Plan[] {
  if (!reuseTemplates(tb, apply)) {
    return plans;
  }
  const digest = bundleDigest(sources);
  for (const p of plans) {
    if (sourcesTargeting(sources, new Set([p.name])).length === 0) {
      continue;
    }
    const base = connectionDatabase(p.entry.connection as DatabaseConnectionSpec);
    p.template = templateName(firstNonEmpty(base, p.name), p.name, digest);
  }
  return plans;
}

/**
 * runtimeBinding builds the runtime Binding tests consume. For database
 * isolation the connection's database is swapped to the isolated one (owning
 * schema unchanged); for schema isolation the connection is unchanged and the
 * isolated schema becomes the search_path. Pure — unit-tested.
 */
export function runtimeBinding(plans: Plan[]): DatabaseBinding {
  const databases: Record<string, DatabaseBindingEntry> = {};
  for (const p of plans) {
    const connection = cloneConnection(p.entry.connection as DatabaseConnectionSpec);
    let schema = p.entry.schema;
    if (p.isolation === 'database') {
      setConnectionDatabase(connection, p.isoDB as string);
    } else {
      schema = p.isoSchema;
    }
    databases[p.name] = { engine: 'postgres', schema, connection };
  }
  return { protocolVersion: DATABASE_PROTOCOL_VERSION, databases };
}

/**
 * provision provisions isolated, migrated databases for every datasource in the
 * test binding, or only for those `options.datasources` names, and returns their
 * runtime binding + a cleanup. With no usable
 * binding it honors the mode: skip → TestProviderSkip, require/auto → a loud
 * error (Docker auto-provisioning is a later slice).
 *
 * A test process that is killed never runs `cleanup`. With database isolation
 * and without keepDatabases, provision therefore first reclaims the per-suite
 * databases of the same base that such a process left behind: those with no
 * open connection that are older than an hour (see reclaimOrphans).
 */
export async function provision(options: ProvisionOptions = {}): Promise<ProvisionResult> {
  const bound = options.binding ?? readEnvBinding();
  if (!bound || Object.keys(bound.databases ?? {}).length === 0) {
    if (effectiveMode(bound) === 'skip') {
      throw new TestProviderSkip();
    }
    failProvider(`no test database binding: set ${ENV_TEST_BINDING} (mode=${effectiveMode(bound)} requires one)`);
  }
  const tb = selectDatasources(bound, options.datasources);

  const suffix = options.newSuffix ?? (() => isolatedSuffix(new Date()));
  const sources = options.sources ?? [];
  const apply = !!tb.applyMigrations && sources.length > 0;
  const plans = assignTemplates(planDatabases(tb, suffix), tb, sources, apply);

  const drops: Array<() => Promise<void>> = [];
  let prevOverride: string | undefined;
  let overridePinned = false;
  const cleanup = async (): Promise<void> => {
    await closeAllDatabases();
    for (let i = drops.length - 1; i >= 0; i--) {
      await drops[i]();
    }
    restoreEnv(ENV_DATABASE_BINDINGS, prevDatabaseBindings);
    if (overridePinned) {
      setDatabaseBindingOverride(prevOverride);
      overridePinned = false;
    }
  };

  const prevDatabaseBindings = process.env[ENV_DATABASE_BINDINGS];
  try {
    for (const p of plans) {
      drops.push(await provisionOne(p, sources, apply));
    }
    const binding = runtimeBinding(plans);
    // Inject the runtime binding so the workload's own database(datasource)
    // connects to the isolated databases for the duration of the suite. It is
    // pinned as the programmatic override too, because the override — unlike
    // the env var — also outranks a managed `database` config-section binding
    // present in the test environment (config wins over env otherwise).
    const raw = JSON.stringify(binding);
    process.env[ENV_DATABASE_BINDINGS] = raw;
    prevOverride = setDatabaseBindingOverride(raw);
    overridePinned = true;

    // Fresh path, decided per plan: a plan cloned from a template is already
    // migrated, every other one is migrated in place now. Runs where only some
    // plans got a template (a datasource the bundle has no migrations for gets
    // none) must therefore neither skip the remaining plans nor re-apply the
    // cloned ones; `applyInto` is a no-op for plans no source targets.
    const fresh = plans.filter((p) => !p.template).map((p) => p.name);
    if (apply && fresh.length > 0) {
      await applyInto(binding, sources, new Set(fresh));
    }
    return { binding, cleanup };
  } catch (err) {
    await cleanup();
    throw err;
  }
}

async function provisionOne(p: Plan, sources: readonly SQLSource[], apply: boolean): Promise<() => Promise<void>> {
  if (p.isolation === 'database') {
    return provisionDatabase(p, sources, apply);
  }
  return provisionSchema(p, apply);
}

async function provisionDatabase(p: Plan, sources: readonly SQLSource[], apply: boolean): Promise<() => Promise<void>> {
  const admin = await openAdmin(p.entry.connection as DatabaseConnectionSpec);
  // keepDatabases: DROP DATABASE forces an immediate cluster-wide checkpoint
  // and waits for it, so on a busy shared server its cost tracks the whole
  // fleet's writes, not the dropped database. A binding whose server dies with
  // the run declares keepDatabases and the teardown becomes a no-op.
  const drop = p.keep
    ? async (): Promise<void> => {}
    : dropDatabase(p.entry.connection as DatabaseConnectionSpec, p.isoDB as string);
  let created = false;

  try {
    // A binding that keeps its databases runs on a server that dies with the
    // run, so there is nothing to reclaim and no DROP worth its checkpoint.
    if (!p.keep && p.isoPrefix) {
      await reclaimOrphans(pgOrphanServer(admin, quoteIdent), p.isoPrefix, new Date(), lockKey);
    }
    if (p.template) {
      await ensureTemplate(admin, p, sources);
      await admin.unsafe(`CREATE DATABASE ${quoteIdent(p.isoDB as string)} TEMPLATE ${quoteIdent(p.template)}`);
      created = true;
      return drop;
    }

    await admin.unsafe(`CREATE DATABASE ${quoteIdent(p.isoDB as string)}`);
    created = true;

    if (apply || (p.isoSchema && p.isoSchema.trim() !== '')) {
      const target = cloneConnection(p.entry.connection as DatabaseConnectionSpec);
      setConnectionDatabase(target, p.isoDB as string);
      const t = await openAdmin(target);
      try {
        if (p.isoSchema && p.isoSchema.trim() !== '') {
          await t.unsafe(`CREATE SCHEMA IF NOT EXISTS ${quoteIdent(p.isoSchema)}`);
        }
      } finally {
        await t.end();
      }
    }
    return drop;
  } catch (err) {
    if (created) {
      await drop().catch(() => {});
    }
    throw err;
  } finally {
    await admin.end().catch(() => {});
  }
}

async function provisionSchema(p: Plan, _apply: boolean): Promise<() => Promise<void>> {
  const admin = await openAdmin(p.entry.connection as DatabaseConnectionSpec);
  try {
    await admin.unsafe(`CREATE SCHEMA ${quoteIdent(p.isoSchema as string)}`);
  } finally {
    await admin.end().catch(() => {});
  }
  return async () => {
    const a = await openAdmin(p.entry.connection as DatabaseConnectionSpec);
    try {
      await a.unsafe(`DROP SCHEMA IF EXISTS ${quoteIdent(p.isoSchema as string)} CASCADE`);
    } finally {
      await a.end();
    }
  };
}

/**
 * ensureTemplate creates and migrates the template database exactly once across
 * concurrent suites/processes, serialized on a Postgres advisory lock held on a
 * single dedicated connection.
 */
async function ensureTemplate(admin: postgres.Sql, p: Plan, sources: readonly SQLSource[]): Promise<void> {
  const key = lockKey(p.template as string);
  await admin.unsafe(`SELECT pg_advisory_lock(${key})`);
  let created = false;
  try {
    const rows = await admin.unsafe(
      `SELECT 1 FROM pg_database WHERE datname = '${(p.template as string).replace(/'/g, "''")}'`,
    );
    if (rows.length > 0) {
      return;
    }
    await admin.unsafe(`CREATE DATABASE ${quoteIdent(p.template as string)}`);
    created = true;
    const templateConn = cloneConnection(p.entry.connection as DatabaseConnectionSpec);
    setConnectionDatabase(templateConn, p.template as string);
    if (p.entry.schema && p.entry.schema.trim() !== '') {
      const t = await openAdmin(templateConn);
      try {
        await t.unsafe(`CREATE SCHEMA IF NOT EXISTS ${quoteIdent(p.entry.schema)}`);
      } finally {
        await t.end();
      }
    }
    const targetBinding: DatabaseBinding = {
      protocolVersion: DATABASE_PROTOCOL_VERSION,
      databases: { [p.name]: { engine: 'postgres', schema: p.entry.schema, connection: templateConn } },
    };
    await applyInto(targetBinding, sources, new Set([p.name]));
    created = false;
  } catch (err) {
    if (created) {
      await admin.unsafe(`DROP DATABASE IF EXISTS ${quoteIdent(p.template as string)} WITH (FORCE)`).catch(() => {});
    }
    throw err;
  } finally {
    await admin.unsafe(`SELECT pg_advisory_unlock(${key})`);
  }
}

/**
 * applyInto applies the migration sources targeting `datasources` against the
 * databases in `targetBinding`, by injecting that binding into DATABASE_BINDINGS
 * — and pinning it as the programmatic override, which also outranks a managed
 * `database` config-section binding — for the duration, so each per-datasource
 * Migrator (which connects through the factory) reaches the right database. It
 * restores the prior binding and closes the factory connections it opened
 * before returning, so applies never leak across targets.
 */
async function applyInto(
  targetBinding: DatabaseBinding,
  sources: readonly SQLSource[],
  datasources: ReadonlySet<string>,
): Promise<void> {
  const prev = process.env[ENV_DATABASE_BINDINGS];
  const raw = JSON.stringify(targetBinding);
  process.env[ENV_DATABASE_BINDINGS] = raw;
  const prevOverride = setDatabaseBindingOverride(raw);
  try {
    const selected = sourcesTargeting(sources, datasources);
    if (selected.length > 0) {
      const registry = new MigrationRegistry();
      for (const s of selected) {
        registry.addSource(s);
      }
      registry.registerRunner(new SQLRunner({ registry, autoApply: true }));
      await registry.applyAll({ force: true });
    }
  } finally {
    await closeAllDatabases();
    restoreEnv(ENV_DATABASE_BINDINGS, prev);
    setDatabaseBindingOverride(prevOverride);
  }
}

/**
 * sourcesTargeting selects the migration sources aimed at one of `datasources`.
 * The single place that predicate lives: both the per-plan template decision and
 * the apply itself go through it, so a plan can never be given a template the
 * apply would leave empty.
 */
function sourcesTargeting(sources: readonly SQLSource[], datasources: ReadonlySet<string>): readonly SQLSource[] {
  return sources.filter((s) => datasources.has(s.datasource));
}

function readEnvBinding(): DatabaseTestBinding | undefined {
  const raw = process.env[ENV_TEST_BINDING]?.trim();
  if (!raw) {
    return undefined;
  }
  return parseTestBinding(raw);
}

/** openAdmin builds a small postgres.js connection for DDL on the given connection. */
async function openAdmin(conn: DatabaseConnectionSpec): Promise<postgres.Sql> {
  const { config, connectionParams } = connectionToConfig(conn);
  const full = { ...ADMIN_DEFAULTS, ...config } as InferConfig<typeof PostgresConfig>;
  const options = await buildOptions(full);
  applyConnectionParams(options, undefined, connectionParams);
  return postgres({ ...options, max: 1 });
}

/** dropDatabase returns a teardown that drops the per-suite database. The reused template is left warm. */
function dropDatabase(baseConn: DatabaseConnectionSpec, name: string): () => Promise<void> {
  return async () => {
    const admin = await openAdmin(baseConn);
    try {
      await admin.unsafe(`DROP DATABASE IF EXISTS ${quoteIdent(name)} WITH (FORCE)`);
    } finally {
      await admin.end();
    }
  };
}

/** bundleDigest computes a stable digest of the migration sources to key reused templates. */
export function bundleDigest(sources: readonly SQLSource[]): string {
  const { operations } = collectBundleContributions(sources);
  const normalized = normalizeBundle({ appName: 'testprovider', operations });
  return computeBundleDigest(normalized);
}

/** connectionDatabase reports the physical database a connection targets. */
export function connectionDatabase(conn: DatabaseConnectionSpec): string {
  if (conn.database?.trim()) {
    return conn.database.trim();
  }
  if (conn.dsn?.trim()) {
    try {
      return new URL(conn.dsn).pathname.replace(/^\//, '');
    } catch {
      return '';
    }
  }
  return '';
}

/**
 * setConnectionDatabase swaps the database a connection targets. Structured
 * connections set the database field; a DSN connection rewrites the URL path.
 */
export function setConnectionDatabase(conn: DatabaseConnectionSpec, db: string): void {
  if (conn.dsn?.trim()) {
    const url = new URL(conn.dsn);
    url.pathname = `/${db}`;
    conn.dsn = url.toString();
    return;
  }
  conn.database = db;
}

function cloneConnection(conn: DatabaseConnectionSpec): DatabaseConnectionSpec {
  return { ...conn, params: conn.params ? { ...conn.params } : undefined };
}

/**
 * pgIdent builds a lowercase Postgres identifier from a base, a tag ("t" for a
 * per-suite database/schema, "tmpl" for a reused template), and a suffix,
 * sanitizing illegal characters and clamping to Postgres' 63-byte limit.
 */
export function pgIdent(base: string, tag: string, suffix: string): string {
  let clean = sanitizeIdent(base) || 'pn';
  const tail = `_${tag}_${suffix}`;
  if (clean.length + tail.length > 63 && 63 - tail.length > 0) {
    clean = clean.slice(0, 63 - tail.length);
  }
  return clean + tail;
}

/**
 * isolatedPrefix is the part of every per-suite database name that precedes the
 * suffix for one base: pgIdent clamps the base so the tail survives, and the
 * clamp depends only on the tail's length.
 */
export function isolatedPrefix(base: string): string {
  return pgIdent(base, 't', '0'.repeat(SUFFIX_LEN)).slice(0, -SUFFIX_LEN);
}

function sanitizeIdent(s: string): string {
  return s
    .toLowerCase()
    .trim()
    .replace(/[^a-z0-9_]/g, '_');
}

/** quoteIdent renders a sanitized identifier for DDL, defending against injection. */
function quoteIdent(name: string): string {
  return `"${name.replace(/"/g, '""')}"`;
}

/** fnv1a is the 32-bit FNV-1a hash, stable across runs and runtimes. */
function fnv1a(s: string): number {
  let h = 2_166_136_261;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 16_777_619);
  }
  return h >>> 0;
}

/** fnv1aHex renders the hash as 8 fixed-width hex chars, safe inside an identifier. */
function fnv1aHex(s: string): string {
  return fnv1a(s).toString(16).padStart(8, '0');
}

/**
 * lockKey hashes a template name into the positive int range pg_advisory_lock
 * accepts. Because template names are per datasource, so are these locks.
 */
function lockKey(name: string): number {
  return Math.abs(fnv1a(name) | 0);
}

function firstNonEmpty(...values: Array<string | undefined>): string {
  for (const v of values) {
    if (v && v.trim() !== '') {
      return v;
    }
  }
  return '';
}
