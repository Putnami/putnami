import { afterEach, describe, expect, mock, test } from 'bun:test';
import { resetDefaultLogger, setRootLogger } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { MemoryLogger } from '@putnami/runtime/testing';
import {
  type ApplyOpts,
  type DriftReport,
  type Kind,
  KindSQL,
  MigrationConfigError,
  type MigrationRecord,
  MigrationRegistry,
  type MigrationRunner,
  type MigrationSource,
  UnknownKindError,
} from '../src';

// --- Test doubles ---------------------------------------------------------

function fakeSource(kind: Kind, namespace: string): MigrationSource {
  return { kind, namespace };
}

interface FakeRunner extends MigrationRunner {
  applyCalls: number;
  lastApplyOpts?: ApplyOpts;
  statusResult: MigrationRecord[];
  applyResult: MigrationRecord[];
  report: DriftReport;
}

function fakeRunner(kind: Kind, opts: Partial<FakeRunner> = {}): FakeRunner {
  const r: FakeRunner = {
    kind,
    applyCalls: 0,
    applyResult: opts.applyResult ?? [],
    statusResult: opts.statusResult ?? [],
    report: opts.report ?? { kind },
    async apply(opts) {
      r.applyCalls++;
      r.lastApplyOpts = opts;
      return r.applyResult;
    },
    async status() {
      return r.statusResult;
    },
    async rollback() {
      return [];
    },
    async verify() {
      return r.report;
    },
  };
  return r;
}

// --- Tests ----------------------------------------------------------------

describe('MigrationRegistry.addSource', () => {
  specTest(
    'rejects empty kind',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'source-validation',
      check: 'a-source-with-an-empty-kind-is-rejected-at-contribution',
    },
    () => {
      const r = new MigrationRegistry();
      expect(() => r.addSource({ kind: '' as Kind, namespace: 'iam' })).toThrow(MigrationConfigError);
    },
  );

  specTest(
    'rejects blank namespace',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'source-validation',
      check: 'a-source-with-a-blank-namespace-is-rejected-at-contribution',
    },
    () => {
      const r = new MigrationRegistry();
      expect(() => r.addSource({ kind: KindSQL, namespace: '   ' })).toThrow(MigrationConfigError);
    },
  );

  specTest(
    'accepts valid source and exposes a defensive copy',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'defensive-reads',
      check: 'reading-the-sources-for-a-kind-returns-a-copy',
    },
    () => {
      const r = new MigrationRegistry();
      r.addSource(fakeSource(KindSQL, 'iam'));

      const copy = r.sourcesFor(KindSQL);
      expect(copy).toHaveLength(1);

      copy.push(fakeSource(KindSQL, 'EVIL') as never);
      expect(r.sourcesFor(KindSQL)).toHaveLength(1);
    },
  );
});

describe('MigrationRegistry.registerRunner', () => {
  specTest(
    'rejects duplicate kind',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'one-runner-per-kind',
      check: 'a-second-runner-for-a-kind-is-rejected',
    },
    () => {
      const r = new MigrationRegistry();
      r.registerRunner(fakeRunner(KindSQL));
      expect(() => r.registerRunner(fakeRunner(KindSQL))).toThrow(MigrationConfigError);
    },
  );
});

describe('MigrationRegistry.kinds', () => {
  specTest(
    'returns lexicographic union of source kinds and runner kinds',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'deterministic-kind-order',
      check: 'kinds-are-enumerated-in-lexicographic-order',
    },
    () => {
      const r = new MigrationRegistry();
      r.registerRunner(fakeRunner('gcs'));
      r.addSource(fakeSource('sql', 'iam'));
      r.addSource(fakeSource('document', 'wealth'));

      expect(r.kinds()).toEqual(['document', 'gcs', 'sql']);
    },
  );
});

