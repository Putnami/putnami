import { describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { runInContext } from '@putnami/runtime';
import { closeAllDatabases, database } from '../factory';
import { Column, Key, Outcome, Repository, type RotateSpec, runInTransaction, Table } from '../index';
import type { TableDefinition, TableField } from '../table';
import { provision } from '../test-provider';

// This module is the exported TypeScript half of the cross-language transaction
// / concurrency conformance corpus. A downstream test file opts
// into the full corpus with a single committed line:
//
//   import { registerConformanceTests } from '@putnami/database/conformance';
//   registerConformanceTests();
//
// The Go runner (go.putnami.dev/database/conformance) executes the identical
// manifest, so both adapters run the same ordered scenario list and must report
// the identical typed Outcome. The corpus drives the tx/CAS primitives directly
// — compareAndSet / consumeOnce / rotate wrapped in runInTransaction — NOT
// through the HTTP middleware, because that is where cross-language Outcome
// parity is exact.

// The single source of truth for the corpus lives in
// protocols/transaction/conformance/manifest.json; this package ships a
// byte-identical committed copy alongside this runner (guarded against drift by
// test/conformance-drift.test.ts) so the exported runner never depends on a
// repo-relative path into another module.
const MANIFEST_PATH = join(__dirname, 'manifest.json');

type Json = string | number | boolean | null;

interface ManifestColumn {
  name: string;
  type: 'text' | 'boolean' | 'integer';
  primaryKey?: boolean;
  notNull?: boolean;
  default?: string;
}
interface ManifestTable {
  name: string;
  columns: ManifestColumn[];
  seed?: Record<string, Json>[];
}
interface ManifestArgs {
  keyColumn?: string;
  key?: Json;
  column?: string;
  expected?: Json;
  next?: Json;
  claimGuard?: string;
  set?: string;
  predecessorKey?: Json;
  stateColumn?: string;
  revoked?: Json;
  successorColumns?: string[];
  successorValues?: Json[];
}
interface ManifestOperation {
  id: string;
  table: string;
  primitive: 'compare-and-set' | 'consume-once' | 'rotate';
  boundary?: 'none' | 'transaction' | 'nested-transaction';
  fault?: 'none' | 'callback-error';
  repeat?: number;
  args: ManifestArgs;
  expect?: Outcome;
}
interface ManifestConcurrency {
  id: string;
  operations: string[];
  expect: Record<string, number>;
}
interface ManifestRowAssert {
  table: string;
  where: string;
  count: number;
}
interface ManifestCase {
  id: string;
  level: string;
  summary?: string;
  setup: { tables: ManifestTable[] };
  operations: ManifestOperation[];
  concurrency?: ManifestConcurrency[];
  asserts?: ManifestRowAssert[];
}
interface Manifest {
  protocol: string;
  suite: string;
  protocolVersion: number;
  cases: ManifestCase[];
}

function loadManifest(): Manifest {
  return JSON.parse(readFileSync(MANIFEST_PATH, 'utf8')) as Manifest;
}

// Sentinel boundary errors. The runner rolls a transaction back by throwing one
// of these inside runInTransaction; a boundary that throws one is an EXPECTED
// rollback (a conflict, or an injected fault), not a runner failure. Any other
// error propagates.
const INJECTED_FAULT = new Error('conformance: injected callback fault');
const ROLLBACK_NON_APPLIED = new Error('conformance: rollback because outcome was not applied');
function isExpectedRollback(error: unknown): boolean {
  return error === INJECTED_FAULT || error === ROLLBACK_NON_APPLIED;
}

function quoteIdent(name: string): string {
  return `"${name.replace(/"/g, '""')}"`;
}

function pgColumnType(type: ManifestColumn['type']): string {
  if (type === 'boolean') return 'BOOLEAN';
  if (type === 'integer') return 'INTEGER';
  return 'TEXT';
}

function createTableSQL(tbl: ManifestTable): string {
  const cols = tbl.columns.map((col) => {
    let part = `${quoteIdent(col.name)} ${pgColumnType(col.type)}`;
    if (col.primaryKey) part += ' PRIMARY KEY';
    if (col.notNull) part += ' NOT NULL';
    if (col.default) part += ` DEFAULT ${col.default}`;
    return part;
  });
  return `CREATE TABLE ${quoteIdent(tbl.name)} (${cols.join(', ')})`;
}

// insertSQL renders a seed INSERT with deterministically ordered columns (sorted
// by name), mirroring the Go runner so the two runners emit the same statement.
function insertSQL(table: string, row: Record<string, Json>): { query: string; args: Json[] } {
  const keys = Object.keys(row).sort();
  const cols = keys.map(quoteIdent).join(', ');
  const placeholders = keys.map((_, i) => `$${i + 1}`).join(', ');
  const args = keys.map((k) => row[k] as Json);
  return { query: `INSERT INTO ${quoteIdent(table)} (${cols}) VALUES (${placeholders})`, args };
}

// buildRepository builds a Repository bound to the table name and provisioned
// datasource. The CAS primitives only read the table name and datasource from the
// schema (they build raw SQL fragments), so the column definitions merely mirror
// the manifest for faithfulness.
function buildRepository(tbl: ManifestTable, dsName: string): Repository<TableDefinition> {
  const schema: Record<string, TableField> = {};
  for (const col of tbl.columns) {
    const primitive = col.type === 'boolean' ? Boolean : col.type === 'integer' ? Number : String;
    schema[col.name] = col.primaryKey ? Key(primitive) : Column(primitive);
  }
  const table = Table(tbl.name, schema, { db: dsName });
  return new Repository(table);
}

async function callPrimitive(repo: Repository<TableDefinition>, op: ManifestOperation): Promise<Outcome> {
  const a = op.args;
  switch (op.primitive) {
    case 'compare-and-set':
      return repo.compareAndSet(a.keyColumn as string, a.key, a.column as string, a.expected, a.next);
    case 'consume-once':
      return repo.consumeOnce(a.keyColumn as string, a.key, a.claimGuard as string, a.set as string);
    case 'rotate':
      return repo.rotateRow({
        keyColumn: a.keyColumn as string,
        predecessorKey: a.predecessorKey,
        stateColumn: a.stateColumn as string,
        expected: a.expected,
        revoked: a.revoked,
        successorColumns: a.successorColumns as string[],
        successorValues: a.successorValues as unknown[],
      } satisfies RotateSpec);
    default:
      throw new Error(`unknown primitive ${op.primitive}`);
  }
}

// runInBoundary runs the primitive inside a runInTransaction boundary. The
// boundary commits iff the primitive returned Outcome.Applied and no fault is
// injected; otherwise it rolls back (a conflict must not half-commit; an injected
// fault must release the connection). For a nested boundary the primitive runs in
// an INNER runInTransaction joined to the OUTER (join-outer, no savepoints), and
// the outer governs the shared fate — so an outer rollback undoes the inner write.
async function runInBoundary(
  repo: Repository<TableDefinition>,
  op: ManifestOperation,
  nested: boolean,
): Promise<Outcome> {
  let outcome: Outcome | undefined;
  const finalize = (): void => {
    if (op.fault === 'callback-error') throw INJECTED_FAULT;
    if (outcome !== Outcome.Applied) throw ROLLBACK_NON_APPLIED;
  };
  try {
    await runInContext({}, async () => {
      if (nested) {
        await runInTransaction(async () => {
          await runInTransaction(async () => {
            outcome = await callPrimitive(repo, op); // inner joins the outer (no savepoint)
          });
          finalize(); // the OUTER decides commit/rollback for the joined write
        });
      } else {
        await runInTransaction(async () => {
          outcome = await callPrimitive(repo, op);
          finalize();
        });
      }
    });
  } catch (error) {
    if (!isExpectedRollback(error)) throw error;
  }
  if (outcome === undefined) {
    throw new Error(`operation ${op.id} produced no outcome`);
  }
  return outcome;
}

async function runOnce(repo: Repository<TableDefinition>, op: ManifestOperation): Promise<Outcome> {
  switch (op.boundary ?? 'none') {
    case 'none':
      return callPrimitive(repo, op);
    case 'transaction':
      return runInBoundary(repo, op, false);
    case 'nested-transaction':
      return runInBoundary(repo, op, true);
    default:
      throw new Error(`unknown boundary ${op.boundary}`);
  }
}

// runOperation runs one operation repeat times (default 1), returning the final
// outcome. The repeated-failure case keeps re-applying against a rolled-back row.
async function runOperation(repo: Repository<TableDefinition>, op: ManifestOperation): Promise<Outcome> {
  const repeat = op.repeat && op.repeat > 0 ? op.repeat : 1;
  let outcome: Outcome | undefined;
  for (let i = 0; i < repeat; i++) {
    outcome = await runOnce(repo, op);
  }
  return outcome as Outcome;
}

async function runCase(c: ManifestCase, dsName: string): Promise<void> {
  const sql = await database(dsName);
  const repos = new Map<string, Repository<TableDefinition>>();

  for (const tbl of c.setup.tables) {
    await sql.unsafe(`DROP TABLE IF EXISTS ${quoteIdent(tbl.name)}`);
    await sql.unsafe(createTableSQL(tbl));
    for (const row of tbl.seed ?? []) {
      const { query, args } = insertSQL(tbl.name, row);
      await sql.unsafe(query, args as unknown as never);
    }
    repos.set(tbl.name, buildRepository(tbl, dsName));
  }

  const opByID = new Map(c.operations.map((op) => [op.id, op]));
  const concurrent = new Set<string>();
  for (const g of c.concurrency ?? []) {
    for (const id of g.operations) concurrent.add(id);
  }

  // Sequential operations (those carrying an expected Outcome) first, in listed
  // order; concurrency groups afterwards, in parallel.
  for (const op of c.operations) {
    if (concurrent.has(op.id)) continue;
    const repo = repos.get(op.table);
    if (!repo) throw new Error(`operation ${op.id} references unknown table ${op.table}`);
    const outcome = await runOperation(repo, op);
    expect(outcome, `operation ${op.id}`).toBe(op.expect as Outcome);
  }

  for (const g of c.concurrency ?? []) {
    const outcomes = await Promise.all(
      g.operations.map((id) => {
        const op = opByID.get(id);
        if (!op) throw new Error(`concurrency group ${g.id} references unknown operation ${id}`);
        const repo = repos.get(op.table);
        if (!repo) throw new Error(`operation ${op.id} references unknown table ${op.table}`);
        return runOperation(repo, op);
      }),
    );
    const got: Record<string, number> = {};
    for (const o of outcomes) got[o] = (got[o] ?? 0) + 1;
    expect(got, `concurrency group ${g.id}`).toEqual(g.expect);
  }

  for (const a of c.asserts ?? []) {
    const rows = await sql.unsafe<{ n: number | string }[]>(
      `SELECT COUNT(*)::int AS n FROM ${quoteIdent(a.table)} WHERE ${a.where}`,
    );
    expect(Number(rows[0]?.n ?? -1), `assert ${a.table} WHERE ${a.where}`).toBe(a.count);
  }
}

// registerConformanceTests emits the transaction conformance corpus describe/it
// blocks into the calling bun test file. Live provisioning against an externally
// provided Postgres runs only when DATABASE_TEST_BINDINGS is injected (CI service
// container / local server), so unit runs pay no database cost; the non-gated
// "loads the shared manifest" case always runs, keeping the corpus structurally
// checked even without a database.
export function registerConformanceTests(): void {
  const manifest = loadManifest();
  const hasBinding = !!process.env['DATABASE_TEST_BINDINGS']?.trim();

  describe('transaction conformance corpus (live)', () => {
    it('loads the shared manifest', () => {
      expect(manifest.protocol).toBe('putnami.transaction.v1');
      expect(manifest.suite).toBe('putnami.transaction.conformance.v1');
      expect(manifest.cases.length).toBeGreaterThan(0);
    });

    it.skipIf(!hasBinding)('Go and TypeScript pass the same transaction and concurrency corpus', async () => {
      const result = await provision();
      try {
        const names = Object.keys(result.binding.databases ?? {}).sort();
        expect(names.length).toBeGreaterThan(0);
        const dsName = names[0] as string;

        for (const c of manifest.cases) {
          await runCase(c, dsName);
        }
      } finally {
        await closeAllDatabases();
        await result.cleanup();
      }
    });
  });
}
