/**
 * Regression tests for the preBuild generate hook, covering two defects.
 *
 * ## 1. Silent "no exports, no assets" on an unloadable entry point
 *
 * `findApplication` swallows import errors, so a project whose entry point
 * imports a module another extension generates into `.gen` produced a hook that
 * exited 0 with no `schema/capabilities.json`. The failure only surfaced three
 * layers away, in the TypeScript extension, as
 * "generated server loaders require schema/capabilities.json" — with no cause.
 *
 * Pinned invariants:
 *  1. An entry point that cannot be LOADED is a hard hook error that names the
 *     entry point and the underlying import failure.
 *  2. A module that loads and simply exposes no Application stays legitimate
 *     (plain libraries depend on @putnami/application too) and still yields an
 *     empty, successful hook run.
 *  3. A module that loads but whose `app()` factory throws stays legitimate too:
 *     a factory may demand deploy-time configuration at build time
 *     (typescript/samples/07-authentication requires OAUTH_CLIENT_ID). Only the
 *     unloadable case is fatal — widening it breaks green builds.
 *
 * ## 2. Secret names dropped from the deployability manifest
 *
 * The hook emitted `.gen/infra/secrets.json` straight after `app.build()`,
 * without walking the plugin tree for `ConfigContributor` blocks — so every
 * secret a DEPENDENCY declares was missing from the committed
 * `infra/requirements.json`, while `schema/config.json`, written by the
 * configExtract hook (which did walk), listed it. Nothing failed: an omitted
 * secret name silently skips provisioning, binding and preflight.
 *
 * The route-owned half was already covered — `Application.build()` imports the
 * generated `*-loader` modules from the capability producer's `postGenerate` —
 * and the fix keeps that import in the shared activation helper so BOTH halves
 * are one readable contract instead of one call and one side effect. The
 * fixtures below use a duck-typed Application whose `build()` does no importing,
 * which is what makes the helper's own loop observable.
 *
 * Pinned invariants:
 *  4. A loader-registered sensitive field reaches the fragment.
 *  5. A ConfigContributor-owned sensitive field reaches the fragment.
 *  6. Removing one sensitive field removes only its secret name; removing the
 *     last one removes the fragment.
 *  7. A secret VALUE never reaches the fragment or the hook's own output.
 *  8. A generated loader that cannot be imported fails the hook instead of
 *     committing a manifest built from a half-activated registry.
 */
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'bun:test';

const HOOK_SCRIPT = resolve(import.meta.dir, '../../bin/generate.ts');
// Absolute path to the runtime entry the hook itself resolves via
// `@putnami/runtime`. Importing the same file from the fixture guarantees both
// sides share one module realm (bun keys its module cache on the resolved real
// path), so the fixture's `configToken()` calls land in the registry the hook
// reads.
const RUNTIME_INDEX = resolve(import.meta.dir, '../../../runtime/src/index.ts');

interface HookRun {
  exitCode: number;
  stdout: string;
  stderr: string;
}

