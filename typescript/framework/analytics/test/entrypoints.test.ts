import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { existsSync, mkdirSync, mkdtempSync, rmSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';

const FEATURE = 'typescript/web-analytics-collection';
const BOUNDARY = 'the-browser-bundle-stays-inside-the-boundary';

/**
 * Server-only `@putnami/runtime` APIs the server half imports. None of them
 * has a browser counterpart, so a published browser entry that reaches one has
 * crossed the package's server/browser authority boundary.
 */
const SERVER_ONLY_RUNTIME_APIS = ['useContext', 'runInContext', 'useLogger'];

/**
 * The subset of the list above this package's server half actually imports.
 * Liveness is asserted on it: a probe that expected an API nobody imports
 * would fail for the wrong reason and hide a real leak of the other two.
 */
const SERVER_RUNTIME_APIS_IN_USE = ['useLogger'];

/**
 * Implementation symbols that only exist under `src/server/**`. Any of them
 * inside the browser graph means server code was linked into a chunk a visitor
 * downloads — and with it the identity secret, the SQL, and the sanitizer.
 */
const SERVER_IMPLEMENTATION_SYMBOLS = [
  'AnalyticsPlugin',
  'createMigrationSource',
  'visitorHash',
  'sanitizeBatch',
  'registerIngestRoute',
];

/** Every module specifier a bundled ESM file imports from. */
const importSpecifiers = (source: string): string[] => {
  const specifiers: string[] = [];
  for (const match of source.matchAll(/(?:from|import)\s*["']([^"']+)["']/g)) {
    specifiers.push(match[1] as string);
  }
  return specifiers;
};

/**
 * Walks the emitted file and every relative file it imports, so the assertions
 * cover the whole chunk closure a consumer would link — not just the entry.
 */
const collectGraphClosure = async (entry: string): Promise<Map<string, string>> => {
  const closure = new Map<string, string>();
  const queue = [entry];
  while (queue.length > 0) {
    const file = queue.pop() as string;
    if (closure.has(file) || !existsSync(file)) continue;
    const source = await Bun.file(file).text();
    closure.set(file, source);
    for (const specifier of importSpecifiers(source)) {
      if (specifier.startsWith('.')) queue.push(resolve(dirname(file), specifier));
    }
  }
  return closure;
};

/** Named bindings the closure imports from `@putnami/runtime`, aliases resolved. */
const runtimeImportBindings = (source: string): string[] => {
  const bindings: string[] = [];
  for (const match of source.matchAll(/import\s*\{([^}]*)\}\s*from\s*["']@putnami\/runtime["']/g)) {
    for (const clause of (match[1] as string).split(',')) {
      const name = clause
        .trim()
        .split(/\s+as\s+/)[0]
        ?.trim();
      if (name) bindings.push(name);
    }
  }
  return bindings;
};

describe('package entrypoints', () => {
  specTest(
    'keeps the server half out of the published browser build graph',
    { feature: FEATURE, requirement: BOUNDARY, check: 'no-server-only-runtime-import-reaches-the-browser-graph' },
    async () => {
      const packageRoot = join(import.meta.dir, '..');
      // Keep hard-kill leftovers ignored and typecheck-excluded under
      // node_modules, while retaining resolution of workspace dependencies.
      const testTmpRoot = join(packageRoot, 'node_modules', '.putnami-test');
      mkdirSync(testTmpRoot, { recursive: true });
      const publishedRoot = mkdtempSync(join(testTmpRoot, 'browser-consumer-'));

      try {
        // Mirror publication: the TypeScript extension partitions entrypoints
        // by export condition and runs one `bun build` per graph. Both graphs
        // share --root/--outdir so the emitted paths match the published
        // `exports` map.
        const graphOptions = {
          format: 'esm',
          splitting: true,
          packages: 'external',
          root: packageRoot,
          outdir: publishedRoot,
        } as const;
        const serverGraph = await Bun.build({
          ...graphOptions,
          entrypoints: [join(packageRoot, 'src', 'index.ts')],
          target: 'bun',
        });
        if (!serverGraph.success) {
          throw new Error(serverGraph.logs.map((log) => log.message).join('\n'));
        }
        const browserGraph = await Bun.build({
          ...graphOptions,
          entrypoints: [join(packageRoot, 'src', 'index.browser.ts'), join(packageRoot, 'src', 'client', 'entry.ts')],
          target: 'browser',
        });
        if (!browserGraph.success) {
          throw new Error(browserGraph.logs.map((log) => log.message).join('\n'));
        }

        const publishedServerEntry = join(publishedRoot, 'src', 'index.js');
        const publishedBrowserEntry = join(publishedRoot, 'src', 'index.browser.js');
        const publishedTrackerEntry = join(publishedRoot, 'src', 'client', 'entry.js');
        expect(existsSync(publishedServerEntry)).toBe(true);
        expect(existsSync(publishedBrowserEntry)).toBe(true);
        expect(existsSync(publishedTrackerEntry)).toBe(true);

        // Two invocations into one --outdir may only agree on a filename when
        // the bytes are identical (chunk names are content-hashed); otherwise
        // the later graph would silently overwrite the earlier one's chunk.
        const browserOutputs = new Map(browserGraph.outputs.map((output) => [output.path, output]));
        for (const serverOutput of serverGraph.outputs) {
          const collided = browserOutputs.get(serverOutput.path);
          if (!collided) continue;
          expect(await collided.text()).toBe(await serverOutput.text());
        }

        const browserSources = [
          ...(await collectGraphClosure(publishedBrowserEntry)).values(),
          ...(await collectGraphClosure(publishedTrackerEntry)).values(),
        ].join('\n');
        const serverSources = [...(await collectGraphClosure(publishedServerEntry)).values()].join('\n');

        // Liveness: the probe only means something if the same symbols really
        // do live in the server graph. If this fails, everything below is
        // vacuous — an empty bundle satisfies every negative assertion.
        for (const symbol of SERVER_IMPLEMENTATION_SYMBOLS) {
          expect(serverSources).toContain(symbol);
        }
        expect(runtimeImportBindings(serverSources)).toEqual(expect.arrayContaining(SERVER_RUNTIME_APIS_IN_USE));

        // Containment: no server implementation is reachable from the browser
        // entry, directly or through any chunk it imports.
        for (const symbol of SERVER_IMPLEMENTATION_SYMBOLS) {
          expect(browserSources).not.toContain(symbol);
        }
        expect(browserSources).not.toContain('src/server/');
        // Bundlers give no browser implementation for a server-only runtime
        // API, so reaching one is a build that breaks in the visitor's tab.
        for (const binding of runtimeImportBindings(browserSources)) {
          expect(SERVER_ONLY_RUNTIME_APIS).not.toContain(binding);
        }
        // No `node:` module either: the tracker runs where there is no Node.
        for (const specifier of importSpecifiers(browserSources)) {
          expect(specifier.startsWith('node:')).toBe(false);
        }
        // And the tracker really is linked, not tree-shaken into nothing.
        expect(browserSources).toContain('putnami.analytics.queue');
        expect(browserSources).toContain('putnami.analytics.session');
      } finally {
        rmSync(publishedRoot, { recursive: true, force: true });
      }
    },
    30_000,
  );
});

describe('cross-package event name', () => {
  // The tracker repeats the event name rather than importing the constant,
  // because importing the value would put a runtime edge from a 4 KiB bundle
  // into the whole @putnami/web browser graph. That copy is only safe while
  // something holds it to the original: without this test, renaming the event
  // on the dispatching side leaves every test here green and silently ends
  // single-page navigation tracking — no error, no warning, no recorded view.
  //
  // A test is not bundled, so importing the constant here costs the budget
  // nothing.
  it('listens on exactly the name @putnami/web dispatches', async () => {
    const { PUTNAMI_NAVIGATION_EVENT } = await import('@putnami/web');
    const { NAVIGATION_EVENT } = await import('../src/client/tracker');

    expect(NAVIGATION_EVENT).toBe(PUTNAMI_NAVIGATION_EVENT);
  });
});
