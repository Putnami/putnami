import { describe, expect } from 'bun:test';
import { readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import {
  type Bundle,
  buildInfraManifest,
  computeBundleDigest,
  type InfraDatabaseRequirement,
  type Kind,
  KindSQL,
  type MigrationSource,
} from '../src';

// EQUIVALENCE_FIXTURE_PATH is the shared golden file the Go migration producer
// asserts against too. Both languages writing identical bytes to that fixture
// is the test surface the audit's S1 finding would have caught (a silent shape
// divergence between the two implementations).
const EQUIVALENCE_FIXTURE_PATH = join(
  __dirname,
  '../../../../protocols/infra/fixtures/equivalence/migration.golden.json',
);

// BUNDLE_FIXTURE_PATH is the shared golden bundle the Go protocol package
// asserts against too (protocols/migration/bundle_equivalence_test.go).
const BUNDLE_FIXTURE_PATH = join(__dirname, '../../../../protocols/migration/fixtures/equivalence/bundle.golden.json');

// GOLDEN_BUNDLE_DIGEST mirrors goldenBundleDigest in the Go test. Both languages
// computing this exact value for the same fixture is the parity guarantee.
const GOLDEN_BUNDLE_DIGEST = '6533cedd09aa95f9622ec420f675782c4ad34a7646702ed7ba9a0918d1feb86e';

// COMPATIBLE_BUNDLE_FIXTURE_PATH is the shared golden bundle whose operations all
// set the `compatible` capability — the marker a Go producer writes for an
// expand/contract migration an environment may roll back across.
const COMPATIBLE_BUNDLE_FIXTURE_PATH = join(
  __dirname,
  '../../../../protocols/migration/fixtures/equivalence/bundle-compatible.golden.json',
);

// GOLDEN_COMPATIBLE_BUNDLE_DIGEST mirrors goldenCompatibleBundleDigest in the Go
// test. A capability the canonical form here does not rebuild would split the two
// digests silently, which is exactly what this pin catches.
const GOLDEN_COMPATIBLE_BUNDLE_DIGEST = '2884a81ce4de0e7e28f776b46c370c3ec1c7bbd1cb1bf2c8149ce61e2c23a2bc';

/** A migration source that contributes one (name, engine, schema) requirement. */
function infraSource(kind: Kind, namespace: string, db: InfraDatabaseRequirement): MigrationSource {
  return { kind, namespace, infraDatabase: () => db };
}

describe('cross-language equivalence', () => {
  // Counterpart: go/framework/migration/cross_language_test.go.
  // Same conceptual inputs, same golden output. A change to either
  // implementation that breaks byte-equivalence fails both tests.
  specTest(
    'migration producer matches the shared golden fixture',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'cross-language-bundle',
      check: 'the-producer-output-matches-the-shared-golden-fixture',
    },
    async () => {
      // Two sources target (primary, postgres) with different schemas (sorted
      // union), plus a second database, ordered out of sort so the deterministic
      // (name, engine) sort is exercised on both sides.
      const sources = [
        infraSource(KindSQL, 'audit', { name: 'primary', engine: 'postgres', schemas: ['audit'] }),
        infraSource(KindSQL, 'iam', { name: 'primary', engine: 'postgres', schemas: ['iam'] }),
        infraSource(KindSQL, 'ledger', { name: 'wealth', engine: 'postgres', schemas: ['ledger'] }),
      ];

      const manifest = buildInfraManifest(sources);
      const got = `${JSON.stringify(manifest, null, 2)}\n`;

      const want = await readFile(EQUIVALENCE_FIXTURE_PATH, 'utf8');

      expect(got).toBe(want);
    },
  );

  // Counterpart: protocols/migration/bundle_equivalence_test.go.
  // The bundle digest is the content address used for idempotent publish, so
  // the TS and Go implementations must compute it byte-for-byte identically.
  specTest(
    'bundle digest matches the shared golden fixture',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'cross-language-bundle',
      check: 'the-bundle-digest-matches-the-shared-golden-fixture',
    },
    async () => {
      const bundle = JSON.parse(await readFile(BUNDLE_FIXTURE_PATH, 'utf8')) as Bundle;

      // Recomputing from the fixture (with operations deliberately out of
      // canonical order) must reproduce the Go-computed digest exactly...
      expect(computeBundleDigest(bundle)).toBe(GOLDEN_BUNDLE_DIGEST);
      // ...and match the digest the fixture itself embeds.
      expect(bundle.digest).toBe(GOLDEN_BUNDLE_DIGEST);
    },
  );

  // Counterpart: TestCrossLanguage_CompatibleBundleDigest in
  // protocols/migration/bundle_equivalence_test.go. The canonical operation form
  // is rebuilt field by field here, so every capability Go can write has to be
  // written here too, in Go declaration order.
  specTest(
    'bundle digest matches Go for a bundle that sets compatible',
    {
      feature: 'typescript/migration-orchestration',
      requirement: 'compatible-marker-parity',
      check: 'compatible-bundle-digest-matches-go',
    },
    async () => {
      const bundle = JSON.parse(await readFile(COMPATIBLE_BUNDLE_FIXTURE_PATH, 'utf8')) as Bundle;

      for (const op of bundle.operations) {
        expect(op.capabilities?.compatible).toBe(true);
      }
      expect(computeBundleDigest(bundle)).toBe(GOLDEN_COMPATIBLE_BUNDLE_DIGEST);
      expect(bundle.digest).toBe(GOLDEN_COMPATIBLE_BUNDLE_DIGEST);

      // Dropping the marker has to move the digest: an implementation that
      // ignored `compatible` would pass the assertions above by accident.
      const without: Bundle = {
        ...bundle,
        operations: bundle.operations.map((op) => ({
          ...op,
          capabilities: { ...op.capabilities, compatible: undefined },
        })),
      };
      expect(computeBundleDigest(without)).not.toBe(GOLDEN_COMPATIBLE_BUNDLE_DIGEST);
    },
  );
});
