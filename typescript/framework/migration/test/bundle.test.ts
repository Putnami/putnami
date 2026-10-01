import { describe, expect, it } from 'bun:test';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import {
  BUNDLE_OUTPUT_DIR,
  BUNDLE_PROTOCOL,
  type Bundle,
  type BundleOperation,
  type BundlePayload,
  collectBundleContributions,
  computeBundleDigest,
  DatasourceSchemaConflictError,
  computePayloadHash,
  emitMigrationBundle,
  normalizeBundle,
  normalizeOperation,
  writeBundle,
} from '../src';

const UP_HASH = '1111111111111111111111111111111111111111111111111111111111111111';
const DOWN_HASH = '2222222222222222222222222222222222222222222222222222222222222222';

function sampleBundle(): Bundle {
  return {
    appName: 'wealth',
    version: '1.4.0',
    operations: [
      {
        kind: 'sql',
        target: 'default',
        namespace: 'iam',
        name: 'iam/0001_create_users',
        up: { path: 'payload/sql/default/iam/0001_create_users.up.sql', hash: UP_HASH },
        down: { path: 'payload/sql/default/iam/0001_create_users.down.sql', hash: DOWN_HASH },
        safety: 'safe-online',
      },
    ],
  };
}

describe('migration bundle', () => {
  it('normalizes defaults and infers reversibility from a down payload', () => {
    const op = normalizeOperation({
      kind: 'sql',
      target: '',
      name: 'app/0001',
      up: { path: 'a.up.sql', hash: UP_HASH },
      down: { path: 'a.down.sql', hash: DOWN_HASH },
      safety: '',
    });
    expect(op.target).toBe('default');
    expect(op.orderKey).toBe('app/0001');
    expect(op.safety).toBe('safe-online');
    expect(op.capabilities?.reversible).toBe(true);
  });

  specTest(
    'computes a digest that ignores release provenance and input order',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'cross-language-bundle',
      check: 'the-digest-ignores-release-provenance-and-input-order',
    },
    () => {
      const a = sampleBundle();
      a.version = '1.4.0';
      a.git = { revision: 'abc' };
      a.generatedAt = '2026-06-04T12:00:00Z';

      const b = sampleBundle();
      b.version = '1.5.0';
      b.git = { revision: 'totally-different' };
      b.generatedAt = '2026-07-01T00:00:00Z';
      b.operations.push({
        kind: 'sql',
        target: 'default',
        namespace: 'billing',
        name: 'billing/0002',
        up: { path: 'payload/sql/default/billing/0002.up.sql', hash: DOWN_HASH },
        safety: 'safe-online',
      });
      a.operations.push(b.operations[1]);
      // Reverse a's order so the two differ only by input ordering + provenance.
      a.operations.reverse();

      expect(computeBundleDigest(a)).toBe(computeBundleDigest(b));
    },
  );

  it('defaults the protocol identifier', () => {
    expect(normalizeBundle(sampleBundle()).protocol).toBe(BUNDLE_PROTOCOL);
  });

  specTest(
    'writes a self-contained bundle that round-trips',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'cross-language-bundle',
      check: 'a-bundle-round-trips-self-contained',
    },
    async () => {
      const dir = await mkdtemp(join(tmpdir(), 'bundle-'));
      try {
        const up = 'CREATE TABLE iam.users (id uuid primary key);\n';
        const upPath = 'payload/sql/default/iam/0001.up.sql';
        const bundle: Bundle = {
          appName: 'svc',
          operations: [
            {
              kind: 'sql',
              target: 'default',
              name: 'iam/0001',
              up: { path: upPath, hash: computePayloadHash(up) },
              safety: 'safe-online',
            },
          ],
        };
        await writeBundle(dir, bundle, [{ path: upPath, bytes: up }]);

        const manifest = JSON.parse(await readFile(join(dir, 'bundle.json'), 'utf8')) as Bundle;
        expect(manifest.digest).toBe(computeBundleDigest(bundle));
        expect(await readFile(join(dir, upPath), 'utf8')).toBe(up);
      } finally {
        await rm(dir, { recursive: true, force: true });
      }
    },
  );

  specTest(
    'rejects a payload path that escapes the bundle root',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'cross-language-bundle',
      check: 'a-payload-path-escaping-the-bundle-root-is-rejected',
    },
    async () => {
      const dir = await mkdtemp(join(tmpdir(), 'bundle-'));
      try {
        const up = 'CREATE TABLE iam.users (id uuid primary key);\n';
        const upPath = '../escape.sql';
        const bundle: Bundle = {
          appName: 'svc',
          operations: [
            {
              kind: 'sql',
              target: 'default',
              name: 'iam/0001',
              up: { path: upPath, hash: computePayloadHash(up) },
              safety: 'safe-online',
            },
          ],
        };
        await expect(writeBundle(dir, bundle, [{ path: upPath, bytes: up }])).rejects.toThrow(/clean relative path/);
      } finally {
        await rm(dir, { recursive: true, force: true });
      }
    },
  );

  specTest(
    'rejects a payload whose bytes do not match the pinned hash',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'cross-language-bundle',
      check: 'a-payload-whose-bytes-do-not-match-the-pinned-hash-is-rejected',
    },
    async () => {
      const dir = await mkdtemp(join(tmpdir(), 'bundle-'));
      try {
        const upPath = 'payload/sql/default/iam/0001.up.sql';
        const bundle: Bundle = {
          appName: 'svc',
          operations: [
            {
              kind: 'sql',
              target: 'default',
              name: 'iam/0001',
              up: { path: upPath, hash: computePayloadHash('expected') },
              safety: 'safe-online',
            },
          ],
        };
        await expect(writeBundle(dir, bundle, [{ path: upPath, bytes: 'tampered' }])).rejects.toThrow();
      } finally {
        await rm(dir, { recursive: true, force: true });
      }
    },
  );
});