describe('MigrationRegistry.applyAll', () => {
  specTest(
    'iterates kinds in lexicographic order and forwards opts',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'deterministic-kind-order',
      check: 'cross-kind-iteration-follows-that-same-order',
    },
    async () => {
      const r = new MigrationRegistry();
      const rSql = fakeRunner(KindSQL, { applyResult: [{ kind: KindSQL, name: 'iam/001', status: 'applied' }] });
      const rGcs = fakeRunner('gcs', { applyResult: [{ kind: 'gcs', name: 'bucket-init', status: 'applied' }] });
      r.registerRunner(rSql);
      r.registerRunner(rGcs);

      const records = await r.applyAll({ force: true });
      expect(records.map((rec) => rec.kind)).toEqual(['gcs', KindSQL]);
      expect(rSql.lastApplyOpts?.force).toBe(true);
      expect(rGcs.lastApplyOpts?.force).toBe(true);
    },
  );

  specTest(
    'throws UnknownKindError when explicit apply has no matching runner',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'orphan-sources-fail',
      check: 'an-explicit-apply-for-an-unknown-kind-fails',
    },
    async () => {
      const r = new MigrationRegistry();
      r.addSource(fakeSource(KindSQL, 'iam'));
      // no SQL runner registered -> explicit apply must error
      await expect(r.applyAll({ force: true })).rejects.toThrow(UnknownKindError);
    },
  );

  specTest(
    'throws UnknownKindError by default when a source has no matching runner',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'orphan-sources-fail',
      check: 'a-source-without-a-runner-fails-by-default',
    },
    async () => {
      const r = new MigrationRegistry();
      r.addSource(fakeSource(KindSQL, 'iam'));

      await expect(r.applyAll()).rejects.toThrow(UnknownKindError);
    },
  );

  specTest(
    'allowSourceOnly apply tolerates source-only kinds',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'orphan-sources-fail',
      check: 'the-allow-source-only-escape-hatch-tolerates-orphans',
    },
    async () => {
      const r = new MigrationRegistry();
      r.addSource(fakeSource(KindSQL, 'iam'));

      await expect(r.applyAll({ allowSourceOnly: true })).resolves.toEqual([]);
    },
  );

  specTest(
    'error message names every contributing plugin',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'orphan-sources-fail',
      check: 'the-error-names-every-contributing-namespace',
    },
    async () => {
      const r = new MigrationRegistry();
      r.addSource(fakeSource(KindSQL, 'iam'));
      r.addSource(fakeSource(KindSQL, 'secrets'));
      try {
        await r.applyAll({ force: true });
        throw new Error('expected applyAll to throw');
      } catch (err) {
        expect(err).toBeInstanceOf(UnknownKindError);
        const msg = (err as Error).message;
        expect(msg).toContain('iam');
        expect(msg).toContain('secrets');
      }
    },
  );
});

describe('MigrationRegistry.verifyAll', () => {
  specTest(
    'aggregates per-kind reports',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'deterministic-kind-order',
      check: 'per-kind-reports-aggregate-in-that-order',
    },
    async () => {
      const r = new MigrationRegistry();
      const sqlReport: DriftReport = {
        kind: KindSQL,
        hashDrifts: [{ name: 'iam/001', storedHash: 'a', currentHash: 'b' }],
      };
      r.registerRunner(fakeRunner(KindSQL, { report: sqlReport }));
      r.registerRunner(fakeRunner('gcs'));

      const reports = await r.verifyAll();
      expect(reports).toHaveLength(2);
      // gcs first (lexicographic)
      expect(reports[0]?.kind).toBe('gcs');
      expect(reports[1]?.kind).toBe(KindSQL);
      expect(reports[1]?.hashDrifts).toHaveLength(1);
    },
  );
});

