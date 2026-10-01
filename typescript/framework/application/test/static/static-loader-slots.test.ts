import { afterAll, describe, expect } from 'bun:test';
import {
  cpSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  realpathSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import { join, relative } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import type { GenerateResult } from '../../src/application';

/**
 * A workload with several StaticPlugins: one that loads nothing, the default
 * public folder, and a second folder mounted under `/docs`.
 *
 * The packaged serve entrypoint registers every generated loader under its
 * export key and each plugin looks its own up. A shared `static-loader` key
 * made the second plugin's loader replace the first, and both plugins wrote the
 * same `.static.gen.ts` and staged into the same `.gen/public/`, so the two
 * `index.html` files overwrote each other.
 */

const PACKAGE_ROOT = join(import.meta.dir, '..', '..');
const PACKAGED_TIMEOUT_MS = 60_000;
const REPORT_MARKER = 'static-loader-slots-report:';
const originalProjectRoot = process.env['PUTNAMI_PROJECT_ROOT'];
const temporaryRoots: string[] = [];

const MAIN = `import { resolve } from 'node:path';
import { application, http, StaticPlugin, staticFiles } from '@putnami/application';

export const single = () => application().use(http({ port: 0 })).use(staticFiles());

export const app = () =>
  application()
    .use(http({ port: 0 }))
    // Loads no generated module, so it takes no slot.
    .use(new StaticPlugin({ skipLoading: true }))
    // The default public folder at the project root.
    .use(staticFiles())
    // A second folder, mounted under /docs.
    .use(staticFiles({ scanPath: resolve(import.meta.dir, '..', 'docs'), prefix: '/docs' }));
`;

const PATHS = ['/index.html', '/app.css', '/guide.html', '/docs/index.html', '/docs/guide.html', '/docs/app.css'];

interface BuiltWorkload {
  projectRoot: string;
  mainPath: string;
  singleLoaders: Record<string, string>;
  singleLoaderBytes: string;
  loaders: Record<string, string>;
}

interface PackagedReport {
  status: Record<string, number>;
  bodies: Record<string, string>;
}

afterAll(() => {
  if (originalProjectRoot === undefined) delete process.env.PUTNAMI_PROJECT_ROOT;
  else process.env['PUTNAMI_PROJECT_ROOT'] = originalProjectRoot;
  for (const root of temporaryRoots.splice(0)) rmSync(root, { recursive: true, force: true });
});

function temporaryRoot(prefix: string): string {
  const root = realpathSync(mkdtempSync(join(tmpdir(), prefix)));
  temporaryRoots.push(root);
  return root;
}

function loaderExports(result: GenerateResult): Record<string, string> {
  return Object.fromEntries(
    Object.entries(result.exports ?? {})
      .filter(([key]) => key.endsWith('-loader') && !key.endsWith('client-loader'))
      .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0)),
  );
}

let built: Promise<BuiltWorkload> | undefined;

/** Lay the workload out outside the framework tree and build it: first alone, then with its siblings. */
function builtWorkload(): Promise<BuiltWorkload> {
  built ??= (async () => {
    const projectRoot = temporaryRoot('putnami-static-loader-slots-');
    const mainPath = join(projectRoot, 'src', 'main.ts');
    mkdirSync(join(projectRoot, 'node_modules', '@putnami'), { recursive: true });
    symlinkSync(PACKAGE_ROOT, join(projectRoot, 'node_modules', '@putnami', 'application'), 'dir');
    mkdirSync(join(projectRoot, 'src'), { recursive: true });
    mkdirSync(join(projectRoot, 'public'), { recursive: true });
    mkdirSync(join(projectRoot, 'docs'), { recursive: true });
    writeFileSync(mainPath, MAIN);
    writeFileSync(join(projectRoot, 'public', 'index.html'), 'public home');
    writeFileSync(join(projectRoot, 'public', 'app.css'), 'body{}');
    writeFileSync(join(projectRoot, 'docs', 'index.html'), 'docs home');
    writeFileSync(join(projectRoot, 'docs', 'guide.html'), 'docs guide');

    process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;
    type Buildable = () => { build(options?: object): Promise<GenerateResult> };
    const { app, single } = (await import(mainPath)) as { app: Buildable; single: Buildable };
    const options = { publishCapabilityManifest: false, publishDesignGraph: false };
    const singleLoaders = loaderExports(await single().build(options));
    const singleLoaderBytes = readFileSync(join(projectRoot, '.gen', 'src', 'static', '.static.gen.ts'), 'utf8');
    const loaders = loaderExports(await app().build(options));
    return { projectRoot, mainPath, singleLoaders, singleLoaderBytes, loaders };
  })();
  return built;
}

/**
 * The packaged serve entrypoint of the TypeScript extension (GenerateBundledServe):
 * the app first, then one static import and one registration per loader key. It
 * requests every path once and reports status and body.
 */
function packagedEntrypoint(entryDir: string, mainPath: string, loaders: Record<string, string>): string {
  const importPath = (path: string) => {
    const withoutExtension = relative(entryDir, path).replace(/\.ts$/, '');
    return withoutExtension.startsWith('.') ? withoutExtension : `./${withoutExtension}`;
  };
  const entries = Object.entries(loaders);
  return [
    `import { HttpPlugin, registerModuleLoader } from '@putnami/application';`,
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
    '  const bodies: Record<string, string> = {};',
    `  for (const path of ${JSON.stringify(PATHS)}) {`,
    '    const response = await fetch(`http://127.0.0.1:${port}${path}`);',
    '    status[path] = response.status;',
    '    bodies[path] = response.status === 200 ? await response.text() : "";',
    '  }',
    `  console.log(${JSON.stringify(REPORT_MARKER)} + JSON.stringify({ status, bodies }));`,
    '} finally {',
    '  await workload.stop();',
    '}',
    '',
  ].join('\n');
}

