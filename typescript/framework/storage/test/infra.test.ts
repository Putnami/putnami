import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { Bucket } from '../src/bucket/bucket.builders';
import { bucketRegistry } from '../src/bucket/bucket.registry';
import { buildStorageInfraManifest, writeStorageInfraManifest } from '../src/infra';

const SCHEMA_URL = 'https://putnami.dev/schemas/putnami-infra.json';

afterEach(() => {
  bucketRegistry.clear();
});

describe('buildStorageInfraManifest', () => {
  specTest(
    'returns undefined when no buckets are registered',
    {
      feature: 'typescript/object-storage',
      requirement: 'declared-infrastructure',
      check: 'no-registered-bucket-emits-nothing',
    },
    () => {
      expect(buildStorageInfraManifest(bucketRegistry.getAll())).toBeUndefined();
    },
  );

  specTest(
    'emits a single registered bucket defaulting access to readwrite',
    {
      feature: 'typescript/object-storage',
      requirement: 'declared-infrastructure',
      check: 'a-registered-bucket-defaults-its-access-level',
    },
    () => {
      Bucket('avatars');

      expect(buildStorageInfraManifest(bucketRegistry.getAll())).toEqual({
        $schema: SCHEMA_URL,
        protocolVersion: 2,
        storage: [{ name: 'avatars', access: 'readwrite' }],
      });
    },
  );

  specTest(
    'preserves an explicit access level and public flag',
    {
      feature: 'typescript/object-storage',
      requirement: 'declared-infrastructure',
      check: 'an-explicit-access-level-and-public-flag-survive',
    },
    () => {
      Bucket('public-assets', { access: 'read', public: true });

      expect(buildStorageInfraManifest(bucketRegistry.getAll())?.storage).toEqual([
        { name: 'public-assets', access: 'read', public: true },
      ]);
    },
  );

  specTest(
    'emits one sorted entry per bucket name for a multi-bucket registry',
    {
      feature: 'typescript/object-storage',
      requirement: 'declared-infrastructure',
      check: 'entries-are-sorted-one-per-bucket',
    },
    () => {
      Bucket('invoices');
      Bucket('avatars');
      Bucket('exports');

      const manifest = buildStorageInfraManifest(bucketRegistry.getAll());

      expect(manifest?.storage).toEqual([
        { name: 'avatars', access: 'readwrite' },
        { name: 'exports', access: 'readwrite' },
        { name: 'invoices', access: 'readwrite' },
      ]);
    },
  );

  specTest(
    'includes the retention string when a bucket declares one',
    {
      feature: 'typescript/object-storage',
      requirement: 'declared-infrastructure',
      check: 'a-declared-retention-is-included',
    },
    () => {
      Bucket('archive', { retention: '30d' });

      expect(buildStorageInfraManifest(bucketRegistry.getAll())?.storage).toEqual([
        { name: 'archive', access: 'readwrite', retention: '30d' },
      ]);
    },
  );

  it('omits retention for buckets without (or with blank) retention', () => {
    Bucket('avatars');
    Bucket('blank', { retention: '   ' });
    Bucket('archive', { retention: '90d' });

    expect(buildStorageInfraManifest(bucketRegistry.getAll())?.storage).toEqual([
      { name: 'archive', access: 'readwrite', retention: '90d' },
      { name: 'avatars', access: 'readwrite' },
      { name: 'blank', access: 'readwrite' },
    ]);
  });
});

describe('writeStorageInfraManifest', () => {
  let projectRoot: string;

  beforeEach(async () => {
    projectRoot = await mkdtemp(join(tmpdir(), 'putnami-storage-infra-'));
  });

  afterEach(async () => {
    await rm(projectRoot, { recursive: true, force: true });
  });

  specTest(
    'writes the sidecar manifest at .gen/infra/storage.json',
    {
      feature: 'typescript/object-storage',
      requirement: 'declared-infrastructure',
      check: 'the-sidecar-is-written-at-its-declared-path',
    },
    async () => {
      Bucket('avatars', { retention: '30d' });
      Bucket('invoices');

      const path = await writeStorageInfraManifest(projectRoot, bucketRegistry.getAll());

      expect(path).toBe(join(projectRoot, '.gen/infra/storage.json'));
      const written = await readFile(path!, 'utf8');
      expect(written.endsWith('\n')).toBe(true);
      expect(JSON.parse(written)).toEqual({
        $schema: SCHEMA_URL,
        protocolVersion: 2,
        storage: [
          { name: 'avatars', access: 'readwrite', retention: '30d' },
          { name: 'invoices', access: 'readwrite' },
        ],
      });
    },
  );

  it('writes nothing and returns undefined when no buckets are registered', async () => {
    const path = await writeStorageInfraManifest(projectRoot, bucketRegistry.getAll());

    expect(path).toBeUndefined();
    expect(await Bun.file(join(projectRoot, '.gen/infra/storage.json')).exists()).toBe(false);
  });

  specTest(
    'removes a stale sidecar when the registry becomes empty',
    {
      feature: 'typescript/object-storage',
      requirement: 'declared-infrastructure',
      check: 'a-stale-sidecar-is-removed-when-the-registry-empties',
    },
    async () => {
      const sidecarPath = join(projectRoot, '.gen/infra/storage.json');
      await Bun.write(sidecarPath, '{"protocolVersion":2,"storage":[{"name":"old"}]}\n');

      const path = await writeStorageInfraManifest(projectRoot, bucketRegistry.getAll());

      expect(path).toBeUndefined();
      expect(await Bun.file(sidecarPath).exists()).toBe(false);
    },
  );
});
