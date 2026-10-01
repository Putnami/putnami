import { afterAll, describe, expect } from 'bun:test';
import { mkdirSync, mkdtempSync, realpathSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, relative } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import type { GenerateResult } from '../../src/application';

/**
 * A workload with several api() plugins: one that only registers routes, one
 * that names a second scan root, and the default one next to its entry point.
 *
 * The packaged serve entrypoint registers every generated loader under its
 * export key and each plugin looks its own up. Inside the package there is no
 * route folder and `import.meta.dir` names the bundle, so the key cannot come
 * from a scan path. A shared key made the second plugin overwrite the first and
 * every plugin — the registered-only one included — resolve the survivor,
 * dropping a whole route surface from the packaged workload while
 * `putnami serve` still showed it.
 */

const PACKAGE_ROOT = join(import.meta.dir, '..', '..');
const PACKAGED_TIMEOUT_MS = 60_000;
const REPORT_MARKER = 'api-loader-slots-report:';
const originalProjectRoot = process.env['PUTNAMI_PROJECT_ROOT'];
const temporaryRoots: string[] = [];

const MAIN = `import { resolve } from 'node:path';
import { api, application, endpoint, http } from '@putnami/application';

export const app = () =>
  application()
    .use(http({ port: 0 }))
    // Registered, not scanned: this plugin owns no generated loader.
    .use(api({ autoScan: false }).register('/jwks', endpoint(() => ({ keys: [] })), 'GET'))
    // A second scan root, named the way a workload names one.
    .use(api({ scanPath: resolve(import.meta.dir, 'internal-api') }))
    // The default scan root, found next to this file.
    .use(api());
`;

interface BuiltWorkload {
  projectRoot: string;
  mainPath: string;
  loaders: Record<string, string>;
}

interface PackagedReport {
  packagedRoot: string;
  status: Record<string, number>;
  plugins: Array<{ scanPath: string | null; routes: string[] }>;
}

afterAll(() => {
  if (originalProjectRoot === undefined) delete process.env.PUTNAMI_PROJECT_ROOT;
  else process.env['PUTNAMI_PROJECT_ROOT'] = originalProjectRoot;
  for (const root of temporaryRoots.splice(0)) rmSync(root, { recursive: true, force: true });
});

function routeModule(scope: string): string {
  return `import { endpoint } from '@putnami/application';\nexport default endpoint(() => ({ scope: ${JSON.stringify(scope)} }));\n`;
}

function temporaryRoot(prefix: string): string {
  // Real path: the entry point's import.meta.dir is resolved, and a project
  // root spelled through a symlink would relativize scan paths out of it.
  const root = realpathSync(mkdtempSync(join(tmpdir(), prefix)));
  temporaryRoots.push(root);
  return root;
}

/** The loader exports of a build, the way the packaged entrypoint consumes them. */
function loaderExports(result: GenerateResult): Record<string, string> {
  return Object.fromEntries(
    Object.entries(result.exports ?? {})
      .filter(([key]) => key.endsWith('-loader') && !key.endsWith('client-loader'))
      .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0)),
  );
}

let built: Promise<BuiltWorkload> | undefined;

/**
 * Lay the workload out outside the framework tree — a caller under
 * `typescript/framework/` counts as framework code, so the default api() would
 * never find its folder — and build it once.
 */
function builtWorkload(): Promise<BuiltWorkload> {
  built ??= (async () => {
    const projectRoot = temporaryRoot('putnami-api-loader-slots-');
    const mainPath = join(projectRoot, 'src', 'main.ts');
    mkdirSync(join(projectRoot, 'node_modules', '@putnami'), { recursive: true });
    symlinkSync(PACKAGE_ROOT, join(projectRoot, 'node_modules', '@putnami', 'application'), 'dir');
    mkdirSync(join(projectRoot, 'src', 'internal-api', 'revocations'), { recursive: true });
    mkdirSync(join(projectRoot, 'src', 'api', 'tokens'), { recursive: true });
    writeFileSync(mainPath, MAIN);
    writeFileSync(join(projectRoot, 'src', 'internal-api', 'revocations', 'get.ts'), routeModule('internal'));
    writeFileSync(join(projectRoot, 'src', 'api', 'tokens', 'get.ts'), routeModule('public'));

    process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;
    const { app } = (await import(mainPath)) as {
      app: () => { build(options?: object): Promise<GenerateResult> };
    };
    const result = await app().build({ publishCapabilityManifest: false, publishDesignGraph: false });
    return { projectRoot, mainPath, loaders: loaderExports(result) };
  })();
  return built;
}

/**
 * The packaged serve entrypoint of the TypeScript extension (GenerateBundledServe):
 * the app first, then one static import and one registration per loader key.
 * Instead of serving forever it calls every route once and reports what each
 * api() plugin ended up owning.
 */
