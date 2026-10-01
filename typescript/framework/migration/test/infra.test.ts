import { afterEach, beforeEach, describe, expect, test } from 'bun:test';
import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import {
  buildInfraManifest,
  emitInfraRequirements,
  INFRA_PER_PROJECT_SCHEMA_URL,
  INFRA_PROTOCOL_VERSION,
  INFRA_SIDECAR_FILENAME,
  type InfraDatabaseRequirement,
  type InfraEngine,
  infraRequirements,
  infraSidecarPath,
  type Kind,
  KindSQL,
  MigrationRegistry,
  type MigrationSource,
  mergeInfraDatabases,
  type PerProjectInfraManifest,
  writeInfraSidecar,
} from '../src';

// --- Test doubles ---------------------------------------------------------

/** A source that contributes one (name, engine, schema) infra requirement. */
function infraSource(kind: Kind, namespace: string, db: InfraDatabaseRequirement): MigrationSource {
  return { kind, namespace, infraDatabase: () => db };
}

/** A source of some kind that contributes nothing to infra. */
function plainSource(kind: Kind, namespace: string): MigrationSource {
  return { kind, namespace };
}

function sqlSource(namespace: string, datasource: string): MigrationSource {
  return infraSource(KindSQL, namespace, { name: datasource, engine: 'postgres', schemas: [namespace] });
}

// --- buildInfraManifest ---------------------------------------------------

describe('buildInfraManifest', () => {
  specTest(
    'zero sources contributes nothing',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'infra-aggregation',
      check: 'zero-sources-contribute-nothing',
    },
    () => {
      expect(buildInfraManifest([])).toBeUndefined();
    },
  );

  specTest(
    'sources without an infra footprint are skipped',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'infra-aggregation',
      check: 'a-source-without-an-infra-footprint-is-skipped',
    },
    () => {
      expect(buildInfraManifest([plainSource('cache', 'sessions')])).toBeUndefined();
    },
  );

  specTest(
    'one SQL source emits a single database entry',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'infra-aggregation',
      check: 'one-sql-source-emits-one-database-entry',
    },
    () => {
      const manifest = buildInfraManifest([sqlSource('iam', 'primary')]);
      expect(manifest).toEqual({
        $schema: INFRA_PER_PROJECT_SCHEMA_URL,
        protocolVersion: INFRA_PROTOCOL_VERSION,
        databases: [{ name: 'primary', engine: 'postgres', schemas: ['iam'] }],
      });
    },
  );

  specTest(
    'two sources for the same database union and sort their schemas',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'infra-aggregation',
      check: 'two-sources-for-one-database-union-and-sort-their-schemas',
    },
    () => {
      const manifest = buildInfraManifest([sqlSource('iam', 'primary'), sqlSource('audit', 'primary')]);
      expect(manifest?.databases).toEqual([{ name: 'primary', engine: 'postgres', schemas: ['audit', 'iam'] }]);
    },
  );

  specTest(
    'cross-kind sources with different engines emit separate entries',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'infra-aggregation',
      check: 'different-engines-emit-separate-entries',
    },
    () => {
      const documentSource = infraSource('document', 'docs', {
        name: 'primary',
        engine: 'sqlite',
        schemas: ['docs'],
      });
      const manifest = buildInfraManifest([sqlSource('iam', 'primary'), documentSource]);
      // Same name, different engine → keyed separately, sorted by engine.
      expect(manifest?.databases).toEqual([
        { name: 'primary', engine: 'postgres', schemas: ['iam'] },
        { name: 'primary', engine: 'sqlite', schemas: ['docs'] },
      ]);
    },
  );
});

// --- mergeInfraDatabases --------------------------------------------------

