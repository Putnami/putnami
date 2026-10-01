import { describe, expect } from 'bun:test';
import { readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { Uuid } from '@putnami/runtime';
import { specTest } from '@putnami/spectest';
import { buildInfraRequirements } from '../src/infra';
import { Key, Table } from '../src/table';

// EQUIVALENCE_FIXTURE_PATH is the shared golden file the Go database producer
// asserts against too. Both languages writing identical bytes to that fixture
// is the test surface the audit's S1 finding would have caught (a silent shape
// divergence between the two implementations).
const EQUIVALENCE_FIXTURE_PATH = join(
  __dirname,
  '../../../../protocols/infra/fixtures/equivalence/database.golden.json',
);

describe('cross-language equivalence', () => {
  // Counterpart: go/framework/database/cross_language_test.go.
  // Same conceptual inputs, same golden output. A change to either
  // implementation that breaks byte-equivalence fails both tests.
  specTest(
    'database producer matches the shared golden fixture',
    {
      feature: 'typescript/relational-persistence',
      requirement: 'cross-language-conformance',
      check: 'the-producer-matches-the-shared-golden-fixture',
    },
    async () => {
      const id = { id: Key(Uuid) };
      // A pool declared across two tables with different schemas (sorted union),
      // a pool with no schema (the omitted field), and three distinct pool names
      // so the deterministic name sort is exercised on both sides. Table names
      // are irrelevant to the manifest — only the pool (db) and schema are.
      const tables = [
        Table('primary_users', id, { db: 'primary', schema: 'iam' }),
        Table('analytics_reports', id, { db: 'analytics', schema: 'reporting' }),
        Table('analytics_events', id, { db: 'analytics', schema: 'events' }),
        Table('cache_entries', id, { db: 'cache' }),
      ];

      const manifest = buildInfraRequirements(tables);
      const got = `${JSON.stringify(manifest, null, 2)}\n`;

      const want = await readFile(EQUIVALENCE_FIXTURE_PATH, 'utf8');

      expect(got).toBe(want);
    },
  );
});
