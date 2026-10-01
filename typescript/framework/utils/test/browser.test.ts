import { describe, expect, it } from 'bun:test';
import { readdir, readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { specTest } from '@putnami/spectest';

const SHARED_ROOT = join(import.meta.dir, '..', 'src', 'shared');

/** Every `.ts` file under `src/shared`, recursively. */
async function sharedSources(directory = SHARED_ROOT): Promise<string[]> {
  const entries = await readdir(directory, { withFileTypes: true });
  const files: string[] = [];
  for (const entry of entries) {
    const path = join(directory, entry.name);
    if (entry.isDirectory()) {
      files.push(...(await sharedSources(path)));
    } else if (entry.name.endsWith('.ts')) {
      files.push(path);
    }
  }
  return files;
}

describe('browser entrypoints', () => {
  it('exports browser-safe shared utilities from client index', async () => {
    const clientModule = await import('../src/client/index');

    expect(typeof clientModule.ensureArray).toBe('function');
    expect(typeof clientModule.mergeDeep).toBe('function');
    expect(typeof clientModule.validateSchema).toBe('function');
  });

  it('re-exports the client index from index.browser', async () => {
    const browserModule = await import('../src/index.browser');

    expect(typeof browserModule.ensureArray).toBe('function');
    expect(typeof browserModule.validateSchema).toBe('function');
  });

  specTest(
    'exposes the whole schema vocabulary to the browser build',
    {
      feature: 'typescript/declarative-schema',
      requirement: 'browser-safe-barrel',
      check: 'the-schema-vocabulary-is-browser-exported',
    },
    async () => {
      // Config blocks and endpoint payloads are declared with the same helpers.
      // A browser bundle that can read a declaration but not the combinators it
      // was built from cannot share a single declaration with the server.
      const browserModule = (await import('../src/index.browser')) as unknown as Record<string, unknown>;

      for (const symbol of ['Optional', 'ArrayOf', 'MapOf', 'Int', 'Default', 'Env', 'Sensitive', 'Desc', 'schema']) {
        expect(browserModule[symbol]).toBeDefined();
      }
      expect(browserModule['compileSchemaValidator']).toBeFunction();
    },
  );

  specTest(
    'keeps server-only utilities out of the browser barrel',
    {
      feature: 'typescript/declarative-schema',
      requirement: 'browser-safe-barrel',
      check: 'no-server-only-utility-in-the-browser-barrel',
    },
    async () => {
      // These are the Node-backed families: filesystem, OS, path, workspace,
      // package.json, and the code generator. Any one of them reaching the
      // browser barrel drags a `node:` import into a consumer's bundle.
      const browserModule = (await import('../src/index.browser')) as unknown as Record<string, unknown>;

      for (const symbol of [
        'readFileContent',
        'writeFileContent',
        'listDir',
        'getPlatform',
        'getHomeDir',
        'joinPath',
        'resolvePath',
        'getWorkspaceRoot',
        'getCurrentProject',
        'readPackageJson',
        'GeneratorHelper',
        'createLogger',
      ]) {
        expect(browserModule[symbol]).toBeUndefined();
      }
    },
  );

  specTest(
    'keeps the JSONL hook protocol off both the root and browser barrels',
    {
      feature: 'typescript/declarative-schema',
      requirement: 'hooks-are-opt-in',
      check: 'hooks-live-only-on-the-dedicated-subpath',
    },
    async () => {
      // `@putnami/utils/hooks` is the build-time seam with the CLI extension
      // system, not a general utility: importing it must stay a decision.
      const rootModule = (await import('../src/index')) as unknown as Record<string, unknown>;
      const browserModule = (await import('../src/index.browser')) as unknown as Record<string, unknown>;
      const hooksModule = (await import('../src/server/hooks')) as unknown as Record<string, unknown>;

      for (const symbol of ['runHookCommand', 'standardHookModel', 'emitLog', 'emitArtifact', 'readHookContext']) {
        expect(rootModule[symbol]).toBeUndefined();
        expect(browserModule[symbol]).toBeUndefined();
        expect(hooksModule[symbol]).toBeDefined();
      }
    },
  );

  specTest(
    'imports no Node built-in from any shared module',
    {
      feature: 'typescript/declarative-schema',
      requirement: 'browser-safe-barrel',
      check: 'shared-modules-import-no-node-builtin',
    },
    async () => {
      // Tree shaking removes unused code, not unresolvable imports: one `node:`
      // specifier anywhere in the shared graph breaks every browser consumer.
      const offenders: string[] = [];
      for (const file of await sharedSources()) {
        const source = await readFile(file, 'utf8');
        if (/(?:from\s+|require\()\s*['"]node:/.test(source)) {
          offenders.push(file.slice(SHARED_ROOT.length + 1));
        }
      }

      expect(offenders).toEqual([]);
    },
  );
});
