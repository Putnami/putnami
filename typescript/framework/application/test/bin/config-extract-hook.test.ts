/**
 * Regression tests: the config-extract hook silently
 * no-oped for TypeScript workloads — a run that registered zero config
 * definitions returned `status:'empty'`/`status:'skipped'` and left a stale
 * committed `schema/config.json` untouched, which the Go-side runner then
 * re-reported as a fresh extraction ("Extracted 5 config blocks ()").
 *
 * Pinned invariants:
 *  1. A workload whose configs register at module load regenerates a
 *     drifted `schema/config.json` from source.
 *  2. Zero registrations + an existing manifest with config blocks = hard
 *     error (never a silent keep), for both the "app found but registry
 *     empty" and the "no Application could be loaded" paths.
 *  3. The legitimate empty state (no configs, no manifest) still succeeds.
 *  4. Route-owned and dependency-owned blocks reach BOTH the schema and the
 *     infra-requirements fragment. Both hooks now activate the registry through
 *     `bin/_activate-config.ts`, so the committed schema and the committed
 *     deployability manifest can no longer describe two different workloads.
 *  5. The hook writes NOTHING that `build-generate` declares as its own output:
 *     `.gen/schema/capabilities.json`, its feature evidence and
 *     `.gen/design/graph.json` survive the hook byte-for-byte, and no committed
 *     `schema/capabilities.json` appears. Publishing from here produced a second,
 *     different manifest for one declared-output path — see the `app.build()`
 *     call in `bin/config-extract.ts`.
 */
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { findExistingManifest, manifestCandidatePaths } from '../../bin/config-extract';

const HOOK_SCRIPT = resolve(import.meta.dir, '../../bin/config-extract.ts');
// Absolute path to the runtime entry the hook itself resolves via
// `@putnami/runtime` — importing the same file from the fixture guarantees
// both sides share one module realm (bun keys its module cache on the
// resolved real path).
const RUNTIME_INDEX = resolve(import.meta.dir, '../../../runtime/src/index.ts');
// The real Application module, imported by absolute path for the same
// module-realm reason. A fixture built on it exercises the production
// `Application.build()` — capability producer, design-graph emitter and all —
// rather than a duck app that can only report what it was asked to do.
const APPLICATION_MODULE = resolve(import.meta.dir, '../../src/application/application.ts');

const DUCK_APP = `
export const app = () => ({
  build: async () => ({ exports: {} }),
  use() { return this; },
  getPlugins: () => [],
  start: async () => {},
  registerContributedConfigs: () => {},
});
`;

const MAIN_WITH_CONFIG = `
import { Config, Default, configToken } from ${JSON.stringify(RUNTIME_INDEX)};

export const ServerConfig = Config('server', {
  host: Default(String, '0.0.0.0'),
  port: Default(Number, 8080),
});
configToken(ServerConfig);
${DUCK_APP}
`;

interface HookRun {
  exitCode: number;
  stdout: string;
  stderr: string;
}