function packagedEntrypoint(entryDir: string, mainPath: string, loaders: Record<string, string>): string {
  const importPath = (path: string) => {
    const withoutExtension = relative(entryDir, path).replace(/\.ts$/, '');
    return withoutExtension.startsWith('.') ? withoutExtension : `./${withoutExtension}`;
  };
  const entries = Object.entries(loaders);
  return [
    `import { ApiPlugin, HttpPlugin, registerModuleLoader } from '@putnami/application';`,
    `import { app } from ${JSON.stringify(importPath(mainPath))};`,
    ...entries.map(([, path], index) => `import * as loaderModule${index} from ${JSON.stringify(importPath(path))};`),
    ...entries.map(
      ([key], index) => `registerModuleLoader(${JSON.stringify(key)}, () => Promise.resolve(loaderModule${index}));`,
    ),
    'const workload = app();',
    'await workload.start();',
    'try {',
    '  const port = workload.getPlugin(HttpPlugin).getServer()?.port;',
    '  const status: Record<string, number> = {};',
    "  for (const path of ['/jwks', '/revocations', '/tokens']) {",
    "    const response = await fetch(`http://127.0.0.1:${port}${path}`, { headers: { accept: 'application/json' } });",
    '    status[path] = response.status;',
    '  }',
    '  const plugins = workload',
    '    .collectPlugins()',
    '    .flatMap(({ plugin }) => (plugin instanceof ApiPlugin ? [plugin] : []))',
    '    .map((plugin) => ({',
    '      scanPath: plugin.scanPath ?? null,',
    '      routes: plugin.routes.map((route) => `${route.method} ${route.path}`).sort(),',
    '    }));',
    `  console.log(${JSON.stringify(REPORT_MARKER)} + JSON.stringify({ status, plugins }));`,
    '} finally {',
    '  await workload.stop();',
    '}',
    '',
  ].join('\n');
}

let packaged: Promise<PackagedReport> | undefined;

/** Bundle the entrypoint into a root that carries neither src/ nor .gen/, run it once. */
function packagedReport(): Promise<PackagedReport> {
  packaged ??= (async () => {
    const { projectRoot, mainPath, loaders } = await builtWorkload();
    const entryDir = join(projectRoot, '.gen', 'src');
    const entry = join(entryDir, 'serve.bundled.ts');
    writeFileSync(entry, packagedEntrypoint(entryDir, mainPath, loaders));

    const packagedRoot = temporaryRoot('putnami-api-loader-slots-packaged-');
    const build = await Bun.build({ entrypoints: [entry], outdir: packagedRoot, target: 'bun', naming: 'server.js' });
    if (!build.success) throw new Error(build.logs.map((log) => log.message).join('\n'));
    const output = build.outputs.find((artifact) => artifact.kind === 'entry-point');
    if (!output) throw new Error('packaged entrypoint output was not emitted');

    const child = Bun.spawn([process.execPath, output.path], {
      cwd: packagedRoot,
      env: { ...process.env, PWD: packagedRoot, PUTNAMI_PROJECT_ROOT: packagedRoot, NODE_ENV: 'test' },
      stdout: 'pipe',
      stderr: 'pipe',
    });
    const [stdout, stderr, exitCode] = await Promise.all([
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
      child.exited,
    ]);
    if (exitCode !== 0) throw new Error(`packaged workload exited ${exitCode}:\n${stderr}`);
    // The framework logs to stdout too; the report is the one marked line.
    const line = stdout.split('\n').find((candidate) => candidate.startsWith(REPORT_MARKER));
    if (!line) throw new Error(`packaged workload printed no report:\n${stdout}\n${stderr}`);
    const report = JSON.parse(line.slice(REPORT_MARKER.length)) as Omit<PackagedReport, 'packagedRoot'>;
    return { packagedRoot, ...report };
  })();
  return packaged;
}

describe('api() route loaders in a packaged workload', () => {
  specTest(
    'every scanning api() plugin exports its own route loader',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'route-loader-activation',
      check: 'every-scanning-api-plugin-exports-its-own-route-loader',
    },
    async () => {
      const { projectRoot, loaders } = await builtWorkload();
      // Slots follow registration order among the plugins that scan; the
      // registered-only plugin exports nothing and takes no slot.
      expect(loaders).toEqual({
        'api-1-loader': join(projectRoot, '.gen', 'src', 'api', '.api-application.gen.ts'),
        'api-loader': join(projectRoot, '.gen', 'src', 'internal-api', '.api-application.gen.ts'),
      });
    },
    PACKAGED_TIMEOUT_MS,
  );

  specTest(
    'each scanning api() plugin serves its own routes from the packaged entrypoint',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'route-loader-activation',
      check: 'each-api-plugin-resolves-only-its-own-route-loader-in-a-package',
    },
    async () => {
      const report = await packagedReport();

      expect(report.status).toEqual({ '/jwks': 200, '/revocations': 200, '/tokens': 200 });
      // Inside the package the explicit scan root names the bundle's directory
      // and the default api() finds no folder at all: neither could key a
      // loader, which is why the slot does.
      expect(report.plugins.slice(1)).toEqual([
        { scanPath: join(report.packagedRoot, 'internal-api'), routes: ['GET /revocations'] },
        { scanPath: null, routes: ['GET /tokens'] },
      ]);
    },
    PACKAGED_TIMEOUT_MS,
  );

  specTest(
    'an api() plugin that scans nothing adopts no loader in the packaged entrypoint',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'route-loader-activation',
      check: 'a-plugin-that-does-not-scan-resolves-no-route-loader',
    },
    async () => {
      const report = await packagedReport();

      // `api-loader` is registered, but it belongs to the first scanning
      // plugin: the registered-only plugin keeps exactly what it registered.
      expect(report.plugins[0]).toEqual({ scanPath: null, routes: ['GET /jwks'] });
    },
    PACKAGED_TIMEOUT_MS,
  );
});
