import { afterAll, afterEach, beforeEach, describe, expect, it, mock } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { mkdir, readFile, rm } from 'node:fs/promises';
import { join } from 'node:path';

const debugMock = mock(() => {});
const infoMock = mock(() => {});
const warnMock = mock(() => {});
const errorMock = mock(() => {});
const useConfigMock = mock();
const loadRegisteredModuleMock = mock(async () => undefined);
const incCounterMock = mock(() => {});
const observeHistogramMock = mock(() => {});
const setGaugeMock = mock(() => {});

let currentProject: { name: string } | null = { name: 'app' };
let dependencyGraph = new Map<string, string[]>();
let projectRoot = '/tmp/putnami-storage-factory-tests';
let generatedHeads: string[] = [];
let generatedFiles: Array<{ fileName: string; lines: string[] }> = [];

const realRuntime = require('../../runtime/src/index');
const realApplication = require('../../application/src/index');
const realUtils = require('../../utils/src/index');

// Subclass instead of fake from scratch: Bun keeps mock.module installs global
// across test files, so this class is what `@putnami/storage`'s storage.plugin
// will see in OTHER test files in this run too. It must remain a fully-functional
// GeneratorHelper (normalizeImport, real .write(), etc.) — capturing calls is
// purely additive for this file's assertions on `generatedHeads`/`generatedFiles`.
class FakeGeneratorHelper extends realUtils.GeneratorHelper {
  private readonly captured: string[] = [];

  appendHead(line: string) {
    this.captured.push(line);
    generatedHeads.push(line);
    return super.appendHead(line);
  }

  write() {
    generatedFiles.push({
      fileName: (this as unknown as { fileName: string }).fileName,
      lines: [...this.captured],
    });
    return super.write();
  }
}

const FakeHttpPlugin = Symbol('FakeHttpPlugin');
const mockLogger = {
  debug: debugMock,
  info: infoMock,
  warn: warnMock,
  error: errorMock,
};

mock.module('@putnami/runtime', () => ({
  ...realRuntime,
  useConfig: (...args: unknown[]) => useConfigMock(...args),
  useLogger: () => mockLogger,
}));

mock.module('@putnami/application', () => ({
  ...realApplication,
  HttpPlugin: FakeHttpPlugin,
  loadRegisteredModule: (...args: unknown[]) => loadRegisteredModuleMock(...args),
  incCounter: (...args: unknown[]) => incCounterMock(...args),
  observeHistogram: (...args: unknown[]) => observeHistogramMock(...args),
  setGauge: (...args: unknown[]) => setGaugeMock(...args),
}));

mock.module('@putnami/utils', () => ({
  ...realUtils,
  GeneratorHelper: FakeGeneratorHelper,
  getCurrentProject: () => currentProject,
  getProjectRoot: () => projectRoot,
  joinPath: (...parts: string[]) => parts.filter(Boolean).join('/').replace(/\/+/g, '/'),
  listProjectDependencies: (name: string) => dependencyGraph.get(name) ?? [],
}));

const storageModule = await import('../src/index');
const { Bucket } = await import('../src/bucket/bucket.builders');
const { bucketRegistry } = await import('../src/bucket/bucket.registry');
const { FileBackend } = await import('../src/backend/file.backend');
const { MemoryBackend } = await import('../src/backend/memory.backend');
const { closeAllStorage, closeStorage, getStorageClient, storage } = await import('../src/factory');

function defaultConfig(overrides: Record<string, unknown> = {}) {
  return {
    backend: 'memory',
    endpoint: 'https://storage.example.com',
    accessKey: 'access-key',
    dataDir: join(projectRoot, 'data'),
    tokenSecret: 'factory-secret',
    slowOperationThresholdMs: 250,
    ...overrides,
  };
}

async function mountStorageServer(options?: { dataDir?: string; tokenSecret?: string }) {
  const routes = new Map<string, (ctx: Record<string, unknown>) => Promise<unknown>>();
  const httpPlugin = {
    route: mock((method: string, path: string, handler: (ctx: Record<string, unknown>) => Promise<unknown>) => {
      routes.set(`${method} ${path}`, handler);
    }),
  };
  const app = {
    ensurePlugin: mock(async (plugin: unknown) => {
      expect(plugin).toBe(FakeHttpPlugin);
      return httpPlugin;
    }),
  };

  const plugin = storageModule.storageServer(options);
  await plugin.warmup?.(app as never);

  return { app, httpPlugin, plugin, routes };
}

