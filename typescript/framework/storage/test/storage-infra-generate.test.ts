import { afterAll, afterEach, beforeEach, describe, expect, it, mock } from 'bun:test';
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

// Absolute path to the real Bucket() builder so a generated fixture registers
// into the same bucketRegistry instance this test inspects.
const BUILDERS_PATH = join(import.meta.dir, '../src/bucket/bucket.builders.ts');

let currentProject: { name: string } = { name: 'app' };
let projectRoot = '';
let dependencyGraph = new Map<string, string[]>();

const realRuntime = require('../../runtime/src/index');
const realUtils = require('../../utils/src/index');

mock.module('@putnami/runtime', () => ({
  ...realRuntime,
  useLogger: () => ({ debug() {}, info() {}, warn() {}, error() {} }),
}));

mock.module('@putnami/utils', () => ({
  ...realUtils,
  getCurrentProject: () => currentProject,
  getProjectRoot: () => projectRoot,
  listProjectDependencies: (name: string) => dependencyGraph.get(name) ?? [name],
}));

const { storagePlugin } = await import('../src/storage.plugin');
const { bucketRegistry } = await import('../src/bucket/bucket.registry');

async function writeSource(relPath: string, contents: string) {
  const abs = join(projectRoot, relPath);
  await mkdir(join(abs, '..'), { recursive: true });
  await writeFile(abs, contents, 'utf8');
}

async function readSidecar() {
  return JSON.parse(await readFile(join(projectRoot, '.gen/infra/storage.json'), 'utf8'));
}

beforeEach(async () => {
  projectRoot = await mkdtemp(join(tmpdir(), 'putnami-storage-gen-'));
  currentProject = { name: 'app' };
  dependencyGraph = new Map([['app', ['app']]]);
  bucketRegistry.clear();
});

afterEach(async () => {
  bucketRegistry.clear();
  await rm(projectRoot, { recursive: true, force: true });
});

afterAll(() => {
  // Bun's mock.restore() does not undo mock.module(). Reinstall the complete
  // modules so code first loaded after this suite cannot inherit its stale
  // projectRoot or logger overrides.
  mock.module('@putnami/runtime', () => realRuntime);
  mock.module('@putnami/utils', () => realUtils);
});

describe('storagePlugin generate() lifecycle', () => {
  it('registers buckets declared in local files and emits them to the sidecar', async () => {
    // The fixture is NOT imported by this test — generate() must discover it,
    // wire it into the loader, and import it to populate the registry.
    await writeSource(
      'src/buckets.ts',
      [
        '// declares buckets for @putnami/storage',
        `import { Bucket } from ${JSON.stringify(BUILDERS_PATH)};`,
        "Bucket('avatars', { retention: '30d' });",
        "Bucket('invoices');",
        '',
      ].join('\n'),
    );

    expect(bucketRegistry.getNames()).toEqual([]);

    const result = await storagePlugin().generate?.({} as never);

    expect(result).toEqual({ exports: { 'storage-loader': join(projectRoot, '.gen/src/.storage.gen.ts') } });
    expect(bucketRegistry.getNames().sort()).toEqual(['avatars', 'invoices']);
    expect(await readSidecar()).toEqual({
      $schema: 'https://putnami.dev/schemas/putnami-infra.json',
      protocolVersion: 2,
      storage: [
        { name: 'avatars', access: 'readwrite', retention: '30d' },
        { name: 'invoices', access: 'readwrite' },
      ],
    });
  });

  it('emits no sidecar when the project declares no buckets', async () => {
    await writeSource('src/index.ts', "export const noop = () => 'no buckets here';\n");

    const result = await storagePlugin().generate?.({} as never);

    expect(result).toEqual({});
    expect(await Bun.file(join(projectRoot, '.gen/infra/storage.json')).exists()).toBe(false);
  });
});

describe('storagePlugin generate() with several instances', () => {
  // The loader is project-wide: it reads the project's dependencies and source
  // files, never the instance. Several storagePlugin() instances therefore emit
  // the same key, path, and bytes, and the build keeping only the last export
  // under `storage-loader` loses nothing, unlike api(), static() and events().
  it('emits the same storage-loader key, path, and bytes from every instance', async () => {
    await writeSource(
      'src/buckets.ts',
      [
        '// declares buckets for @putnami/storage',
        `import { Bucket } from ${JSON.stringify(BUILDERS_PATH)};`,
        "Bucket('avatars');",
        '',
      ].join('\n'),
    );
    const loaderPath = join(projectRoot, '.gen/src/.storage.gen.ts');

    const first = await storagePlugin().generate?.({} as never);
    const firstBytes = await readFile(loaderPath, 'utf8');
    const second = await storagePlugin().generate?.({} as never);

    expect(first).toEqual({ exports: { 'storage-loader': loaderPath } });
    expect(second).toEqual(first);
    expect(await readFile(loaderPath, 'utf8')).toBe(firstBytes);
  });
});