describe('generate hook (subprocess)', () => {
  let workloadDir: string;

  beforeEach(() => {
    workloadDir = mkdtempSync(join(tmpdir(), 'putnami-generate-hook-'));
    writeFileSync(join(workloadDir, 'package.json'), JSON.stringify({ name: '@fixture/app', main: 'src/main.ts' }));
    mkdirSync(join(workloadDir, 'src'), { recursive: true });
  });

  afterEach(() => {
    rmSync(workloadDir, { recursive: true, force: true });
  });

  function writeMain(source: string): void {
    writeFileSync(join(workloadDir, 'src', 'main.ts'), source);
  }

  function runHook(): HookRun {
    const contextPath = join(workloadDir, 'hook-context.json');
    writeFileSync(
      contextPath,
      JSON.stringify({
        workspaceRoot: workloadDir,
        projectRoot: workloadDir,
        extensionRoot: resolve(import.meta.dir, '../..'),
        outputRoot: join(workloadDir, '.gen'),
        cacheRoot: join(workloadDir, '.putnami-cache'),
        debug: false,
        hook: 'preBuild',
        extension: '@putnami/application',
        projectName: '@fixture/app',
        mode: 'build',
      }),
    );

    const env = { ...process.env };
    env['PUTNAMI_PROJECT_ROOT'] = undefined;
    env['PUTNAMI_WORKSPACE_ROOT'] = undefined;

    const result = Bun.spawnSync({
      cmd: [process.execPath, HOOK_SCRIPT, '--putnami-context', contextPath],
      cwd: workloadDir,
      env,
      stdout: 'pipe',
      stderr: 'pipe',
    });
    return {
      exitCode: result.exitCode,
      stdout: result.stdout.toString(),
      stderr: result.stderr.toString(),
    };
  }

  it('fails loudly when the entry point cannot be imported', () => {
    // The reproducing shape: the entry point imports a module another
    // extension's hook generates into .gen, which does not exist yet.
    writeMain(`
      import '../.gen/src/app/.react-application.gen.tsx';
      export const app = () => ({
        build: async () => ({ exports: {} }),
        use() { return this; },
        getPlugins: () => [],
        start: async () => {},
      });
    `);

    const run = runHook();
    expect(run.exitCode).not.toBe(0);
    const output = run.stdout + run.stderr;
    expect(output).toContain('src/main.ts');
    expect(output).toContain('.react-application.gen');
    // The remedy has to travel with the failure — the whole bug was that the
    // reader had no way back to the hook ordering from the symptom.
    expect(output).toContain('order');
  });

  it('still succeeds for a module that loads but exposes no Application', () => {
    writeMain('export const notAnApp = 1;\n');

    const run = runHook();
    expect(run.exitCode).toBe(0);
    expect(emptySummaryExports(run)).toEqual({});
  });

  // Regression guard for the misclassification this hook shipped with first:
  // resolveApplicationFromModule INVOKES the factory, so probing the import and
  // the factory in one try reported 07-authentication (which calls
  // requireEnv('OAUTH_CLIENT_ID') inside `app()`) as an unloadable module and
  // failed a build that is green on main.
  it('still succeeds when the app() factory throws for missing runtime config', () => {
    writeMain(`
      function requireEnv(name: string): string {
        const value = process.env[name];
        if (!value) throw new Error(\`Missing required environment variable: \${name}\`);
        return value;
      }
      export const app = () => ({
        clientId: requireEnv('OAUTH_CLIENT_ID_FIXTURE'),
        build: async () => ({ exports: {} }),
        use() { return this; },
        getPlugins: () => [],
        start: async () => {},
      });
    `);

    const run = runHook();
    expect(run.exitCode).toBe(0);
    expect(emptySummaryExports(run)).toEqual({});
  });

  function emptySummaryExports(run: HookRun): Record<string, string> | undefined {
    const summary = run.stdout
      .split('\n')
      .filter((line) => line.includes('"type":"summary"'))
      .map((line) => JSON.parse(line) as { data?: { exports?: Record<string, string> } })
      .at(-1);
    return summary?.data?.exports;
  }
});

/**
 * End-to-end lifecycle coverage for the infra-requirements scratch fragment the
 * preBuild hook owns (`.gen/infra/secrets.json`).
 *
 * The fixture reproduces the three registration paths a real workload has, in a
 * duck-typed Application so the assertion is about the HOOK's activation order
 * and not about any plugin's internals:
 *
 *  - `server.host`   — the workload's own module-level block. Not sensitive, so
 *                      it must never appear as a secret; it is here to prove the
 *                      registry the buggy version saw was non-empty (the defect
 *                      was a partial registry, not an absent one).
 *  - `analytics.secret` — route-owned, reachable only by importing the generated
 *                      `api-loader`. This is the field the original report lost.
 *  - `billing.apiKey`   — dependency-owned, carried in by
 *                      `registerContributedConfigs()`, and canonicalized to
 *                      `billing.api_key` by the infra name grammar.
 */