beforeEach(() => {
  dependencyGraph = new Map([['app', ['app']]]);
  currentProject = { name: 'app' };
  projectRoot = join('/tmp', `putnami-storage-${Date.now()}-${crypto.randomUUID()}`);
  generatedHeads = [];
  generatedFiles = [];

  useConfigMock.mockReset();
  useConfigMock.mockImplementation((_schema: unknown, options?: { confInit?: Record<string, unknown> }) =>
    defaultConfig(options?.confInit),
  );
  loadRegisteredModuleMock.mockReset();
  loadRegisteredModuleMock.mockResolvedValue(undefined);
  incCounterMock.mockReset();
  observeHistogramMock.mockReset();
  setGaugeMock.mockReset();
  debugMock.mockReset();
  infoMock.mockReset();
  warnMock.mockReset();
  errorMock.mockReset();

  (globalThis as Record<string, unknown>).__storagePluginLoaded = undefined;
});

afterEach(async () => {
  await closeAllStorage();
  bucketRegistry.clear();
  await rm(projectRoot, { recursive: true, force: true });
  (globalThis as Record<string, unknown>).__storagePluginLoaded = undefined;
});

afterAll(() => {
  mock.restore();
  // mock.restore() only resets function mocks; Bun keeps mock.module() installs
  // process-global across test files. Restore complete module namespaces for
  // code first loaded after this suite.
  mock.module('@putnami/runtime', () => realRuntime);
  mock.module('@putnami/application', () => realApplication);
  mock.module('@putnami/utils', () => realUtils);
});

describe('Package entrypoints', () => {
  it('should re-export the main storage APIs', async () => {
    const backendIndex = await import('../src/backend/index');
    const bucketIndex = await import('../src/bucket/index');
    const clientIndex = await import('../src/client/index');
    const serverIndex = await import('../src/server/index');
    const storageBackendModule = await import('../src/backend/storage.backend');

    expect(storageModule.storage).toBe(storage);
    expect(storageModule.storagePlugin).toBeDefined();
    expect(storageModule.storageServer).toBe(serverIndex.storageServer);
    expect(storageModule.FileBackend).toBe(backendIndex.FileBackend);
    expect(storageModule.MemoryBackend).toBe(backendIndex.MemoryBackend);
    expect(storageModule.RemoteBackend).toBe(backendIndex.RemoteBackend);
    expect(storageModule.Bucket).toBe(bucketIndex.Bucket);
    expect(storageModule.StorageClient).toBe(clientIndex.StorageClient);
    expect(storageBackendModule).toBeObject();
  });

  it('should expose only the curated index via the package exports map', async () => {
    const pkg = JSON.parse(await readFile(join(import.meta.dir, '..', 'package.json'), 'utf8')) as {
      exports?: Record<string, unknown>;
    };
    // An exports map makes index.ts the single public entry point and keeps
    // internal helpers (safePath, generateToken, the token secret) unimportable.
    expect(pkg.exports).toEqual({ '.': './src/index.ts' });
  });
});

describe('Storage metrics', () => {
  it('should record successful operations and slow warnings', () => {
    storageModule.recordStorageOp({
      operation: 'put',
      bucket: 'avatars',
      duration: 42,
      bytes: 5,
      key: 'hello.txt',
    });
    storageModule.recordSlowStorageOp(
      {
        operation: 'put',
        bucket: 'avatars',
        duration: 500,
        key: 'hello.txt',
      },
      250,
    );
    storageModule.recordSlowStorageOp(
      {
        operation: 'put',
        bucket: 'avatars',
        duration: 100,
      },
      250,
    );

    expect(incCounterMock).toHaveBeenCalledWith('storage.put.avatars');
    expect(incCounterMock).toHaveBeenCalledWith('storage.operation.slow');
    expect(observeHistogramMock).toHaveBeenCalledWith('storage.put.avatars.duration', 42);
    expect(observeHistogramMock).toHaveBeenCalledWith('storage.operation.duration', 42);
    expect(observeHistogramMock).toHaveBeenCalledWith('storage.put.bytes', 5);
    expect(debugMock).toHaveBeenCalledWith(
      'put avatars',
      expect.objectContaining({ operation: 'put', bucket: 'avatars', duration: 42, bytes: 5, key: 'hello.txt' }),
    );
    expect(warnMock).toHaveBeenCalledTimes(1);
  });

  it('should record failures and client lifecycle metrics', () => {
    storageModule.recordStorageError('get', 'avatars', 21, new Error('boom'));
    storageModule.recordClientCreated('avatars');
    storageModule.recordClientClosed('avatars');
    storageModule.recordClientCount(3);

    expect(incCounterMock).toHaveBeenCalledWith('storage.get.avatars.error');
    expect(incCounterMock).toHaveBeenCalledWith('storage.operation.error');
    expect(incCounterMock).toHaveBeenCalledWith('storage.client.created');
    expect(incCounterMock).toHaveBeenCalledWith('storage.client.closed');
    expect(observeHistogramMock).toHaveBeenCalledWith('storage.operation.duration', 21);
    expect(errorMock).toHaveBeenCalledWith(
      'get avatars failed',
      expect.objectContaining({ operation: 'get', bucket: 'avatars', duration: 21, error: 'boom' }),
    );
    expect(debugMock).toHaveBeenCalledWith('Storage client created', { bucket: 'avatars' });
    expect(debugMock).toHaveBeenCalledWith('Storage client closed', { bucket: 'avatars' });
    expect(setGaugeMock).toHaveBeenCalledWith('storage.client.count', 3);
  });
});