describe('config-extract hook (subprocess)', () => {
  let workloadDir: string;

  beforeEach(() => {
    workloadDir = mkdtempSync(join(tmpdir(), 'putnami-config-extract-'));
    writeFileSync(join(workloadDir, 'package.json'), JSON.stringify({ name: '@fixture/app', main: 'src/main.ts' }));
    mkdirSync(join(workloadDir, 'src'), { recursive: true });
  });

  afterEach(() => {
    rmSync(workloadDir, { recursive: true, force: true });
  });

  function writeMain(source: string): void {
    writeFileSync(join(workloadDir, 'src', 'main.ts'), source);
  }

  function plantManifest(blocks: number): string {
    const schemaDir = join(workloadDir, 'schema');
    mkdirSync(schemaDir, { recursive: true });
    const manifestPath = join(schemaDir, 'config.json');
    writeFileSync(
      manifestPath,
      JSON.stringify(
        {
          appName: '@fixture/app',
          version: '',
          // No schemaHash on purpose: mimics a manifest written by an older
          // extractor — the artifact behind the "Extracted 5 config blocks ()"
          // (empty-hash) log from the original report.
          configs: Array.from({ length: blocks }, (_, i) => ({ path: `stale.block${i}`, fields: [] })),
        },
        null,
        2,
      ),
    );
    return manifestPath;
  }

  function plantGeneratedArtifact(relative: string, content: string): string {
    const path = join(workloadDir, relative);
    mkdirSync(join(path, '..'), { recursive: true });
    writeFileSync(path, content);
    return path;
  }

  function runHook(projectName?: string): HookRun {
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
        hook: 'configExtract',
        extension: '@putnami/application',
        mode: 'config-extract',
        // Omitted entirely when undefined so the context mirrors an older Go
        // runner's output (backward-compat / fallback path).
        ...(projectName === undefined ? {} : { projectName }),
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

  it('regenerates a drifted schema/config.json from source', () => {
    writeMain(MAIN_WITH_CONFIG);
    const manifestPath = plantManifest(5);

    const run = runHook();
    expect(run.exitCode).toBe(0);

    const manifest = JSON.parse(readFileSync(manifestPath, 'utf-8')) as {
      schemaHash: string;
      configs: Array<{ path: string; fields: Array<{ name: string }> }>;
    };
    // The drifted content is fully replaced by what the source declares.
    expect(manifest.configs.map((c) => c.path)).toEqual(['server']);
    expect(manifest.configs[0].fields.map((f) => f.name)).toEqual(['host', 'port']);
    expect(manifest.schemaHash).toMatch(/^sha256:[0-9a-f]{16}$/);
    // The companion JSON Schema is emitted alongside the manifest.
    expect(existsSync(join(workloadDir, 'schema', 'config.jsonschema.json'))).toBe(true);
    expect(run.stdout).toContain('"status":"ok"');
  }, 30_000);

  it('writes the putnami project identity from the context as manifest appName', () => {
    // The npm package.json name (@fixture/app) can never equal a project path
    // (paths contain slashes). The Go runner plumbs ctx.Project.Name — the
    // putnami identity — through the context; the hook must key appName on
    // that, not on the package.json name.
    writeMain(MAIN_WITH_CONFIG);
    const manifestPath = plantManifest(1);

    const run = runHook('identity/workloads/auth-server');
    expect(run.exitCode).toBe(0);

    const manifest = JSON.parse(readFileSync(manifestPath, 'utf-8')) as { appName: string };
    expect(manifest.appName).toBe('identity/workloads/auth-server');
    expect(manifest.appName).not.toBe('@fixture/app');
  }, 30_000);

  it('falls back to the package.json name when the context omits projectName (backward compatible)', () => {
    // An older Go runner writes no projectName — the hook must reproduce the
    // pre-fix behavior (getCurrentProject().name = the package.json name).
    writeMain(MAIN_WITH_CONFIG);
    const manifestPath = plantManifest(1);

    const run = runHook();
    expect(run.exitCode).toBe(0);

    const manifest = JSON.parse(readFileSync(manifestPath, 'utf-8')) as { appName: string };
    expect(manifest.appName).toBe('@fixture/app');
  }, 30_000);

  it('fails loudly when the registry is empty but a committed manifest has config blocks', () => {
    // App loads fine but registers nothing — the earlier behavior was a
    // silent `status:'empty'` that left the stale manifest in place.
    writeMain(DUCK_APP);
    const manifestPath = plantManifest(5);

    const run = runHook();
    expect(run.exitCode).not.toBe(0);
    expect(run.stdout).toContain('zero config definitions');
    expect(run.stdout).toContain('5 config block(s)');
    // The stale file is left for the user to inspect — never deleted, never
    // re-reported as fresh.
    expect(existsSync(manifestPath)).toBe(true);
  }, 30_000);

  it('fails loudly when no Application loads but a committed manifest has config blocks', () => {
    // Mirrors the real-world reproduction: the entry point throws at import
    // time (e.g. a requireEnv() guard), findApplication swallows it, and the
    // hook used to report `status:'skipped'` while the stale committed
    // schema survived untouched.
    writeMain(`throw new Error('boom: missing OAUTH_CLIENT_ID');`);
    plantManifest(5);

    const run = runHook();
    expect(run.exitCode).not.toBe(0);
    expect(run.stdout).toContain('no Application could be loaded');
    // Entry-point diagnostics surface the underlying import failure.
    expect(run.stdout).toContain('boom: missing OAUTH_CLIENT_ID');
  }, 30_000);

  it('carries route-owned and dependency-owned secrets into both the schema and the infra fragment', () => {
    // Same three registration paths the preBuild hook fixture uses, so the two
    // hooks are pinned against ONE expectation: whatever `schema/config.json`
    // declares sensitive, `.gen/infra/secrets.json` names.
    const loaderDir = join(workloadDir, '.gen', 'src', 'api');
    mkdirSync(loaderDir, { recursive: true });
    const loaderPath = join(loaderDir, 'loader.ts');
    writeFileSync(
      loaderPath,
      `import { Config, Sensitive, configToken } from ${JSON.stringify(RUNTIME_INDEX)};
configToken(Config('analytics', { secret: Sensitive(String) }));
`,
    );
    writeMain(
      `import { Config, Default, Sensitive, configToken, registerContributedConfig } from ${JSON.stringify(RUNTIME_INDEX)};
configToken(Config('server', { host: Default(String, '0.0.0.0') }));
const BillingConfig = Config('billing', { apiKey: Sensitive(String) });
export const app = () => ({
  build: async () => ({ exports: { 'api-loader': ${JSON.stringify(loaderPath)} } }),
  use() { return this; },
  getPlugins: () => [],
  start: async () => {},
  registerContributedConfigs: () => { registerContributedConfig(BillingConfig); },
});
`,
    );

    const run = runHook();
    expect(run.exitCode).toBe(0);

    const manifest = JSON.parse(readFileSync(join(workloadDir, 'schema', 'config.json'), 'utf-8')) as {
      configs: Array<{ path: string }>;
    };
    expect(manifest.configs.map((c) => c.path).sort()).toEqual(['analytics', 'billing', 'server']);

    const fragment = JSON.parse(readFileSync(join(workloadDir, '.gen', 'infra', 'secrets.json'), 'utf-8')) as {
      secrets: string[];
    };
    expect(fragment.secrets).toEqual(['analytics.secret', 'billing.api_key']);
  }, 30_000);

  it('leaves the capability manifest, feature evidence and design graph build-generate owns untouched', () => {
    // `sites/putnami.dev/schema/capabilities.json` must not flip between two
    // producers. `build-generate` declares `<project>/.gen` as an output it owns
    // exclusively and `schema/capabilities.json` as an artifact whose sole author
    // is the framework producer; this hook used to run `Application.build()` with
    // the default publication ON, which (a) DELETES the manifest and its feature
    // evidence up front (`invalidateCapabilityManifest`) and (b) rewrites the
    // manifest from a different input set — `.gen/infra/*.json` fragments are on
    // disk here but not during build-generate, and only build-generate merges the
    // generated loader exports back in.
    //
    // The fixture is the real Application, so publication is decided by the
    // production code path, not by the fixture.
    writeMain(`
import { Config, Default, configToken } from ${JSON.stringify(RUNTIME_INDEX)};
import { application } from ${JSON.stringify(APPLICATION_MODULE)};
configToken(Config('server', { host: Default(String, '0.0.0.0') }));
export const app = () => application();
`);
    const manifest = plantGeneratedArtifact('.gen/schema/capabilities.json', '{"owner":"build-generate"}\n');
    const evidence = plantGeneratedArtifact(
      '.gen/schema/feature-evidence/typescript-framework.json',
      '{"owner":"build-generate-evidence"}\n',
    );
    const graph = plantGeneratedArtifact('.gen/design/graph.json', '{"owner":"build-generate-graph"}\n');

    const run = runHook();

    // Byte-for-byte, not just "still exists": a rewritten manifest is the same
    // defect as a deleted one.
    expect(readFileSync(manifest, 'utf-8')).toBe('{"owner":"build-generate"}\n');
    expect(readFileSync(evidence, 'utf-8')).toBe('{"owner":"build-generate-evidence"}\n');
    expect(readFileSync(graph, 'utf-8')).toBe('{"owner":"build-generate-graph"}\n');
    // Promotion into the tracked tree belongs to build-generate alone.
    expect(existsSync(join(workloadDir, 'schema', 'capabilities.json'))).toBe(false);
    // And the hook still does its own job on the same run.
    expect(run.exitCode).toBe(0);
    expect(run.stdout).toContain('"status":"ok"');
  }, 30_000);

  it('asks Application.build() for no capability and no design-graph publication', () => {
    // The invariant above depends on two explicit flags rather than on
    // `publishDesignGraph` defaulting to `publishCapabilityManifest`. Pin what the
    // hook actually requests, so a change to either default cannot silently hand
    // this hook a second write into build-generate's declared output.
    const optionsPath = join(workloadDir, 'build-options.json');
    writeMain(`
import { writeFileSync } from 'node:fs';
import { Config, Default, configToken } from ${JSON.stringify(RUNTIME_INDEX)};
configToken(Config('server', { host: Default(String, '0.0.0.0') }));
export const app = () => ({
  build: async (options) => {
    writeFileSync(${JSON.stringify(optionsPath)}, JSON.stringify(options ?? null));
    return { exports: {} };
  },
  use() { return this; },
  getPlugins: () => [],
  start: async () => {},
  registerContributedConfigs: () => {},
});
`);

    const run = runHook('identity/workloads/auth-server');
    expect(run.exitCode).toBe(0);

    const options = JSON.parse(readFileSync(optionsPath, 'utf-8')) as {
      projectName?: string;
      publishCapabilityManifest?: boolean;
      publishDesignGraph?: boolean;
      publishMigrationArtifacts?: boolean;
    };
    expect(options.publishCapabilityManifest).toBe(false);
    expect(options.publishDesignGraph).toBe(false);
    // The migration bundle has the same single writer (build-generate).
    expect(options.publishMigrationArtifacts).toBe(false);
    // The identity the manifest keys on still reaches build().
    expect(options.projectName).toBe('identity/workloads/auth-server');
  }, 30_000);

  it('still extracts when the app predates registerContributedConfigs', () => {
    // The shared DUCK_APP above carries the method, which is precisely how this
    // suite masked the drift: `isApplicationLike` never required it, so a
    // sibling-realm app that satisfies the documented four-method contract used
    // to crash both hooks. Pin the bare contract here too, so the shared helper
    // can never regress on one hook while the other stays green.
    writeMain(`
import { Config, Default, configToken } from ${JSON.stringify(RUNTIME_INDEX)};
configToken(Config('server', { host: Default(String, '0.0.0.0') }));
export const app = () => ({
  build: async () => ({ exports: {} }),
  use() { return this; },
  getPlugins: () => [],
  start: async () => {},
});
`);

    const run = runHook();
    expect(run.exitCode).toBe(0);

    const manifest = JSON.parse(readFileSync(join(workloadDir, 'schema', 'config.json'), 'utf-8')) as {
      configs: Array<{ path: string }>;
    };
    expect(manifest.configs.map((c) => c.path)).toEqual(['server']);
    expect(run.stdout + run.stderr).toContain('registerContributedConfigs');
  }, 30_000);

  it('still succeeds as empty when no configs are declared and no manifest exists', () => {
    writeMain(DUCK_APP);

    const run = runHook();
    expect(run.exitCode).toBe(0);
    expect(run.stdout).toContain('"status":"empty"');
    expect(existsSync(join(workloadDir, 'schema', 'config.json'))).toBe(false);
  }, 30_000);
});

describe('manifestCandidatePaths', () => {
  it('probes the committed default first, then the gen fallback', () => {
    expect(manifestCandidatePaths(undefined)).toEqual(['schema/config.json', '.gen/config-schema.json']);
  });

  it('probes the gen fallback first when output is disabled', () => {
    expect(manifestCandidatePaths(false)).toEqual(['.gen/config-schema.json', 'schema/config.json']);
  });

  it('probes a custom path first, then both known locations', () => {
    expect(manifestCandidatePaths('custom/config.json')).toEqual([
      'custom/config.json',
      'schema/config.json',
      '.gen/config-schema.json',
    ]);
  });
});

describe('findExistingManifest', () => {
  let dir: string;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), 'putnami-existing-manifest-'));
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  function write(relative: string, content: string): void {
    const path = join(dir, relative);
    mkdirSync(join(path, '..'), { recursive: true });
    writeFileSync(path, content);
  }

  it('returns null when nothing exists', () => {
    expect(findExistingManifest(dir, undefined)).toBeNull();
  });

  it('reports a manifest with config blocks as evidence', () => {
    write('schema/config.json', JSON.stringify({ configs: [{ path: 'server', fields: [] }] }));
    const evidence = findExistingManifest(dir, undefined);
    expect(evidence?.blocks).toBe(1);
    expect(evidence?.path).toBe(join(dir, 'schema/config.json'));
  });

  it('treats a zero-block manifest as consistent with an empty registry', () => {
    write('schema/config.json', JSON.stringify({ configs: [] }));
    expect(findExistingManifest(dir, undefined)).toBeNull();
  });

  it('treats an unparsable manifest as evidence (fail closed)', () => {
    write('schema/config.json', 'not json');
    const evidence = findExistingManifest(dir, undefined);
    expect(evidence).not.toBeNull();
    expect(evidence?.blocks).toBeNull();
  });

  it('finds a stale gen fallback even when the committed path is active', () => {
    // The Go-side runner peeks the fallback location in default mode too —
    // the hook must probe the same superset so the two sides never disagree.
    write('.gen/config-schema.json', JSON.stringify({ configs: [{ path: 'app', fields: [] }] }));
    const evidence = findExistingManifest(dir, undefined);
    expect(evidence?.path).toBe(join(dir, '.gen/config-schema.json'));
  });
});
