import { afterEach, describe, expect, it } from 'bun:test';
import { readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { Bucket } from '../src/bucket/bucket.builders';
import { bucketRegistry } from '../src/bucket/bucket.registry';
import { buildStorageInfraManifest } from '../src/infra';

// EQUIVALENCE_FIXTURE_PATH is the shared golden file the Go storage
// producer asserts against too. Both languages writing identical bytes to
// that fixture is the test surface the audit's S1 finding would have
// caught (TypeScript stamped protocolVersion 1 while Go required 2).
const EQUIVALENCE_FIXTURE_PATH = join(
  __dirname,
  '../../../../protocols/infra/fixtures/equivalence/storage.golden.json',
);

afterEach(() => {
  bucketRegistry.clear();
});

describe('cross-language equivalence', () => {
  // Counterpart: go/framework/storage/cross_language_test.go.
  // Same conceptual inputs, same golden output. A change to either
  // implementation that breaks byte-equivalence fails both tests.
  it('storage producer matches the shared golden fixture', async () => {
    // Inputs are unsorted on purpose so the test also covers the
    // deterministic name sort, and they mix with-retention and
    // without-retention buckets so the omitempty handling is exercised
    // on both sides.
    Bucket('uploads');
    Bucket('audit-logs', { retention: '30d' });
    Bucket('avatars', { retention: '90d' });

    const manifest = buildStorageInfraManifest(bucketRegistry.getAll());
    const got = `${JSON.stringify(manifest, null, 2)}\n`;

    const want = await readFile(EQUIVALENCE_FIXTURE_PATH, 'utf8');

    expect(got).toBe(want);
  });
});