describe('Factory', () => {
  it('should create and cache clients for registered buckets', async () => {
    Bucket('avatars');

    const first = await storage('avatars');
    const second = await storage('avatars');

    expect(first).toBe(second);
    expect(getStorageClient('avatars')).toBe(first);
    expect(useConfigMock).toHaveBeenCalledTimes(1);
    expect(incCounterMock).toHaveBeenCalledWith('storage.client.created');
    expect(setGaugeMock).toHaveBeenCalledWith('storage.client.count', 1);
  });

  it('should keep explicit config clients out of the managed registry', async () => {
    Bucket('archive', { storage: 'cold' });
    const managed = await storage('archive');
    const explicitConfig = defaultConfig({ backend: 'file', dataDir: join(projectRoot, 'archive-data') });

    const first = await storage('archive', explicitConfig);
    const second = await storage('archive', explicitConfig);

    expect(first).not.toBe(second);
    expect(getStorageClient('archive')).toBe(managed);
    expect(useConfigMock.mock.calls[1]?.[1]).toMatchObject({
      path: 'storage.cold',
      confInit: explicitConfig,
    });
    expect(incCounterMock.mock.calls.filter(([metric]) => metric === 'storage.client.created')).toHaveLength(1);
    expect(setGaugeMock.mock.calls.filter(([metric]) => metric === 'storage.client.count')).toEqual([
      ['storage.client.count', 1],
    ]);
  });

  specTest(
    'should separate file backends that only differ by token secret',
    {
      feature: 'typescript/object-storage',
      requirement: 'backend-lifecycle',
      check: 'file-backends-differing-only-by-secret-are-not-shared',
    },
    async () => {
      Bucket('alpha', { storage: 'alpha' });
      Bucket('beta', { storage: 'beta' });

      useConfigMock.mockImplementation((_schema: unknown, options?: { path?: string }) => {
        if (options?.path === 'storage.alpha') {
          return defaultConfig({ backend: 'file', dataDir: join(projectRoot, 'shared-data'), tokenSecret: 'secret-a' });
        }
        if (options?.path === 'storage.beta') {
          return defaultConfig({ backend: 'file', dataDir: join(projectRoot, 'shared-data'), tokenSecret: 'secret-b' });
        }
        return defaultConfig();
      });

      const alpha = await storage('alpha');
      const beta = await storage('beta');

      expect((alpha as any).backend).not.toBe((beta as any).backend);
    },
  );

  specTest(
    'should separate remote backends that only differ by credentials',
    {
      feature: 'typescript/object-storage',
      requirement: 'backend-lifecycle',
      check: 'remote-backends-differing-only-by-credentials-are-not-shared',
    },
    async () => {
      Bucket('images', { storage: 'images' });
      Bucket('docs', { storage: 'docs' });

      useConfigMock.mockImplementation((_schema: unknown, options?: { path?: string }) => {
        if (options?.path === 'storage.images') {
          return defaultConfig({
            backend: 'remote',
            endpoint: 'https://storage.example.com',
            accessKey: 'access-a',
          });
        }
        if (options?.path === 'storage.docs') {
          return defaultConfig({
            backend: 'remote',
            endpoint: 'https://storage.example.com',
            accessKey: 'access-b',
          });
        }
        return defaultConfig();
      });

      const images = await storage('images');
      const docs = await storage('docs');

      expect((images as any).backend).not.toBe((docs as any).backend);
    },
  );

  specTest(
    'should remove a cached client when closeStorage is called',
    {
      feature: 'typescript/object-storage',
      requirement: 'backend-lifecycle',
      check: 'closing-one-client-removes-only-that-client',
    },
    async () => {
      Bucket('avatars');

      await storage('avatars');
      expect(getStorageClient('avatars')).toBeDefined();

      await closeStorage('avatars');

      expect(getStorageClient('avatars')).toBeUndefined();
      expect(incCounterMock).toHaveBeenCalledWith('storage.client.closed');
      expect(setGaugeMock).toHaveBeenCalledWith('storage.client.count', 0);
    },
  );

  specTest(
    'should only close a shared backend when the last managed client is removed',
    {
      feature: 'typescript/object-storage',
      requirement: 'backend-lifecycle',
      check: 'a-shared-backend-closes-only-with-the-last-managed-client',
    },
    async () => {
      const originalClose = MemoryBackend.prototype.close;
      const closeMock = mock(async function (this: MemoryBackend) {
        await originalClose.call(this);
      });
      MemoryBackend.prototype.close = closeMock;

      try {
        Bucket('avatars');
        Bucket('documents');

        const avatars = await storage('avatars');
        const documents = await storage('documents');
        expect((avatars as any).backend).toBe((documents as any).backend);

        await closeStorage('avatars');
        expect(closeMock).not.toHaveBeenCalled();
        expect(getStorageClient('documents')).toBe(documents);

        await closeStorage('documents');
        expect(closeMock).toHaveBeenCalledTimes(1);
        expect(getStorageClient('documents')).toBeUndefined();
      } finally {
        MemoryBackend.prototype.close = originalClose;
      }
    },
  );

  specTest(
    'should close cached backends when closeAllStorage is called',
    {
      feature: 'typescript/object-storage',
      requirement: 'backend-lifecycle',
      check: 'close-all-closes-every-cached-backend',
    },
    async () => {
      const originalClose = MemoryBackend.prototype.close;
      const closeMock = mock(async function (this: MemoryBackend) {
        await originalClose.call(this);
      });
      MemoryBackend.prototype.close = closeMock;

      try {
        Bucket('avatars');
        await storage('avatars');

        await closeAllStorage();

        expect(closeMock).toHaveBeenCalledTimes(1);
        expect(getStorageClient('avatars')).toBeUndefined();
        expect(setGaugeMock).toHaveBeenCalledWith('storage.client.count', 0);
      } finally {
        MemoryBackend.prototype.close = originalClose;
      }
    },
  );

  it('should throw when a bucket is not registered', async () => {
    await expect(storage('missing')).rejects.toThrow('Bucket "missing" is not registered');
  });

  it('should throw when the config references an unknown backend', async () => {
    Bucket('avatars');
    useConfigMock.mockImplementation(() => defaultConfig({ backend: 'unknown' }));

    await expect(storage('avatars')).rejects.toThrow('Unknown storage backend');
  });
});

