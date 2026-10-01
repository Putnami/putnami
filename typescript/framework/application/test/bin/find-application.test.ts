import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import {
  type EntryPointFailure,
  formatEntryPointFailures,
  isApplicationLike,
  resolveApplicationFromModule,
} from '../../bin/_find-application';
import { resolveConfigSchemaOutput } from '../../bin/config-extract';

describe('isApplicationLike', () => {
  // Duck typing is what lets the hook script accept Applications from
  // sibling realms (the workload bundles its own @putnami/application
  // copy in node_modules — instanceof would silently miss it).

  it('returns true for an object with the four lifecycle methods', () => {
    const fake = {
      build: async () => ({}),
      use: () => fake,
      getPlugins: () => [],
      start: async () => {},
    };
    expect(isApplicationLike(fake)).toBe(true);
  });

  it('returns false when any required method is missing', () => {
    expect(
      isApplicationLike({
        build: async () => ({}),
        use: () => null,
        getPlugins: () => [],
        // start missing
      }),
    ).toBe(false);
  });

  it('returns false for non-objects', () => {
    expect(isApplicationLike(null)).toBe(false);
    expect(isApplicationLike(undefined)).toBe(false);
    expect(isApplicationLike('app')).toBe(false);
    expect(isApplicationLike(42)).toBe(false);
  });
});

describe('resolveApplicationFromModule', () => {
  // Mirrors the resolution order shipped by the original generate.ts —
  // changing the order would silently break workloads that rely on a
  // particular shape.

  function makeApp() {
    return {
      build: async () => ({}),
      use: () => null,
      getPlugins: () => [],
      start: async () => {},
    };
  }

  it('invokes an `app()` factory when present', () => {
    const app = makeApp();
    const mod = { app: () => app };
    expect(resolveApplicationFromModule(mod)).toBe(app);
  });

  it('returns a default export that is itself application-like', () => {
    const app = makeApp();
    expect(resolveApplicationFromModule({ default: app })).toBe(app);
  });

  it('invokes a default-exported factory', () => {
    const app = makeApp();
    expect(resolveApplicationFromModule({ default: () => app })).toBe(app);
  });

  it('accepts a direct `Application` named export', () => {
    const app = makeApp();
    expect(resolveApplicationFromModule({ Application: app })).toBe(app);
  });

  it('returns null when no export matches', () => {
    expect(resolveApplicationFromModule({})).toBeNull();
    expect(resolveApplicationFromModule({ something: 1, else: 'x' })).toBeNull();
  });

  it('falls through when app() returns a non-application-like value', () => {
    expect(resolveApplicationFromModule({ app: () => ({ unrelated: true }) })).toBeNull();
  });
});

