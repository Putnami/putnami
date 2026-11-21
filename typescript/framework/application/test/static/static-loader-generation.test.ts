import { describe, expect, it } from 'bun:test';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import ts from 'typescript';
import { StaticPlugin } from '../../src/static/static.plugin';

/**
 * Shape guarantees for the generated `.gen/src/static/.static.gen.ts` loader.
 *
 * Both guarantees are load-bearing for the build, not style:
 *
 * - **One statement per route.** A `new StaticPlugin().routeStatic(...).routeStatic(...)`
 *   chain makes the TypeScript checker recurse once per link, so a site with a few
 *   hundred assets overflows the Node stack during `build~types`.
 * - **Sorted scan order.** `Glob.scanSync` yields directory order. An unstable order
 *   rewrites this file on every generate, which misses the `build~types` cache every
 *   run and makes the emitted bytes unreviewable.
 */
describe('static loader generation', () => {
  async function generateLoader(files: string[], compress = 1_000_000): Promise<string> {
    const originalProjectRoot = process.env['PUTNAMI_PROJECT_ROOT'];
    const projectRoot = mkdtempSync(join(tmpdir(), 'putnami-static-loader-'));
    try {
      process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;
      const sourcePath = join(projectRoot, 'public');
      for (const relative of files) {
        const target = join(sourcePath, relative);
        mkdirSync(join(target, '..'), { recursive: true });
        writeFileSync(target, relative);
      }
      await new StaticPlugin({ scanPath: sourcePath, compress }).generate();
      return readFileSync(join(projectRoot, '.gen', 'src', 'static', '.static.gen.ts'), 'utf8');
    } finally {
      if (originalProjectRoot === undefined) {
        delete process.env.PUTNAMI_PROJECT_ROOT;
      } else {
        process.env['PUTNAMI_PROJECT_ROOT'] = originalProjectRoot;
      }
      rmSync(projectRoot, { recursive: true, force: true });
    }
  }

  // Created in reverse-alphabetical order so a generator that trusted the scan
  // order would emit them out of order.
  const files = ['zeta.txt', 'mid/index.html', 'beta.css', 'alpha/deep.json'];

  it('emits one standalone statement per route, never a call chain', async () => {
    const source = await generateLoader(files);

    const routeLines = source.split('\n').filter((line) => line.includes('routeStatic'));
    expect(routeLines.length).toBeGreaterThan(files.length);
    for (const line of routeLines) {
      expect(line).toStartWith('staticPlugin.routeStatic(');
      expect(line).toEndWith(';');
    }
    expect(source).toContain('const staticPlugin = new StaticPlugin();');
    expect(source).toContain('export default staticPlugin;');
  });

  it('emits routes in sorted scan order', async () => {
    const source = await generateLoader(files);

    const targets = [...source.matchAll(/routeStatic\('[^']*', '([^']*)'/g)].map((match) => match[1] as string);
    expect(targets.length).toBeGreaterThan(0);
    expect([...targets]).toEqual([...targets].sort());
  });

  it('is byte-identical across runs', async () => {
    const first = await generateLoader(files);
    const second = await generateLoader(files);

    expect(second).toBe(first);
  });

  it('emits a boolean gzip flag that type-checks when compression is off', async () => {
    const source = await generateLoader(['fonts/inter.woff2', 'index.html'], 0);

    expect(gzipLiterals(source)).toEqual(['false', 'false', 'false', 'false']);
    expect(typeCheckLoader(source)).toEqual([]);
  }, 30_000);

  it('emits a boolean gzip flag that type-checks when files are compressed', async () => {
    const source = await generateLoader(['fonts/inter.woff2', 'index.html'], 1);

    expect(gzipLiterals(source)).toEqual(['true', 'true', 'true', 'true']);
    expect(typeCheckLoader(source)).toEqual([]);
  }, 30_000);
});

/** The value each generated `routeStatic` call passes as `gzip`, as written. */
function gzipLiterals(source: string): string[] {
  return [...source.matchAll(/\{ gzip: ([^,]*), mime:/g)].map((match) => match[1] as string);
}

/**
 * Type-check a generated loader the way a workload's `build~types` does: against
 * the real `StaticPlugin` declaration, which `@putnami/application` resolves to.
 * The loader is served from memory; only its own diagnostics are returned.
 */
function typeCheckLoader(source: string): string[] {
  // TypeScript hands the host forward-slash paths on every platform: compare normalized forms.
  const loaderPath = join(tmpdir(), 'putnami-static-loader-typecheck', '.static.gen.ts').replaceAll('\\', '/');
  const options: ts.CompilerOptions = {
    noEmit: true,
    strict: true,
    skipLibCheck: true,
    types: [],
    target: ts.ScriptTarget.ES2022,
    module: ts.ModuleKind.ESNext,
    moduleResolution: ts.ModuleResolutionKind.Bundler,
    paths: { '@putnami/application': [join(import.meta.dir, '..', '..', 'src', 'static', 'static.plugin.ts')] },
  };
  const host = ts.createCompilerHost(options);
  const readFile = host.getSourceFile.bind(host);
  const fileExists = host.fileExists.bind(host);
  host.getSourceFile = (fileName, languageVersion, ...rest) =>
    fileName.replaceAll('\\', '/') === loaderPath
      ? ts.createSourceFile(fileName, source, languageVersion)
      : readFile(fileName, languageVersion, ...rest);
  host.fileExists = (fileName) => fileName.replaceAll('\\', '/') === loaderPath || fileExists(fileName);

  const program = ts.createProgram([loaderPath], options, host);
  const loader = program.getSourceFile(loaderPath);
  if (!loader) throw new Error('generated loader was not loaded');
  return [...program.getSyntacticDiagnostics(loader), ...program.getSemanticDiagnostics(loader)].map((diagnostic) =>
    ts.flattenDiagnosticMessageText(diagnostic.messageText, '\n'),
  );
}
