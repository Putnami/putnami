import { afterEach, describe, expect, it } from 'bun:test';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { generateApiLoader } from '../../src/api/api-codegen';
import { asRoute } from '../../src/api/api.utils';

const originalProjectRoot = process.env['PUTNAMI_PROJECT_ROOT'];
const tempDirs: string[] = [];

afterEach(() => {
  if (originalProjectRoot === undefined) {
    delete process.env.PUTNAMI_PROJECT_ROOT;
  } else {
    process.env['PUTNAMI_PROJECT_ROOT'] = originalProjectRoot;
  }
  while (tempDirs.length > 0) {
    rmSync(tempDirs.pop()!, { recursive: true, force: true });
  }
});

describe('generateApiLoader', () => {
  it('resolves configured relative scan paths from the project root', async () => {
    const projectRoot = mkdtempSync(join(tmpdir(), 'putnami-api-relative-scan-'));
    tempDirs.push(projectRoot);
    process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;

    const scanPath = join(projectRoot, 'src', 'api');
    mkdirSync(scanPath, { recursive: true });
    writeFileSync(join(scanPath, 'post.ts'), 'export const POST = {};\n');

    const { apiLoaderPath } = await generateApiLoader('src/api', undefined);

    expect(apiLoaderPath).toBe(join(projectRoot, '.gen', 'src', 'api', '.api-application.gen.ts'));
    expect(readFileSync(apiLoaderPath, 'utf8')).toContain("exploring 'src/api'");
  });

  it('preserves csrf opt-in for generated file-based routes', async () => {
    const projectRoot = mkdtempSync(join(tmpdir(), 'putnami-api-codegen-'));
    tempDirs.push(projectRoot);
    process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;

    const scanPath = join(projectRoot, 'src', 'api');
    mkdirSync(scanPath, { recursive: true });
    writeFileSync(join(scanPath, 'post.ts'), 'export const POST = {};\n');

    const { apiLoaderPath } = await generateApiLoader(scanPath, '/api', true);
    const source = readFileSync(apiLoaderPath, 'utf8');

    expect(source).toContain('new ApiPlugin({"autoScan":false,"prefix":"/api","csrf":true})');
  });

  it('emits typed parameterized routes from the generated loader registry', async () => {
    const projectRoot = mkdtempSync(join(tmpdir(), 'putnami-api-routes-'));
    tempDirs.push(projectRoot);
    process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;

    const scanPath = join(projectRoot, 'src', 'api', 'users', '[id]');
    mkdirSync(scanPath, { recursive: true });
    const routeModule = join(import.meta.dir, '../../src/api/route/index.ts');
    writeFileSync(
      join(scanPath, 'get.ts'),
      `import { endpoint } from ${JSON.stringify(routeModule)};\nexport default endpoint().handle(() => ({ ok: true }));\n`,
    );

    const { result } = await generateApiLoader(join(projectRoot, 'src', 'api'), '/api');
    expect(result.httpRoutes).toEqual([
      expect.objectContaining({
        match: 'template',
        path: '/api/users/{id}',
        methods: ['GET', 'HEAD'],
        provenance: expect.objectContaining({
          sourceKind: 'typed-api',
          evidencePath: 'src/api/users/[id]/get.ts',
        }),
      }),
    ]);
  });

  // A scanned stream is the third producer of one route's inventory entry, beside
  // the registered path in ApiPlugin.generate and the Go emitter. All three must
  // report the same methods: a gateway that default-denies on the inventory
  // refuses a method the server answers whenever one of them under-reports.
  const scanStreamRoutes = async (implicitHead: boolean) => {
    const projectRoot = mkdtempSync(join(tmpdir(), 'putnami-api-stream-routes-'));
    tempDirs.push(projectRoot);
    process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;

    const scanPath = join(projectRoot, 'src', 'api');
    const routeModule = join(import.meta.dir, '../../src/api/route/index.ts');
    const streamModule =
      `import { endpoint, Stream } from ${JSON.stringify(routeModule)};\n` +
      `export default endpoint().returns(Stream({ event: String })).handle(async () => {});\n`;
    mkdirSync(join(scanPath, 'chat'), { recursive: true });
    writeFileSync(join(scanPath, 'chat', 'ws.ts'), streamModule);
    mkdirSync(join(scanPath, 'notifications'), { recursive: true });
    writeFileSync(join(scanPath, 'notifications', 'get.ts'), streamModule);

    const { result } = await generateApiLoader(scanPath, undefined, false, false, implicitHead);
    return [...(result.httpRoutes ?? [])]
      .sort((a, b) => a.path.localeCompare(b.path))
      .map((route) => ({ path: route.path, methods: route.methods }));
  };

  it('reports the HEAD companion for a scanned stream route', async () => {
    expect(await scanStreamRoutes(true)).toEqual([
      { path: '/chat', methods: ['GET', 'HEAD'] },
      { path: '/notifications', methods: ['GET', 'HEAD'] },
    ]);
  });

  it('omits the HEAD companion for a scanned stream route when the server serves no implicit HEAD', async () => {
    expect(await scanStreamRoutes(false)).toEqual([
      { path: '/chat', methods: ['GET'] },
      { path: '/notifications', methods: ['GET'] },
    ]);
  });

  it('fails generation when a route module cannot be inspected', async () => {
    const projectRoot = mkdtempSync(join(tmpdir(), 'putnami-api-import-failure-'));
    tempDirs.push(projectRoot);
    process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;

    const scanPath = join(projectRoot, 'src', 'api');
    mkdirSync(scanPath, { recursive: true });
    writeFileSync(join(scanPath, 'get.ts'), `throw new Error('runtime config missing');\n`);

    // The message names the module with the host separator.
    await expect(generateApiLoader(scanPath, undefined)).rejects.toThrow(
      `inspect typed API route module ${join('src', 'api', 'get.ts')}: runtime config missing`,
    );
  });
});

describe('asRoute', () => {
  it('derives the same route key from a Windows-separated scan path', () => {
    // The route scan yields native separators on Windows; the key is a URL path.
    expect(asRoute('users\\[id]\\get.ts')).toBe('users/[id]');
    expect(asRoute('users/[id]/get.ts')).toBe('users/[id]');
    expect(asRoute('get.ts')).toBe('/');
  });
});