describe('generate hook infra requirements (subprocess)', () => {
  let workloadDir: string;

  // A literal default on the route-owned secret. Manifests carry canonical
  // NAMES only, so this string must not appear anywhere the hook writes or
  // prints — it is the tripwire for a serialization regression.
  const SECRET_VALUE = 'fixture-secret-value-must-never-be-serialized';

  beforeEach(() => {
    workloadDir = mkdtempSync(join(tmpdir(), 'putnami-generate-infra-'));
    writeFileSync(join(workloadDir, 'package.json'), JSON.stringify({ name: '@fixture/app', main: 'src/main.ts' }));
    mkdirSync(join(workloadDir, 'src'), { recursive: true });
  });

  afterEach(() => {
    rmSync(workloadDir, { recursive: true, force: true });
  });

  const RUNTIME = JSON.stringify(RUNTIME_INDEX);

  /**
   * Write the generated route loader. Real ones are emitted by the api plugin
   * under `.gen/src/api/` and statically re-export every route handler; what
   * matters to this hook is only that importing the module runs the handlers'
   * module-level `configToken()` calls.
   */
  function writeLoader(): string {
    const dir = join(workloadDir, '.gen', 'src', 'api');
    mkdirSync(dir, { recursive: true });
    const loaderPath = join(dir, 'loader.ts');
    writeFileSync(
      loaderPath,
      `import { Config, Default, Sensitive, configToken } from ${RUNTIME};
export const AnalyticsConfig = Config('analytics', {
  endpoint: Default(String, 'https://analytics.invalid'),
  secret: Sensitive(Default(String, ${JSON.stringify(SECRET_VALUE)})),
});
configToken(AnalyticsConfig);
`,
    );
    return loaderPath;
  }

  /**
   * Write the workload entry point.
   *
   * `loaderPath` is what the duck `build()` reports as its `api-loader` export;
   * pointing it at a file that does not exist reproduces a broken generated
   * loader. `contributed` toggles the dependency-owned block so a test can
   * remove one sensitive field and watch only that secret disappear.
   */
  function writeMain(options: { loaderPath: string; contributed: boolean }): void {
    writeFileSync(
      join(workloadDir, 'src', 'main.ts'),
      `import { Config, Default, Sensitive, configToken, registerContributedConfig } from ${RUNTIME};

export const ServerConfig = Config('server', { host: Default(String, '0.0.0.0') });
configToken(ServerConfig);

const BillingConfig = Config('billing', { apiKey: Sensitive(String) });

export const app = () => ({
  build: async () => ({ exports: { 'api-loader': ${JSON.stringify(options.loaderPath)} } }),
  use() { return this; },
  getPlugins: () => [],
  start: async () => {},
  registerContributedConfigs: () => {
    ${options.contributed ? 'registerContributedConfig(BillingConfig);' : ''}
  },
});
`,
    );
  }

  function runHook(): HookRun {
    const contextPath = join(workloadDir, 'hook-context.json');
    writeFileSync(
      contextPath,
      JSON.stringify({
        workspaceRoot: workloadDir,
        projectRoot: workloadDir,
        extensionRoot: resolve(import.meta.dir, '../..'),
        outputRoot: join(workloadDir, '.gen'),
        cacheRoot: join(workloadDir, '.putnami-cache'),
        debug: false,
        hook: 'preBuild',
        extension: '@putnami/application',
        projectName: '@fixture/app',
        mode: 'build',
      }),
    );

    const env = { ...process.env };
    env['PUTNAMI_PROJECT_ROOT'] = undefined;
    env['PUTNAMI_WORKSPACE_ROOT'] = undefined;

    const result = Bun.spawnSync({
      cmd: [process.execPath, HOOK_SCRIPT, '--putnami-context', contextPath],
      cwd: workloadDir,
      env,
      stdout: 'pipe',
      stderr: 'pipe',
    });
    return {
      exitCode: result.exitCode,
      stdout: result.stdout.toString(),
      stderr: result.stderr.toString(),
    };
  }

  const fragmentPath = (): string => join(workloadDir, '.gen', 'infra', 'secrets.json');

  function readFragment(): { protocolVersion: number; secrets?: string[] } {
    return JSON.parse(readFileSync(fragmentPath(), 'utf-8')) as { protocolVersion: number; secrets?: string[] };
  }

  it('carries both loader-registered and contributor-owned secret names', () => {
    writeMain({ loaderPath: writeLoader(), contributed: true });

    const run = runHook();
    expect(run.exitCode).toBe(0);

    const fragment = readFragment();
    // Sorted, deduplicated, canonicalized: `apiKey` becomes `api_key` because
    // the infra resource-name grammar is lowercase snake_case.
    expect(fragment.secrets).toEqual(['analytics.secret', 'billing.api_key']);
    // The non-sensitive workload block proves the registry was never empty —
    // the defect dropped fields, it did not fail to read anything at all.
    expect(fragment.secrets).not.toContain('server.host');
  }, 30_000);

  it('never serializes a secret value into the fragment or the hook output', () => {
    writeMain({ loaderPath: writeLoader(), contributed: true });

    const run = runHook();
    expect(run.exitCode).toBe(0);

    expect(readFileSync(fragmentPath(), 'utf-8')).not.toContain(SECRET_VALUE);
    expect(run.stdout + run.stderr).not.toContain(SECRET_VALUE);
  }, 30_000);

  it('drops only the secret whose sensitive field was removed', () => {
    const loaderPath = writeLoader();
    writeMain({ loaderPath, contributed: true });
    expect(runHook().exitCode).toBe(0);
    expect(readFragment().secrets).toEqual(['analytics.secret', 'billing.api_key']);

    // Remove the dependency-owned block only. The route-owned secret must
    // survive: the fragment is rebuilt from the registry, never patched.
    writeMain({ loaderPath, contributed: false });
    expect(runHook().exitCode).toBe(0);
    expect(readFragment().secrets).toEqual(['analytics.secret']);
  }, 60_000);

  it('removes the fragment when the last sensitive field is gone', () => {
    const loaderPath = writeLoader();
    writeMain({ loaderPath, contributed: true });
    expect(runHook().exitCode).toBe(0);
    expect(existsSync(fragmentPath())).toBe(true);

    // A loader that registers nothing sensitive, and no contributed block: the
    // stale fragment must go, or the next generator sync commits secrets this
    // workload no longer declares.
    writeFileSync(loaderPath, 'export const nothing = 1;\n');
    writeMain({ loaderPath, contributed: false });
    expect(runHook().exitCode).toBe(0);
    expect(existsSync(fragmentPath())).toBe(false);
  }, 60_000);

  it('is byte-identical across repeated runs', () => {
    // The scratch fragment is written atomically and the same generate task may
    // run concurrently for the build and test pipelines, so two writers must
    // produce the same bytes for the same input — that is what makes the
    // last-rename-wins race harmless, and what makes a batched run equal to a
    // solo one.
    writeMain({ loaderPath: writeLoader(), contributed: true });
    expect(runHook().exitCode).toBe(0);
    const first = readFileSync(fragmentPath(), 'utf-8');
    expect(runHook().exitCode).toBe(0);
    expect(readFileSync(fragmentPath(), 'utf-8')).toBe(first);
  }, 60_000);

  it('degrades with a warning when the app predates registerContributedConfigs', () => {
    // `isApplicationLike` (bin/_find-application.ts) accepts any object carrying
    // build/use/getPlugins/start, and that narrowness is the point: a workload
    // bundling its own @putnami/application hands the hook an Application from a
    // sibling module realm whose method set is whatever THAT copy shipped. This
    // hook runs on every `putnami build`, so calling the newer method blind broke
    // a supported shape with a raw TypeError, after build() had already run.
    //
    // Nothing in this repo reproduces it — every project resolves the workspace
    // copy — which is exactly why it needs a fixture pinned at the documented
    // four-method contract and nothing more.
    const loaderPath = writeLoader();
    writeFileSync(
      join(workloadDir, 'src', 'main.ts'),
      `export const app = () => ({
  build: async () => ({ exports: { 'api-loader': ${JSON.stringify(loaderPath)} } }),
  use() { return this; },
  getPlugins: () => [],
  start: async () => {},
});
`,
    );

    const run = runHook();
    expect(run.exitCode).toBe(0);

    // The half that does not depend on the missing method still lands: the
    // route-owned secret came from importing the generated loader.
    expect(readFragment().secrets).toEqual(['analytics.secret']);

    // Degraded, but never silently — a manifest short a dependency's secrets is
    // the defect this helper closes, so the run has to say so and say why.
    const output = run.stdout + run.stderr;
    expect(output).toContain('registerContributedConfigs');
    expect(output).toContain('dedupe or upgrade it');
  }, 30_000);

  it('fails loudly when a generated loader cannot be imported', () => {
    // A missing loader means the route-owned configs never registered. Emitting
    // the fragment anyway would commit a manifest built from a half-activated
    // registry — silently short a secret — so the hook has to fail instead.
    writeMain({ loaderPath: join(workloadDir, '.gen', 'src', 'api', 'missing-loader.ts'), contributed: true });

    const run = runHook();
    expect(run.exitCode).not.toBe(0);
    const output = run.stdout + run.stderr;
    expect(output).toContain('Could not import api-loader loader at');
    expect(existsSync(fragmentPath())).toBe(false);
  }, 30_000);
});