describe('storagePlugin', () => {
  it('should skip generation when no dependency uses storage', async () => {
    dependencyGraph.set('app', ['app', 'pkg-a']);
    dependencyGraph.set('pkg-a', ['@putnami/web']);

    const result = await storageModule.storagePlugin().generate?.({} as never);

    expect(result).toEqual({});
    expect(generatedFiles).toHaveLength(0);
  });

  it('should generate a storage loader for dependent packages', async () => {
    dependencyGraph.set('app', ['app', 'pkg-a', 'pkg-b', 'pkg-c']);
    dependencyGraph.set('pkg-a', ['@putnami/storage']);
    dependencyGraph.set('pkg-b', ['@putnami/web']);
    dependencyGraph.set('pkg-c', ['@putnami/storage', '@putnami/runtime']);

    const result = await storageModule.storagePlugin().generate?.({} as never);

    expect(result).toEqual({
      exports: {
        'storage-loader': `${projectRoot}/.gen/src/.storage.gen.ts`,
      },
    });
    expect(generatedHeads).toEqual([
      "// generated by the StoragePlugin exploring 'app'",
      'import "pkg-a";',
      'import "pkg-c";',
    ]);
  });

  it('should emit the infra requirements sidecar from registered buckets', async () => {
    Bucket('avatars', { retention: '30d' });
    Bucket('invoices');

    await storageModule.storagePlugin().generate?.({} as never);

    const sidecar = JSON.parse(await readFile(join(projectRoot, '.gen/infra/storage.json'), 'utf8'));
    expect(sidecar).toEqual({
      $schema: 'https://putnami.dev/schemas/putnami-infra.json',
      protocolVersion: 2,
      storage: [
        { name: 'avatars', retention: '30d', access: 'readwrite' },
        { name: 'invoices', access: 'readwrite' },
      ],
    });
  });

  it('should not emit an infra sidecar when no buckets are registered', async () => {
    await storageModule.storagePlugin().generate?.({} as never);

    expect(await Bun.file(join(projectRoot, '.gen/infra/storage.json')).exists()).toBe(false);
  });

  it('should use the preloaded module during warmup', async () => {
    await storageModule.storagePlugin({ preloadedModule: { buckets: true } }).warmup?.();

    expect(debugMock).toHaveBeenCalledWith('Storage buckets registered (preloaded)');
  });

  it('should use a registered storage loader when one is available', async () => {
    loadRegisteredModuleMock.mockResolvedValue({ loader: true });

    await storageModule.storagePlugin().warmup?.();

    expect(loadRegisteredModuleMock).toHaveBeenCalledWith('storage-loader');
    expect(debugMock).toHaveBeenCalledWith('Storage buckets registered (bundled)');
  });

  it('should log when no project is available', async () => {
    currentProject = null;

    await storageModule.storagePlugin().warmup?.();

    expect(debugMock).toHaveBeenCalledWith('No project found');
  });

  it('should log when the generated storage file does not exist', async () => {
    await storageModule.storagePlugin().warmup?.();

    expect(debugMock).toHaveBeenCalledWith('No storage generated file found');
  });

  it('should import the generated storage loader when present', async () => {
    const generatedPath = join(projectRoot, '.gen/src/.storage.gen.ts');
    await mkdir(join(projectRoot, '.gen/src'), { recursive: true });
    await Bun.write(
      generatedPath,
      'globalThis.__storagePluginLoaded = ((globalThis.__storagePluginLoaded as number | undefined) ?? 0) + 1;\nexport {};\n',
    );

    await storageModule.storagePlugin().warmup?.();

    expect((globalThis as Record<string, unknown>).__storagePluginLoaded).toBe(1);
    expect(debugMock).toHaveBeenCalledWith('Storage buckets registered');
  });

  it('should log and rethrow initialization errors', async () => {
    const generatedPath = join(projectRoot, '.gen/src/.storage.gen.ts');
    await mkdir(join(projectRoot, '.gen/src'), { recursive: true });
    await Bun.write(generatedPath, 'throw new Error("storage boom");\nexport {};\n');

    await expect(storageModule.storagePlugin().warmup?.()).rejects.toThrow('storage boom');
    expect(errorMock).toHaveBeenCalledWith('Storage initialization failed:', expect.any(Error));
  });

  it('should delegate stop to storage cleanup', async () => {
    Bucket('avatars');
    await storage('avatars');

    await storageModule.storagePlugin().stop?.();

    expect(getStorageClient('avatars')).toBeUndefined();
    expect(setGaugeMock).toHaveBeenCalledWith('storage.client.count', 0);
  });
});