// A minimal source-level MigrationBundleContributor (mirrors database.SQLSource).
function bundleSource(operations: BundleOperation[], payloads: BundlePayload[]) {
  return { migrationBundleOperations: () => ({ operations, payloads }) };
}

describe('source-driven bundle emission', () => {
  const up = 'CREATE TABLE iam_users (id uuid primary key);';
  const upPath = 'payload/sql/default/iam/0001.up.sql';
  const op: BundleOperation = {
    kind: 'sql',
    target: 'default',
    namespace: 'iam',
    name: 'iam/0001',
    up: { path: upPath, hash: computePayloadHash(up) },
    safety: 'safe-online',
  };

  it('collects contributions from sources, skipping non-contributors', () => {
    const { operations, payloads } = collectBundleContributions([
      bundleSource([op], [{ path: upPath, bytes: up }]),
      { name: 'not-a-contributor' },
    ]);
    expect(operations).toEqual([op]);
    expect(payloads).toEqual([{ path: upPath, bytes: up }]);
  });

  it('writes a bundle under .gen/migration-bundle and removes a stale one when empty', async () => {
    const root = await mkdtemp(join(tmpdir(), 'emit-bundle-'));
    try {
      const written = await emitMigrationBundle([bundleSource([op], [{ path: upPath, bytes: up }])], 'svc', root);
      expect(written?.digest).toMatch(/^[a-f0-9]{64}$/);
      const dir = join(root, BUNDLE_OUTPUT_DIR);
      const manifest = JSON.parse(await readFile(join(dir, 'bundle.json'), 'utf8')) as Bundle;
      expect(manifest.appName).toBe('svc');
      expect(manifest.operations).toHaveLength(1);
      expect(await readFile(join(dir, upPath), 'utf8')).toBe(up);

      // A later build with no contributions clears the stale bundle.
      const cleared = await emitMigrationBundle([{ name: 'noop' }], 'svc', root);
      expect(cleared).toBeUndefined();
      await expect(readFile(join(dir, 'bundle.json'), 'utf8')).rejects.toThrow();
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  });

  it('refuses to write a bundle whose sources put one datasource in two schemas', async () => {
    const root = await mkdtemp(join(tmpdir(), 'emit-bundle-conflict-'));
    try {
      const inSchema = (namespace: string, schema: string): BundleOperation => ({
        ...op,
        target: 'marketing',
        schema,
        namespace,
        name: `${namespace}/0001`,
        up: { path: `payload/sql/marketing/${namespace}/0001.up.sql`, hash: computePayloadHash(up) },
      });
      const app = inSchema('app', 'marketing');
      const analytics = inSchema('putnami-analytics', 'public');
      // Another kind on the same target does not share the SQL search path.
      const document: BundleOperation = { ...inSchema('docs', 'docs'), kind: 'document' };
      // A bundle an earlier build wrote, which the refused build must not leave.
      await emitMigrationBundle([bundleSource([op], [{ path: upPath, bytes: up }])], 'svc', root);

      const emit = emitMigrationBundle(
        [
          bundleSource(
            [app, document],
            [
              { path: app.up.path, bytes: up },
              { path: document.up.path, bytes: up },
            ],
          ),
          bundleSource([analytics], [{ path: analytics.up.path, bytes: up }]),
        ],
        'svc',
        root,
      );

      await expect(emit).rejects.toThrow(DatasourceSchemaConflictError);
      await expect(emit).rejects.toThrow(
        'datasource "marketing" has conflicting schemas across sources: "marketing" and "public" (namespaces "app" and "putnami-analytics")',
      );
      await expect(readFile(join(root, BUNDLE_OUTPUT_DIR, 'bundle.json'), 'utf8')).rejects.toThrow();

      const written = await emitMigrationBundle(
        [
          bundleSource(
            [app, document],
            [
              { path: app.up.path, bytes: up },
              { path: document.up.path, bytes: up },
            ],
          ),
        ],
        'svc',
        root,
      );
      expect(written?.operations).toHaveLength(2);
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  });
});