describe('mergeInfraDatabases', () => {
  specTest(
    'deduplicates schemas and sorts databases by (name, engine)',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'infra-aggregation',
      check: 'schemas-are-de-duplicated-and-databases-sorted',
    },
    () => {
      const merged = mergeInfraDatabases([
        { name: 'wealth', engine: 'postgres', schemas: ['ledger'] },
        { name: 'primary', engine: 'postgres', schemas: ['iam', 'iam'] },
        { name: 'primary', engine: 'postgres', schemas: ['audit'] },
      ]);
      expect(merged).toEqual([
        { name: 'primary', engine: 'postgres', schemas: ['audit', 'iam'] },
        { name: 'wealth', engine: 'postgres', schemas: ['ledger'] },
      ]);
    },
  );

  test('omits schemas when none are contributed', () => {
    const merged = mergeInfraDatabases([{ name: 'primary', engine: 'postgres' as InfraEngine }]);
    expect(merged).toEqual([{ name: 'primary', engine: 'postgres' }]);
  });
});

// --- sidecar emission -----------------------------------------------------

describe('emitInfraRequirements', () => {
  let projectRoot: string;

  beforeEach(() => {
    projectRoot = mkdtempSync(join(tmpdir(), 'putnami-infra-'));
  });

  afterEach(() => {
    rmSync(projectRoot, { recursive: true, force: true });
  });

  specTest(
    'writes an atomic, parseable sidecar at .gen/infra/migration.json',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'infra-aggregation',
      check: 'the-sidecar-is-written-atomically-and-parseably',
    },
    () => {
      const manifest = emitInfraRequirements([sqlSource('iam', 'primary')], projectRoot);
      expect(manifest).toBeDefined();

      const path = infraSidecarPath(projectRoot);
      expect(path).toBe(join(projectRoot, '.gen/infra/migration.json'));
      expect(existsSync(path)).toBe(true);

      const raw = readFileSync(path, 'utf8');
      expect(raw.endsWith('\n')).toBe(true);
      expect(JSON.parse(raw)).toEqual(manifest as PerProjectInfraManifest);
      // No torn temp file left behind.
      expect(existsSync(`${path}.tmp`)).toBe(false);
    },
  );

  specTest(
    'is idempotent — re-emitting the same input yields identical bytes',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'infra-aggregation',
      check: 're-emitting-the-same-input-yields-identical-bytes',
    },
    () => {
      const sources = [sqlSource('iam', 'primary'), sqlSource('audit', 'primary')];
      emitInfraRequirements(sources, projectRoot);
      const first = readFileSync(infraSidecarPath(projectRoot), 'utf8');
      emitInfraRequirements([...sources].reverse(), projectRoot);
      const second = readFileSync(infraSidecarPath(projectRoot), 'utf8');
      expect(second).toBe(first);
    },
  );

  specTest(
    'removes a stale sidecar when nothing is declared',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'infra-aggregation',
      check: 'a-stale-sidecar-is-removed-when-nothing-is-declared',
    },
    () => {
      const path = infraSidecarPath(projectRoot);
      writeInfraSidecar(projectRoot, { protocolVersion: INFRA_PROTOCOL_VERSION, databases: [] });
      expect(existsSync(path)).toBe(true);

      const manifest = emitInfraRequirements([plainSource('cache', 'sessions')], projectRoot);
      expect(manifest).toBeUndefined();
      expect(existsSync(path)).toBe(false);
    },
  );

  test('tolerates removing an absent sidecar', () => {
    expect(emitInfraRequirements([], projectRoot)).toBeUndefined();
    expect(existsSync(infraSidecarPath(projectRoot))).toBe(false);
  });
});

// --- sidecar atomicity / cleanup ------------------------------------------