describe('storageServer', () => {
  it('should register PUT and GET routes and log readiness', async () => {
    Bucket('private-files');
    Bucket('public-files', { public: true });

    const { app, httpPlugin, routes, plugin } = await mountStorageServer({
      dataDir: join(projectRoot, 'storage-server'),
      tokenSecret: 'server-secret',
    });

    expect(app.ensurePlugin).toHaveBeenCalledWith(FakeHttpPlugin);
    expect(httpPlugin.route).toHaveBeenCalledTimes(2);
    expect(routes.has('PUT /_storage/*')).toBe(true);
    expect(routes.has('GET /_storage/*')).toBe(true);
    expect(infoMock).toHaveBeenCalledWith('Storage server ready — serving 2 bucket(s) on /_storage/');

    // The public bucket's bucket-wide unauthenticated-read surface is warned at startup.
    expect(warnMock).toHaveBeenCalledTimes(1);
    const warnArg = warnMock.mock.calls[0]?.[0] as string;
    expect(warnArg).toContain('public-files');
    expect(warnArg).toContain('unauthenticated');
    expect(warnArg).not.toContain('private-files');

    await expect(plugin.stop?.()).resolves.toBeUndefined();
  });

  // The positive half of the public-bucket warning lives here rather than
  // riding along inside the route-registration test: a bucket that serves every
  // object it holds to anonymous readers must be named at startup, and that
  // assertion should not be silently droppable by a refactor of an unrelated
  // test.
  specTest(
    'names each public bucket in exactly one startup warning',
    {
      feature: 'typescript/object-storage',
      requirement: 'public-bucket-warning',
      check: 'each-public-bucket-is-named-in-one-startup-warning',
    },
    async () => {
      Bucket('private-files');
      Bucket('public-files', { public: true });
      Bucket('public-assets', { public: true });

      await mountStorageServer({
        dataDir: join(projectRoot, 'storage-server'),
        tokenSecret: 'server-secret',
      });

      expect(warnMock).toHaveBeenCalledTimes(1);
      const warnArg = warnMock.mock.calls[0]?.[0] as string;
      expect(warnArg).toContain('public-files');
      expect(warnArg).toContain('public-assets');
      expect(warnArg).toContain('unauthenticated');
      expect(warnArg).not.toContain('private-files');
    },
  );

  specTest(
    'should not warn at startup when no buckets are public',
    {
      feature: 'typescript/object-storage',
      requirement: 'public-bucket-warning',
      check: 'no-warning-is-logged-when-no-bucket-is-public',
    },
    async () => {
      Bucket('private-a');
      Bucket('private-b');

      await mountStorageServer({ dataDir: join(projectRoot, 'storage-server'), tokenSecret: 'server-secret' });

      expect(infoMock).toHaveBeenCalledWith('Storage server ready — serving 2 bucket(s) on /_storage/');
      expect(warnMock).not.toHaveBeenCalled();
    },
  );

  it('should reject invalid upload keys and invalid tokens', async () => {
    Bucket('private-files');
    const dataDir = join(projectRoot, 'storage-server');
    const { routes } = await mountStorageServer({ dataDir, tokenSecret: 'server-secret' });
    const putHandler = routes.get('PUT /_storage/*')!;

    const invalidKey = (await putHandler({
      path: () => '_storage/private-files/../escape.txt',
      queryParams: () => ({ token: 'bad-token' }),
      body: async () => new TextEncoder().encode('x').buffer,
      headers: new Headers({ 'content-type': 'text/plain' }),
    })) as InstanceType<typeof storageModule.HttpResponse>;
    expect(invalidKey.status).toBe(400);

    const invalidToken = (await putHandler({
      path: () => '_storage/private-files/file.txt',
      queryParams: () => ({ token: 'bad-token' }),
      body: async () => new TextEncoder().encode('x').buffer,
      headers: new Headers({ 'content-type': 'text/plain' }),
    })) as InstanceType<typeof storageModule.HttpResponse>;
    expect(invalidToken.status).toBe(403);
  });

  it('should reject uploads for unknown buckets and invalid payloads', async () => {
    Bucket('limited-files', { maxFileSize: '1b', allowedMimeTypes: ['image/png'] });
    const dataDir = join(projectRoot, 'storage-server');
    const signer = new FileBackend(dataDir, 'server-secret');
    const { routes } = await mountStorageServer({ dataDir, tokenSecret: 'server-secret' });
    const putHandler = routes.get('PUT /_storage/*')!;

    const unknownToken =
      (await signer.signedUploadUrl('missing-bucket', 'file.txt')).url.match(/token=([^&]+)/)?.[1] ?? '';
    const missingBucket = (await putHandler({
      path: () => '_storage/missing-bucket/file.txt',
      queryParams: () => ({ token: unknownToken }),
      body: async () => new TextEncoder().encode('x').buffer,
      headers: new Headers({ 'content-type': 'text/plain' }),
    })) as InstanceType<typeof storageModule.HttpResponse>;
    expect(missingBucket.status).toBe(404);

    const limitedToken =
      (await signer.signedUploadUrl('limited-files', 'file.txt')).url.match(/token=([^&]+)/)?.[1] ?? '';
    const rejected = (await putHandler({
      path: () => '_storage/limited-files/file.txt',
      queryParams: () => ({ token: limitedToken }),
      body: async () => new TextEncoder().encode('too big').buffer,
      headers: new Headers({ 'content-type': 'text/plain' }),
    })) as InstanceType<typeof storageModule.HttpResponse>;
    expect(rejected.status).toBe(413);
    expect(await rejected.get().json()).toEqual({
      errors: ['File size 7 bytes exceeds maximum 1b', 'MIME type "text/plain" is not allowed. Allowed: image/png'],
    });

    const missingBody = (await putHandler({
      path: () => '_storage/limited-files/file.txt',
      queryParams: () => ({ token: limitedToken }),
      body: async () => undefined,
      headers: new Headers({ 'content-type': 'image/png' }),
    })) as InstanceType<typeof storageModule.HttpResponse>;
    expect(missingBody.status).toBe(400);
  });

  it('should accept valid uploads and serve public downloads without a token', async () => {
    Bucket('public-files', { public: true });
    const dataDir = join(projectRoot, 'storage-server');
    const signer = new FileBackend(dataDir, 'server-secret');
    const { routes } = await mountStorageServer({ dataDir, tokenSecret: 'server-secret' });
    const putHandler = routes.get('PUT /_storage/*')!;
    const getHandler = routes.get('GET /_storage/*')!;

    const uploadToken =
      (await signer.signedUploadUrl('public-files', 'hello.txt')).url.match(/token=([^&]+)/)?.[1] ?? '';
    const uploaded = (await putHandler({
      path: () => '_storage/public-files/hello.txt',
      queryParams: () => ({ token: uploadToken }),
      body: async () => new TextEncoder().encode('hello').buffer,
      headers: new Headers({ 'content-type': 'text/plain' }),
    })) as { key: string; size: number };

    expect(uploaded).toEqual(expect.objectContaining({ key: 'hello.txt', size: 5 }));

    const response = (await getHandler({
      path: () => '_storage/public-files/hello.txt',
      queryParams: () => ({}),
      headers: new Headers(),
    })) as InstanceType<typeof storageModule.HttpResponse>;
    const http = response.get();

    expect(http.status).toBe(200);
    expect(http.headers.get('content-type')).toBe('text/plain');
    expect(http.headers.get('content-length')).toBe('5');
    expect(await http.text()).toBe('hello');
  });

  it('should enforce tokens for private downloads and return 404 for missing objects', async () => {
    Bucket('private-files');
    const dataDir = join(projectRoot, 'storage-server');
    const signer = new FileBackend(dataDir, 'server-secret');
    await signer.put('private-files', 'secret.txt', new Blob(['top secret']), { contentType: 'text/plain' });
    const token = (await signer.signedDownloadUrl('private-files', 'secret.txt')).url.match(/token=([^&]+)/)?.[1] ?? '';
    const { routes } = await mountStorageServer({ dataDir, tokenSecret: 'server-secret' });
    const getHandler = routes.get('GET /_storage/*')!;

    const forbidden = (await getHandler({
      path: () => '_storage/private-files/secret.txt',
      queryParams: () => ({}),
      headers: new Headers(),
    })) as InstanceType<typeof storageModule.HttpResponse>;
    expect(forbidden.status).toBe(403);

    const ok = (await getHandler({
      path: () => '_storage/private-files/secret.txt',
      queryParams: () => ({ token }),
      headers: new Headers(),
    })) as InstanceType<typeof storageModule.HttpResponse>;
    expect(await ok.get().text()).toBe('top secret');

    const missingToken =
      (await signer.signedDownloadUrl('private-files', 'missing.txt')).url.match(/token=([^&]+)/)?.[1] ?? '';
    const missing = (await getHandler({
      path: () => '_storage/private-files/missing.txt',
      queryParams: () => ({ token: missingToken }),
      headers: new Headers(),
    })) as InstanceType<typeof storageModule.HttpResponse>;
    expect(missing.status).toBe(404);
  });

  it('should reject a download token on the PUT route and an upload token on the GET route', async () => {
    // Signed-URL tokens are method-bound: a token minted for GET (download)
    // must never authorize a PUT (upload) — otherwise anyone holding a
    // read-only download link could overwrite the object — and vice-versa.
    Bucket('private-files');
    const dataDir = join(projectRoot, 'storage-server');
    const signer = new FileBackend(dataDir, 'server-secret');
    await signer.put('private-files', 'secret.txt', new Blob(['top secret']), { contentType: 'text/plain' });
    const { routes } = await mountStorageServer({ dataDir, tokenSecret: 'server-secret' });
    const putHandler = routes.get('PUT /_storage/*')!;
    const getHandler = routes.get('GET /_storage/*')!;

    // A download (GET) token must not authorize an upload.
    const downloadToken =
      (await signer.signedDownloadUrl('private-files', 'secret.txt')).url.match(/token=([^&]+)/)?.[1] ?? '';
    const overwriteAttempt = (await putHandler({
      path: () => '_storage/private-files/secret.txt',
      queryParams: () => ({ token: downloadToken }),
      body: async () => new TextEncoder().encode('overwritten').buffer,
      headers: new Headers({ 'content-type': 'text/plain' }),
    })) as InstanceType<typeof storageModule.HttpResponse>;
    expect(overwriteAttempt.status).toBe(403);

    // The object is untouched.
    const readBack = (await getHandler({
      path: () => '_storage/private-files/secret.txt',
      queryParams: () => ({ token: downloadToken }),
      headers: new Headers(),
    })) as InstanceType<typeof storageModule.HttpResponse>;
    expect(await readBack.get().text()).toBe('top secret');

    // An upload (PUT) token must not authorize a download.
    const uploadToken =
      (await signer.signedUploadUrl('private-files', 'secret.txt')).url.match(/token=([^&]+)/)?.[1] ?? '';
    const downloadAttempt = (await getHandler({
      path: () => '_storage/private-files/secret.txt',
      queryParams: () => ({ token: uploadToken }),
      headers: new Headers(),
    })) as InstanceType<typeof storageModule.HttpResponse>;
    expect(downloadAttempt.status).toBe(403);
  });

  it('round-trips a download for a key with URL-special characters via the encoded signed URL', async () => {
    Bucket('private-files');
    const dataDir = join(projectRoot, 'storage-server');
    const signer = new FileBackend(dataDir, 'server-secret');
    // validateKey permits spaces, '?', '#' and '%'; such a key must still produce
    // a usable signed URL. The backend percent-encodes each path segment and the
    // server decodes them back to the raw key the token was signed over.
    const specialKey = 'logs/2026 q1?draft.txt';
    await signer.put('private-files', specialKey, new Blob(['top secret']), { contentType: 'text/plain' });

    const signed = await signer.signedDownloadUrl('private-files', specialKey);
    const [rawPath, query] = signed.url.split('?');
    const path = rawPath.replace(/^\//, '');
    const token = new URLSearchParams(query).get('token') ?? '';

    // The key segment is percent-encoded — no raw space or '?' leaks into the URL.
    expect(path).toContain('logs/2026%20q1%3Fdraft.txt');

    const { routes } = await mountStorageServer({ dataDir, tokenSecret: 'server-secret' });
    const getHandler = routes.get('GET /_storage/*')!;

    const ok = (await getHandler({
      path: () => path,
      queryParams: () => ({ token }),
      headers: new Headers(),
    })) as InstanceType<typeof storageModule.HttpResponse>;

    const http = ok.get();
    expect(http.status).toBe(200);
    expect(await http.text()).toBe('top secret');
  });

  it('should re-throw non-PATH_TRAVERSAL errors from validateKey on PUT', async () => {
    Bucket('private-files');
    const dataDir = join(projectRoot, 'storage-server');
    const { routes } = await mountStorageServer({ dataDir, tokenSecret: 'server-secret' });
    const putHandler = routes.get('PUT /_storage/*')!;

    // A null byte in the key triggers INVALID_KEY (not PATH_TRAVERSAL), which should be re-thrown
    await expect(
      putHandler({
        path: () => '_storage/private-files/bad\0key',
        queryParams: () => ({ token: 'any' }),
        body: async () => new TextEncoder().encode('x').buffer,
        headers: new Headers({ 'content-type': 'text/plain' }),
      }),
    ).rejects.toThrow('Invalid storage key');
  });

  it('should re-throw non-PATH_TRAVERSAL errors from validateKey on GET', async () => {
    Bucket('private-files');
    const dataDir = join(projectRoot, 'storage-server');
    const { routes } = await mountStorageServer({ dataDir, tokenSecret: 'server-secret' });
    const getHandler = routes.get('GET /_storage/*')!;

    // A null byte in the key triggers INVALID_KEY (not PATH_TRAVERSAL), which should be re-thrown
    await expect(
      getHandler({
        path: () => '_storage/private-files/bad\0key',
        queryParams: () => ({}),
        headers: new Headers(),
      }),
    ).rejects.toThrow('Invalid storage key');
  });

  it('should return 400 for path traversal keys on GET', async () => {
    Bucket('private-files');
    const dataDir = join(projectRoot, 'storage-server');
    const { routes } = await mountStorageServer({ dataDir, tokenSecret: 'server-secret' });
    const getHandler = routes.get('GET /_storage/*')!;

    const result = (await getHandler({
      path: () => '_storage/private-files/../escape.txt',
      queryParams: () => ({}),
      headers: new Headers(),
    })) as InstanceType<typeof storageModule.HttpResponse>;
    expect(result.status).toBe(400);
  });

  it('should prefer explicit server options over config values', async () => {
    Bucket('private-files');
    useConfigMock.mockImplementation(() =>
      defaultConfig({
        dataDir: join(projectRoot, 'config-storage'),
        tokenSecret: 'config-secret',
      }),
    );

    const overriddenDir = join(projectRoot, 'override-storage');
    const signer = new FileBackend(overriddenDir, 'override-secret');
    await signer.put('private-files', 'configured.txt', new Blob(['from override']), { contentType: 'text/plain' });
    const token =
      (await signer.signedDownloadUrl('private-files', 'configured.txt')).url.match(/token=([^&]+)/)?.[1] ?? '';
    const { routes } = await mountStorageServer({ dataDir: overriddenDir, tokenSecret: 'override-secret' });
    const getHandler = routes.get('GET /_storage/*')!;

    const response = (await getHandler({
      path: () => '_storage/private-files/configured.txt',
      queryParams: () => ({ token }),
      headers: new Headers(),
    })) as InstanceType<typeof storageModule.HttpResponse>;

    expect(await response.get().text()).toBe('from override');
  });
});