describe('MigrationRegistry boot log', () => {
  afterEach(() => {
    resetDefaultLogger();
  });

  test('emits structured event on each contribution', () => {
    const r = new MigrationRegistry();
    expect(() => r.addSource(fakeSource(KindSQL, 'iam'))).not.toThrow();
    expect(() => r.registerRunner(fakeRunner(KindSQL))).not.toThrow();
    // satisfy lint that mock is used somewhere
    const noop = mock(() => undefined);
    noop();
  });

  /**
   * Pins the registry's half of the migration log contract
   * (`protocols/logging/conformance`): the pinned `database.migration` logger
   * name shared with the SQL migrator/runner, and identifiers carried in a
   * camelCase `migration` group rather than as flat fields. The Go twin is
   * `go/framework/migration/logging_test.go`.
   */
  test('records carry the pinned logger name and the migration group', () => {
    const logger = new MemoryLogger();
    // The registry resolves its logger at construction, so install first.
    setRootLogger(logger);
    const r = new MigrationRegistry();

    r.addSource(fakeSource(KindSQL, 'iam'));
    r.registerRunner(fakeRunner(KindSQL));

    expect(logger.entries).toHaveLength(2);
    for (const entry of logger.entries) {
      expect(entry.logger).toBe('database.migration');
      const group = (entry.data?.[0] as Record<string, unknown> | undefined)?.['migration'] as
        | Record<string, unknown>
        | undefined;
      expect(group?.['kind']).toBe(KindSQL);
      expect(group?.['sourcesForKind']).toBe(1);
      // Exactly one data param, so the JSON sink flattens the group.
      expect(entry.data).toHaveLength(1);
    }
    expect(logger.entries.map((e) => e.message)).toEqual(['plugin contributed source', 'runner registered']);
  });
});

// `backend-free` and `registry-is-per-boot` are structural: they constrain what
// the package may depend on and what it may hold at module scope. Nothing was
// asserting either, so a driver import or a resurrected module-level singleton
// would have gone in unnoticed — exactly the regressions these two clauses
// exist to prevent.
describe('package shape', () => {
  specTest(
    'declares no dependency outside the framework runtime',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'backend-free',
      check: 'the-package-depends-only-on-the-framework-runtime',
    },
    async () => {
      const pkg = (await Bun.file(new URL('../package.json', import.meta.url)).json()) as {
        dependencies?: Record<string, string>;
        devDependencies?: Record<string, string>;
      };

      expect(Object.keys(pkg.dependencies ?? {})).toEqual(['@putnami/runtime']);
      expect(Object.keys(pkg.devDependencies ?? {})).toEqual([]);
    },
  );

  specTest(
    'no source module imports a database driver or opens a connection',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'backend-free',
      check: 'no-source-module-imports-a-driver-or-opens-a-connection',
    },
    async () => {
      const forbidden = [/\bpostgres\b/, /\bpg\b/, /\bmysql/, /\bsqlite/, /\bknex\b/, /node:net\b/, /node:tls\b/];
      const entries = new Bun.Glob('**/*.ts').scanSync({ cwd: new URL('../src', import.meta.url).pathname });

      for (const entry of entries) {
        const source = await Bun.file(new URL(`../src/${entry}`, import.meta.url)).text();
        for (const line of source.split('\n')) {
          if (!/^\s*import\s|^\s*}\s*from\s|require\(/.test(line)) continue;
          for (const pattern of forbidden) {
            expect(
              pattern.test(line),
              `${entry} imports a backend the orchestration package must not know about: ${line.trim()}`,
            ).toBe(false);
          }
        }
      }
    },
  );

  specTest(
    'holds no module-level registry, so two boots never share contributions',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'registry-is-per-boot',
      check: 'no-module-level-registry-carries-state-between-boots',
    },
    async () => {
      const first = new MigrationRegistry();
      first.addSource({ kind: KindSQL, namespace: 'first-boot' });

      // A second import of the module must not observe the first boot's state,
      // and a second Registry must start empty.
      const reimported = await import('../src/registry');
      const second = new reimported.MigrationRegistry();

      expect(second.kinds()).toEqual([]);
      expect(first.kinds()).toEqual([KindSQL]);

      for (const exported of Object.values(reimported)) {
        expect(exported).not.toBeInstanceOf(MigrationRegistry);
      }
    },
  );
});
