import { describe, expect, it } from 'bun:test';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
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
  async function generateLoader(files: string[]): Promise<string> {
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
      await new StaticPlugin({ scanPath: sourcePath, compress: 1_000_000 }).generate();
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
});
