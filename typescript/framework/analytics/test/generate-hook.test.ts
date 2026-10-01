import { afterEach, describe, expect, it } from 'bun:test';
import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { buildTrackerBundle } from '../bin/_build';

const packageRoot = join(import.meta.dir, '..');
const workspaceRoot = join(packageRoot, '..', '..', '..');

const tempRoots: string[] = [];

function createProject(): { projectRoot: string; contextPath: string } {
  const projectRoot = mkdtempSync(join(tmpdir(), 'putnami-analytics-hook-'));
  tempRoots.push(projectRoot);
  mkdirSync(join(projectRoot, '.gen'), { recursive: true });
  writeFileSync(join(projectRoot, 'package.json'), `${JSON.stringify({ name: 'hook-host' })}\n`);
  writeFileSync(
    join(projectRoot, 'putnami.json'),
    `${JSON.stringify({ name: '@example/hook-host', tags: ['ts'] }, null, 2)}\n`,
  );
  const contextPath = join(projectRoot, 'context.json');
  writeFileSync(
    contextPath,
    `${JSON.stringify({
      workspaceRoot,
      projectRoot,
      extensionRoot: packageRoot,
      outputRoot: join(projectRoot, '.gen'),
      cacheRoot: join(projectRoot, '.cache'),
      debug: false,
      hook: 'preBuild',
      extension: '@putnami/analytics',
      projectName: '@example/hook-host',
      mode: 'build',
    })}\n`,
  );
  return { projectRoot, contextPath };
}

afterEach(() => {
  for (const root of tempRoots.splice(0)) {
    rmSync(root, { recursive: true, force: true });
  }
});

describe('bin/generate.ts', () => {
  it('bundles the tracker and declares the analytics routes', async () => {
    const { projectRoot, contextPath } = createProject();

    const proc = Bun.spawn(['bun', 'run', join(packageRoot, 'bin', 'generate.ts'), '--putnami-context', contextPath], {
      cwd: projectRoot,
      stdout: 'pipe',
      stderr: 'pipe',
    });
    const stdout = await new Response(proc.stdout).text();
    const stderr = await new Response(proc.stderr).text();
    await proc.exited;
    expect(proc.exitCode, `stderr: ${stderr}\nstdout: ${stdout}`).toBe(0);

    const bundleDir = join(projectRoot, '.gen', 'public', 'analytics');
    const bundles = readdirSync(bundleDir).filter((f) => /^analytics\.[A-Za-z0-9]+\.js\.gz$/.test(f));
    expect(bundles).toHaveLength(1);
    expect(existsSync(join(bundleDir, bundles[0] as string))).toBe(true);

    const summary = stdout
      .split('\n')
      .filter((line) => line.trim().length > 0)
      .map((line) => JSON.parse(line) as { type: string; data?: { assets?: Record<string, string> } })
      .find((event) => event.type === 'summary');
    expect(Object.keys(summary?.data?.assets ?? {})).toHaveLength(1);

    const fragment = JSON.parse(readFileSync(join(projectRoot, '.gen', 'http-routes.d', 'analytics.json'), 'utf8')) as {
      routes: { path: string; match: string; methods: string[]; provenance: { package: string } }[];
    };
    expect(fragment.routes.map((route) => route.path)).toEqual(['/analytics/', '/_putnami/analytics/events']);
    expect(fragment.routes[0]?.match).toBe('prefix');
    expect(fragment.routes[1]?.methods).toEqual(['POST']);
    expect(fragment.routes.every((route) => route.provenance.package === '@putnami/analytics')).toBe(true);
  }, 60_000);

  // A published package has no `.ts` under `src/`: the TypeScript extension
  // transpiles the `./tracker` export to `src/client/entry.js` and ships only
  // declarations beside it. The hook must bundle from that file, or every npm
  // consumer's `putnami build` fails on `ModuleNotFound` while this workspace,
  // which consumes the source, stays green.
  it('bundles the tracker from the published entry when the source is absent', async () => {
    const publishedRoot = mkdtempSync(join(tmpdir(), 'putnami-analytics-published-'));
    tempRoots.push(publishedRoot);
    const published = await Bun.build({
      entrypoints: [join(packageRoot, 'src', 'client', 'entry.ts')],
      format: 'esm',
      packages: 'external',
      root: packageRoot,
      outdir: publishedRoot,
      target: 'browser',
    });
    if (!published.success) {
      throw new Error(published.logs.map((log) => log.message).join('\n'));
    }
    const publishedEntry = join(publishedRoot, 'src', 'client', 'entry.js');
    expect(existsSync(publishedEntry)).toBe(true);
    expect(existsSync(join(publishedRoot, 'src', 'client', 'entry.ts'))).toBe(false);

    const { projectRoot } = createProject();
    const scripts = await buildTrackerBundle(publishedEntry, 'public', projectRoot);

    expect(scripts).toHaveLength(1);
    const bundle = join(projectRoot, '.gen', 'public', 'analytics', scripts[0]?.path.split('/').pop() as string);
    expect(existsSync(bundle)).toBe(true);
    const source = new TextDecoder().decode(Bun.gunzipSync(readFileSync(bundle)));
    expect(source).toContain('data-putnami-analytics');
    expect(source).toContain('putnami.analytics.session');
  }, 60_000);
});