describe('findApplication entry-point probing', () => {
  // Integration-style: write a fake workload to disk and verify the
  // helper picks the right entry point and resolves the Application.

  let workloadDir: string;

  beforeEach(() => {
    workloadDir = mkdtempSync(join(tmpdir(), 'putnami-find-app-'));
  });

  afterEach(() => {
    rmSync(workloadDir, { recursive: true, force: true });
  });

  it('returns null when no entry point exists', async () => {
    // No package.json main and no conventional files — should resolve to null
    // rather than throw, so callers can SKIP cleanly.
    const { findApplication } = await import('../../bin/_find-application');
    writeFileSync(join(workloadDir, 'package.json'), JSON.stringify({ name: 'empty' }));
    const result = await findApplication(workloadDir);
    expect(result).toBeNull();
  });

  it('discovers an Application via src/main.ts when package.json main is absent', async () => {
    const { findApplication } = await import('../../bin/_find-application');
    writeFileSync(join(workloadDir, 'package.json'), JSON.stringify({ name: 'demo' }));
    mkdirSync(join(workloadDir, 'src'));
    writeFileSync(
      join(workloadDir, 'src', 'main.ts'),
      `
      export const app = () => ({
        build: async () => ({}),
        use() { return this; },
        getPlugins: () => [],
        start: async () => {},
      });
      `,
    );

    const app = await findApplication(workloadDir);
    expect(app).not.toBeNull();
    expect(typeof app?.build).toBe('function');
  });

  // Callers cannot treat "this is not an application" and "this
  // application could not be loaded" the same way. The first is a plain library
  // and must stay silent; the second is a broken build that used to be
  // swallowed into an empty hook result.
  it('classifies an unimportable entry point as an import error', async () => {
    const { findApplication } = await import('../../bin/_find-application');
    writeFileSync(join(workloadDir, 'package.json'), JSON.stringify({ name: 'broken' }));
    mkdirSync(join(workloadDir, 'src'));
    writeFileSync(join(workloadDir, 'src', 'main.ts'), "import './does-not-exist.ts';\n");

    const failures: EntryPointFailure[] = [];
    const diagnostics: string[] = [];
    expect(await findApplication(workloadDir, false, diagnostics, failures)).toBeNull();
    expect(failures.map((f) => f.kind)).toEqual(['import-error']);
    expect(failures[0].entryPoint).toBe('src/main.ts');
    expect(diagnostics[0]).toContain('src/main.ts');
    expect(formatEntryPointFailures(failures)).toContain('src/main.ts');
  });

  it('classifies a loadable module without an Application as no-application', async () => {
    const { findApplication } = await import('../../bin/_find-application');
    writeFileSync(join(workloadDir, 'package.json'), JSON.stringify({ name: 'library' }));
    mkdirSync(join(workloadDir, 'src'));
    writeFileSync(join(workloadDir, 'src', 'main.ts'), 'export const helper = 1;\n');

    const failures: EntryPointFailure[] = [];
    expect(await findApplication(workloadDir, false, undefined, failures)).toBeNull();
    expect(failures.map((f) => f.kind)).toEqual(['no-application']);
  });

  // The module loads fine; only the `app()` factory throws. Modelled on
  // typescript/samples/07-authentication, whose factory calls
  // requireEnv('OAUTH_CLIENT_ID') — that build is green and must stay green, so
  // this must NOT be reported as an unloadable module.
  it('classifies a throwing app() factory as factory-error, not import-error', async () => {
    const { findApplication } = await import('../../bin/_find-application');
    writeFileSync(join(workloadDir, 'package.json'), JSON.stringify({ name: 'needs-secrets' }));
    mkdirSync(join(workloadDir, 'src'));
    writeFileSync(
      join(workloadDir, 'src', 'main.ts'),
      `
      function requireEnv(name: string): string {
        const value = process.env[name];
        if (!value) throw new Error(\`Missing required environment variable: \${name}\`);
        return value;
      }
      export const app = () => ({ clientId: requireEnv('OAUTH_CLIENT_ID_FIXTURE') });
      `,
    );

    const failures: EntryPointFailure[] = [];
    const diagnostics: string[] = [];
    expect(await findApplication(workloadDir, false, diagnostics, failures)).toBeNull();
    expect(failures.map((f) => f.kind)).toEqual(['factory-error']);
    expect(failures[0].detail).toContain('OAUTH_CLIENT_ID_FIXTURE');
    // The diagnostics string shape is unchanged by the split: config-extract
    // renders it into its own hard error and its wording is pinned there.
    expect(diagnostics).toEqual(['src/main.ts: Missing required environment variable: OAUTH_CLIENT_ID_FIXTURE']);
  });
});

describe('resolveConfigSchemaOutput', () => {
  it('uses the default committed schema path when project config is absent', () => {
    expect(resolveConfigSchemaOutput()).toBeUndefined();
  });

  it('maps project schema false to the gitignored fallback path sentinel', () => {
    expect(resolveConfigSchemaOutput({ schema: false })).toBe(false);
  });

  it('maps project schema true to the committed default', () => {
    expect(resolveConfigSchemaOutput({ schema: true })).toBeUndefined();
  });
});