let packaged: Promise<PackagedReport> | undefined;

/**
 * Bundle the entrypoint into a root that carries no src/, no public/ and no
 * generated loader, only the staged `.gen/public` the Docker package copies,
 * and run it once.
 */
function packagedReport(): Promise<PackagedReport> {
  packaged ??= (async () => {
    const { projectRoot, mainPath, loaders } = await builtWorkload();
    const entryDir = join(projectRoot, '.gen', 'src');
    const entry = join(entryDir, 'serve.bundled.ts');
    writeFileSync(entry, packagedEntrypoint(entryDir, mainPath, loaders));

    const packagedRoot = temporaryRoot('putnami-static-loader-slots-packaged-');
    const build = await Bun.build({ entrypoints: [entry], outdir: packagedRoot, target: 'bun', naming: 'server.js' });
    if (!build.success) throw new Error(build.logs.map((log) => log.message).join('\n'));
    const output = build.outputs.find((artifact) => artifact.kind === 'entry-point');
    if (!output) throw new Error('packaged entrypoint output was not emitted');
    cpSync(join(projectRoot, '.gen', 'public'), join(packagedRoot, '.gen', 'public'), { recursive: true });

    const env: Record<string, string | undefined> = {
      ...process.env,
      PWD: packagedRoot,
      PUTNAMI_PROJECT_ROOT: packagedRoot,
      NODE_ENV: 'test',
    };
    env['PUTNAMI_ASSETS_DIR'] = undefined;
    const child = Bun.spawn([process.execPath, output.path], {
      cwd: packagedRoot,
      env,
      stdout: 'pipe',
      stderr: 'pipe',
    });
    const [stdout, stderr, exitCode] = await Promise.all([
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
      child.exited,
    ]);
    if (exitCode !== 0) throw new Error(`packaged workload exited ${exitCode}:\n${stderr}`);
    const line = stdout.split('\n').find((candidate) => candidate.startsWith(REPORT_MARKER));
    if (!line) throw new Error(`packaged workload printed no report:\n${stdout}\n${stderr}`);
    return JSON.parse(line.slice(REPORT_MARKER.length)) as PackagedReport;
  })();
  return packaged;
}

describe('static() route loaders in a packaged workload', () => {
  specTest(
    'every loading StaticPlugin exports its own route loader and staging folder',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'route-loader-activation',
      check: 'every-loading-static-plugin-exports-its-own-route-loader',
    },
    async () => {
      const { projectRoot, loaders } = await builtWorkload();
      // The skipLoading plugin takes no slot; the others follow registration order.
      expect(loaders).toEqual({
        'static-1-loader': join(projectRoot, '.gen', 'src', 'static', '.static-1.gen.ts'),
        'static-loader': join(projectRoot, '.gen', 'src', 'static', '.static.gen.ts'),
      });
      // Both folders hold an index.html; neither overwrites the other.
      expect(readFileSync(join(projectRoot, '.gen', 'public', 'index.html'), 'utf8')).toBe('public home');
      expect(readFileSync(join(projectRoot, '.gen', 'public', '.static-1', 'index.html'), 'utf8')).toBe('docs home');
      const second = readFileSync(join(projectRoot, '.gen', 'src', 'static', '.static-1.gen.ts'), 'utf8');
      expect(second).toContain("staticPlugin.routeStatic('guide.html', '.static-1/guide.html'");
      expect(second).not.toContain("'app.css'");
    },
    PACKAGED_TIMEOUT_MS,
  );

  specTest(
    'a second StaticPlugin leaves the first plugin loader byte-identical',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'route-loader-activation',
      check: 'a-single-static-plugin-keeps-its-historical-loader',
    },
    async () => {
      const { projectRoot, singleLoaders, singleLoaderBytes } = await builtWorkload();
      expect(singleLoaders).toEqual({
        'static-loader': join(projectRoot, '.gen', 'src', 'static', '.static.gen.ts'),
      });
      // The slot-0 scan never matches the hidden `.static-1/` folder.
      expect(readFileSync(join(projectRoot, '.gen', 'src', 'static', '.static.gen.ts'), 'utf8')).toBe(
        singleLoaderBytes,
      );
    },
    PACKAGED_TIMEOUT_MS,
  );

  specTest(
    'each StaticPlugin serves only its own files from the packaged entrypoint',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'route-loader-activation',
      check: 'each-static-plugin-serves-its-own-files-in-a-package',
    },
    async () => {
      const report = await packagedReport();

      expect(report.status).toEqual({
        '/index.html': 200,
        '/app.css': 200,
        '/guide.html': 404,
        '/docs/index.html': 200,
        '/docs/guide.html': 200,
        '/docs/app.css': 404,
      });
      expect(report.bodies['/index.html']).toBe('public home');
      expect(report.bodies['/docs/index.html']).toBe('docs home');
      expect(report.bodies['/docs/guide.html']).toBe('docs guide');
    },
    PACKAGED_TIMEOUT_MS,
  );
});
