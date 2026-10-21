import { afterEach, describe, expect, it } from 'bun:test';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const packageRoot = join(import.meta.dir, '..');
const workspaceRoot = join(packageRoot, '..', '..', '..');
const hookPath = join(packageRoot, 'bin', 'generate.ts');
const projectName = '@example/web-hook-host';

/** The version 1 route evaluation a successful web hook reports, as the summary line carries it. */
const COMPLETE_EVALUATION_WIRE = '"httpRoutes":{"v":1,"state":"complete"}';

const tempRoots: string[] = [];

interface HookSummary {
  type: string;
  data?: {
    assets?: Record<string, string>;
    exports?: Record<string, string>;
    httpRoutes?: unknown;
  };
}

interface HookRun {
  /** The raw summary line the hook wrote to stdout. */
  line: string;
  summary: HookSummary;
}

interface FragmentRoute {
  match: string;
  path: string;
  methods: string[];
  provenance: { project: string; package: string; sourceKind: string; evidencePath?: string };
}

/** A consumer project outside the workspace, with the hook context the CLI runner writes. */
function createProject(): { projectRoot: string; contextPath: string } {
  const projectRoot = mkdtempSync(join(tmpdir(), 'putnami-web-hook-'));
  tempRoots.push(projectRoot);
  writeFileSync(join(projectRoot, 'package.json'), `${JSON.stringify({ name: 'web-hook-host' })}\n`);
  writeFileSync(join(projectRoot, 'putnami.json'), `${JSON.stringify({ name: projectName, tags: ['ts'] }, null, 2)}\n`);
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
      extension: '@putnami/web',
      projectName,
      mode: 'build',
    })}\n`,
  );
  return { projectRoot, contextPath };
}

/**
 * Links the packages a consumer's generated client entry and pages import, as
 * an installed consumer resolves them from its own `node_modules`.
 */
function linkConsumerDependencies(projectRoot: string): void {
  const modules = join(projectRoot, 'node_modules');
  mkdirSync(join(modules, '@putnami'), { recursive: true });
  symlinkSync(packageRoot, join(modules, '@putnami', 'web'), 'junction');
  symlinkSync(join(packageRoot, 'node_modules', 'react'), join(modules, 'react'), 'junction');
}

/** Runs the web preBuild hook the way the manifest declares it and returns its summary. */
async function runHook(projectRoot: string, contextPath: string): Promise<HookRun> {
  const proc = Bun.spawn([process.execPath, 'run', hookPath, '--putnami-context', contextPath], {
    cwd: projectRoot,
    stdout: 'pipe',
    stderr: 'pipe',
  });
  const [stdout, stderr] = await Promise.all([new Response(proc.stdout).text(), new Response(proc.stderr).text()]);
  await proc.exited;
  expect(proc.exitCode, `stderr: ${stderr}\nstdout: ${stdout}`).toBe(0);

  const summaries = stdout
    .split('\n')
    .filter((line) => line.trim().length > 0)
    .map((line) => ({ line, summary: JSON.parse(line) as HookSummary }))
    .filter((event) => event.summary.type === 'summary');
  expect(summaries).toHaveLength(1);
  return summaries[0] as HookRun;
}

/** Asserts the exact version 1 complete evaluation, with no other member. */
function expectCompleteEvaluation({ line, summary }: HookRun): void {
  expect(summary.data?.httpRoutes).toEqual({ v: 1, state: 'complete' });
  expect(line).toContain(COMPLETE_EVALUATION_WIRE);
}

afterEach(() => {
  for (const root of tempRoots.splice(0)) {
    rmSync(root, { recursive: true, force: true });
  }
});

describe('bin/generate.ts route evaluation', () => {
  it('reports a complete evaluation and writes no fragment without a React app directory', async () => {
    const { projectRoot, contextPath } = createProject();

    expectCompleteEvaluation(await runHook(projectRoot, contextPath));
    expect(existsSync(join(projectRoot, '.gen', 'http-routes.d', 'web.json'))).toBe(false);
  }, 60_000);

  it('reports a complete evaluation and writes no fragment for an app directory without route files', async () => {
    const { projectRoot, contextPath } = createProject();
    mkdirSync(join(projectRoot, 'src', 'app'), { recursive: true });

    expectCompleteEvaluation(await runHook(projectRoot, contextPath));
    expect(existsSync(join(projectRoot, '.gen', 'http-routes.d', 'web.json'))).toBe(false);
  }, 60_000);

  it('writes the page fragment and reports a complete evaluation for a real page', async () => {
    const { projectRoot, contextPath } = createProject();
    linkConsumerDependencies(projectRoot);
    mkdirSync(join(projectRoot, 'src', 'app'), { recursive: true });
    writeFileSync(
      join(projectRoot, 'src', 'app', 'page.tsx'),
      `export default function HomePage() {
  return <h1>Home</h1>;
}
`,
    );

    const result = await runHook(projectRoot, contextPath);
    expectCompleteEvaluation(result);

    const assetNames = Object.keys(result.summary.data?.assets ?? {});
    expect(assetNames.length).toBeGreaterThan(0);
    expect(assetNames.every((name) => name.startsWith('public/react/'))).toBe(true);

    const fragment = JSON.parse(readFileSync(join(projectRoot, '.gen', 'http-routes.d', 'web.json'), 'utf8')) as {
      routes: FragmentRoute[];
    };
    expect(fragment.routes.map((route) => [route.match, route.path, route.provenance.sourceKind])).toEqual([
      ['exact', '/', 'file-route'],
      ['prefix', '/react/', 'static-mount'],
    ]);
    expect(fragment.routes[0]?.methods).toEqual(['GET', 'HEAD']);
    expect(fragment.routes[0]?.provenance.evidencePath).toBe('src/app/page.tsx');
    expect(fragment.routes.every((route) => route.provenance.project === projectName)).toBe(true);
    expect(fragment.routes.every((route) => route.provenance.package === '@putnami/web')).toBe(true);
  }, 120_000);
});