describe('writeInfraSidecar atomicity', () => {
  let projectRoot: string;

  beforeEach(() => {
    projectRoot = mkdtempSync(join(tmpdir(), 'putnami-infra-atomic-'));
  });

  afterEach(() => {
    rmSync(projectRoot, { recursive: true, force: true });
  });

  const manifest = (): PerProjectInfraManifest => ({
    protocolVersion: INFRA_PROTOCOL_VERSION,
    databases: [{ name: 'primary', engine: 'postgres', schemas: ['iam'] }],
  });

  test('rethrows and removes the temp file when renameSync fails', () => {
    const finalPath = infraSidecarPath(projectRoot);
    // Make the destination a NON-EMPTY directory so renaming the temp file
    // onto it fails (POSIX: rename(file -> non-empty dir) → ENOTEMPTY/EISDIR).
    // This exercises the catch → rmSync(tmp) → rethrow branch deterministically
    // without monkeypatching node:fs.
    mkdirSync(finalPath, { recursive: true });
    writeFileSync(join(finalPath, 'occupied'), 'x');

    expect(() => writeInfraSidecar(projectRoot, manifest())).toThrow();

    // The destination directory is untouched (rename never succeeded) ...
    expect(readdirSync(finalPath)).toEqual(['occupied']);
    // ... and no torn `.tmp` file is leaked in the sidecar directory.
    const sidecarDir = join(projectRoot, '.gen/infra');
    const leaked = readdirSync(sidecarDir).filter((name) => name.endsWith('.tmp'));
    expect(leaked).toEqual([]);
  });

  test('concurrent writers of identical input converge on one valid sidecar with no temp leak', () => {
    const sources = [sqlSource('iam', 'primary'), sqlSource('audit', 'primary')];
    const built = buildInfraManifest(sources) as PerProjectInfraManifest;

    // Per-call random temp names mean concurrent writers never clobber each
    // other's temp file; the last successful rename wins and the bytes are
    // identical regardless of ordering.
    const paths = Array.from({ length: 8 }, () => writeInfraSidecar(projectRoot, built));
    expect(new Set(paths).size).toBe(1);

    const finalPath = infraSidecarPath(projectRoot);
    expect(JSON.parse(readFileSync(finalPath, 'utf8'))).toEqual(built);

    const sidecarDir = join(projectRoot, '.gen/infra');
    expect(readdirSync(sidecarDir)).toEqual([INFRA_SIDECAR_FILENAME]);
  });
});

// --- registry integration -------------------------------------------------

describe('MigrationRegistry.infraManifest', () => {
  specTest(
    'aggregates infra requirements across contributed sources',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'infra-aggregation',
      check: 'infra-requirements-aggregate-across-contributed-sources',
    },
    () => {
      const registry = new MigrationRegistry();
      registry.addSource(sqlSource('iam', 'primary'));
      registry.addSource(sqlSource('audit', 'primary'));
      expect(registry.infraManifest()?.databases).toEqual([
        { name: 'primary', engine: 'postgres', schemas: ['audit', 'iam'] },
      ]);
    },
  );

  test('returns undefined when no source contributes infra', () => {
    const registry = new MigrationRegistry();
    expect(registry.infraManifest()).toBeUndefined();
  });
});

// --- generate plugin -------------------------------------------------------

describe('infraRequirements plugin', () => {
  let projectRoot: string;

  beforeEach(() => {
    projectRoot = mkdtempSync(join(tmpdir(), 'putnami-infra-plugin-'));
  });

  afterEach(() => {
    rmSync(projectRoot, { recursive: true, force: true });
  });

  test('emits the sidecar from MigrationContributor plugins in the module tree', () => {
    const contributor = { migrationSources: () => [sqlSource('iam', 'primary')] };
    const owner = {
      getRoot: () => ({ collectPlugins: () => [{ plugin: contributor }, { plugin: { name: 'noop' } }] }),
    };

    const plugin = infraRequirements({ projectRoot });
    expect(plugin.generate(owner)).toEqual({});

    const raw = readFileSync(infraSidecarPath(projectRoot), 'utf8');
    expect(JSON.parse(raw).databases).toEqual([{ name: 'primary', engine: 'postgres', schemas: ['iam'] }]);
  });
});
