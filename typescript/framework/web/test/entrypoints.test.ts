import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, relative, resolve } from 'node:path';

/**
 * Server-only `@putnami/runtime` APIs the SSR implementation imports. None of
 * them has a browser counterpart, so a published browser entry that reaches
 * one has crossed the package's server/browser authority boundary.
 */
const SERVER_ONLY_RUNTIME_APIS = ['useContext', 'runInContext', 'useLogger'];

/**
 * Implementation symbols that only exist under `src/ssr/**` (or are only ever
 * constructed by it). Any of them inside the browser graph means SSR code was
 * linked into a browser-reachable chunk.
 */
const SSR_IMPLEMENTATION_SYMBOLS = [
  'SsrDocumentHelper',
  'ReactApplication',
  'ReactPlugin',
  'renderStaticDocument',
  'inlineDeferredBoundaries',
  'revalidateTag',
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
 * Walks the emitted file and every relative file it imports, so assertions
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
    're-exports browser/client modules',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'entrypoint-boundary',
      check: 'the-browser-entry-re-exports-the-client-modules',
    },
    async () => {
      const client = await import('../src/client/index.ts');
      const browser = await import('../src/index.browser');
      const document = await import('../src/client/document/index.ts');
      const form = await import('../src/client/form/index.ts');
      const router = await import('../src/client/router/index.ts');

      expect(browser.page).toBe(client.page);
      expect(document.PageMeta).toBeDefined();
      expect(form.Form).toBeDefined();
      expect(router.hydratePage).toBeDefined();
    },
  );

  specTest(
    're-exports SSR modules from the top-level package entrypoint',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'entrypoint-boundary',
      check: 'the-package-entry-re-exports-the-ssr-modules',
    },
    async () => {
      const ssr = await import('../src/ssr');
      const nodeEntry = await import('../src/index');

      expect(nodeEntry.ReactApplication).toBe(ssr.ReactApplication);
      expect(nodeEntry.react).toBe(ssr.react);
      expect(nodeEntry.ReactPlugin).toBe(ssr.ReactPlugin);
      expect(typeof ssr.cache).toBe('function');
      expect(typeof ssr.page).toBe('function');
    },
  );

  specTest(
    'builds the documented error boundary under both export conditions',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'entrypoint-boundary',
      check: 'the-documented-error-boundary-hooks-build-under-both-export-conditions',
    },
    async () => {
      const packageRoot = join(import.meta.dir, '..');
      const browser = await import('../src/index.browser');
      const server = await import('../src/index');

      // Both entries publish the hooks the error-boundary docs tell an
      // application to import, so no boundary has to depend on react-router.
      expect(typeof browser.useRouteError).toBe('function');
      expect(typeof browser.isRouteErrorResponse).toBe('function');
      expect(typeof server.useRouteError).toBe('function');
      expect(typeof server.isRouteErrorResponse).toBe('function');
      // A route can throw anything, so the hook claims nothing about the value
      // until the boundary narrows it.
      const untyped: () => unknown = browser.useRouteError;
      expect(typeof untyped).toBe('function');

      // The hooks resolved from `src/**` must also exist in the *bundled*
      // browser entry: a mismatch makes an application's generated browser
      // graph die with `No matching export ... for import "useRouteError"`.
      // Only bundling the documented example reproduces that; importing the
      // module does not.
      const testTmpRoot = join(packageRoot, 'node_modules', '.putnami-test');
      mkdirSync(testTmpRoot, { recursive: true });
      const exampleRoot = mkdtempSync(join(testTmpRoot, 'error-boundary-example-'));

      try {
        const graphs = [
          { entry: join(packageRoot, 'src', 'index.browser.ts'), target: 'browser' },
          { entry: join(packageRoot, 'src', 'index.ts'), target: 'bun' },
        ] as const;

        for (const graph of graphs) {
          const consumer = join(exampleRoot, `${graph.target}-error.tsx`);
          writeFileSync(
            consumer,
            `import { error, isRouteErrorResponse, useRouteError } from ${JSON.stringify(relative(exampleRoot, graph.entry))};

export default error().render(function ErrorPage() {
  const thrown = useRouteError();
  if (isRouteErrorResponse(thrown)) {
    return <div>{thrown.status}</div>;
  }
  return <div>{String(thrown)}</div>;
});
`,
          );

          // Only this package is bundled: its dependencies stay external so a
          // failure here is a missing export of `@putnami/web` and nothing else.
          const built = await Bun.build({
            entrypoints: [consumer],
            target: graph.target,
            format: 'esm',
            packages: 'external',
            ...(graph.target === 'browser' ? { conditions: ['browser'] } : {}),
          });
          if (!built.success) {
            throw new Error(`${graph.target} graph: ${built.logs.map((log) => log.message).join('\n')}`);
          }
        }
      } finally {
        rmSync(exampleRoot, { recursive: true, force: true });
      }
    },
    30_000,
  );

  specTest(
    'does not pull the server-only SsrDocumentHelper into the browser bundle',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'entrypoint-boundary',
      check: 'the-server-only-document-helper-stays-out-of-the-browser-bundle',
    },
    async () => {
      // Tree-shaking guard: the browser build swaps document.helper.ts for
      // document.helper.browser.ts (package.json `browser` field), so the
      // server-only head-serialization code in document-ssr.helper.ts must not
      // be reachable from the browser entrypoint.
      const built = await Bun.build({
        entrypoints: [join(import.meta.dir, '..', 'src', 'index.browser.ts')],
        target: 'browser',
        format: 'esm',
        conditions: ['browser'],
        minify: false,
      });

      expect(built.success).toBe(true);

      let bundle = '';
      for (const output of built.outputs) {
        bundle += await output.text();
      }

      // SsrDocumentHelper class and its head-serialization internals must be absent.
      expect(bundle).not.toContain('class SsrDocumentHelper');
      expect(bundle).not.toContain('get headHtml');
      expect(bundle).not.toContain('</head>');
      // The browser DOM helper must still be present.
      expect(bundle).toContain('class BrowserDocumentHelper');
    },
  );

  specTest(
    'keeps SSR implementations out of the published browser build graph',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'entrypoint-boundary',
      check: 'no-ssr-implementation-reaches-the-published-browser-graph',
    },
    async () => {
      const packageRoot = join(import.meta.dir, '..');
      // Keep hard-kill leftovers ignored and typecheck-excluded under node_modules,
      // while retaining resolution of this package's workspace dependencies.
      const testTmpRoot = join(packageRoot, 'node_modules', '.putnami-test');
      mkdirSync(testTmpRoot, { recursive: true });
      const publishedRoot = mkdtempSync(join(testTmpRoot, 'browser-consumer-'));

      try {
        const packageJson = (await Bun.file(join(packageRoot, 'package.json')).json()) as {
          dependencies?: Record<string, string>;
        };
        const packageDependencies = Object.keys(packageJson.dependencies ?? {});
        expect(packageDependencies).toContain('@putnami/runtime');
        const consumerExternals = packageDependencies.filter((dependency) => dependency !== '@putnami/runtime');

        // Mirror publication: the TypeScript extension partitions entrypoints by
        // export condition and runs one `bun build` per graph.
        // The server graph carries the default entry and the bin executables;
        // the browser-condition entry is built separately with browser
        // resolution. Splitting can therefore only emit chunks *within* a graph,
        // and both graphs share --root/--outdir so the emitted paths match the
        // published `exports`.
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
          entrypoints: [join(packageRoot, 'src', 'index.browser.ts')],
          target: 'browser',
        });
        if (!browserGraph.success) {
          throw new Error(browserGraph.logs.map((log) => log.message).join('\n'));
        }

        const publishedServerEntry = join(publishedRoot, 'src', 'index.js');
        const publishedBrowserEntry = join(publishedRoot, 'src', 'index.browser.js');
        expect(existsSync(publishedServerEntry)).toBe(true);
        expect(existsSync(publishedBrowserEntry)).toBe(true);

        // Two invocations into one --outdir may only agree on a filename when the
        // bytes are identical (chunk names are content-hashed); otherwise the
        // later graph would silently overwrite the earlier one's chunk.
        const browserOutputs = new Map(browserGraph.outputs.map((output) => [output.path, output]));
        for (const serverOutput of serverGraph.outputs) {
          const collided = browserOutputs.get(serverOutput.path);
          if (!collided) continue;
          expect(await collided.text()).toBe(await serverOutput.text());
        }

        const browserClosure = await collectGraphClosure(publishedBrowserEntry);
        const browserSources = [...browserClosure.values()].join('\n');
        const serverClosure = await collectGraphClosure(publishedServerEntry);
        const serverSources = [...serverClosure.values()].join('\n');

        // Liveness: the probe only means something if the same symbols really do
        // live in the server graph. If this fails the assertions below are vacuous.
        for (const symbol of SSR_IMPLEMENTATION_SYMBOLS) {
          expect(serverSources).toContain(symbol);
        }
        expect(runtimeImportBindings(serverSources)).toEqual(expect.arrayContaining(SERVER_ONLY_RUNTIME_APIS));

        // Containment: no SSR implementation is reachable from the browser entry,
        // directly or through any chunk it imports.
        for (const symbol of SSR_IMPLEMENTATION_SYMBOLS) {
          expect(browserSources).not.toContain(symbol);
        }
        expect(browserSources).not.toContain('src/ssr/');
        // And no server-only runtime API is imported by that closure. Bundlers
        // give no browser implementation for these, so reaching one is the same
        // failure mode returning through a different module.
        for (const binding of runtimeImportBindings(browserSources)) {
          expect(SERVER_ONLY_RUNTIME_APIS).not.toContain(binding);
        }
        // The browser DOM helper must still be linked — an empty bundle would
        // satisfy every negative assertion above.
        expect(browserSources).toContain('BrowserDocumentHelper');

        const consumerEntrypoint = join(publishedRoot, 'consumer.ts');
        const publishedImport = `./${relative(publishedRoot, publishedBrowserEntry)}`;
        writeFileSync(consumerEntrypoint, `import ${JSON.stringify(publishedImport)};\n`);

        const consumed = await Bun.build({
          entrypoints: [consumerEntrypoint],
          target: 'browser',
          format: 'esm',
          conditions: ['browser'],
          // Keep @putnami/runtime bundled so Bun must resolve its browser
          // condition; externalizing it would make this regression test vacuous.
          external: consumerExternals,
        });
        if (!consumed.success) {
          throw new Error(consumed.logs.map((log) => log.message).join('\n'));
        }

        const bundle = (await Promise.all(consumed.outputs.map((output) => output.text()))).join('\n');
        expect(bundle).not.toContain('node:async_hooks');
        expect(bundle).not.toContain('AsyncLocalStorage');
      } finally {
        rmSync(publishedRoot, { recursive: true, force: true });
      }
    },
    30_000,
  );

  it('preserves ALS-backed document metadata in a standalone compiled executable', async () => {
    const sourceRoot = mkdtempSync(join(import.meta.dir, '.compiled-document-meta-'));
    const executableRoot = mkdtempSync(join(tmpdir(), 'putnami-web-document-meta-'));
    const packageEntrypoint = join(sourceRoot, 'package-entry.ts');
    const consumerEntrypoint = join(sourceRoot, 'consumer.ts');
    const packagedBundle = join(sourceRoot, 'packaged.js');
    const executable = join(executableRoot, 'app-bin');

    try {
      // Create every file of the probe up front, `packaged.js` included. The
      // first build below makes the bundler read this directory, and it keeps
      // that listing for the second build. A `packaged.js` created after the
      // first build is therefore missing from the listing the compile resolves
      // against, which is how CI failed here with `Could not resolve:
      // "./packaged.js"`. The placeholder bytes are overwritten before any
      // build reads the file.
      writeFileSync(packagedBundle, '');
      writeFileSync(
        packageEntrypoint,
        `export { Favicon, HeaderLink } from '../../src/client/document/favicon';
export { Lang } from '../../src/client/document/lang';
export { Script } from '../../src/client/document/script';
export { Style } from '../../src/client/document/style';
export { SsrDocumentHelper } from '../../src/client/document/document-ssr.helper';
`,
      );
      writeFileSync(
        consumerEntrypoint,
        `import { runInContext } from '@putnami/runtime';
import { Favicon, HeaderLink, Lang, Script, SsrDocumentHelper, Style } from './packaged.js';

const result = runInContext({} as never, () => {
  Favicon({ href: '/favicon.svg' });
  HeaderLink({ rel: 'preload', href: '/font.woff2', as: 'font' });
  Style({ children: 'body { color: red; }' });
  Script({ src: '/app.js', type: 'module' });
  Lang({ lang: 'fr' });

  const helper = new SsrDocumentHelper();
  return { lang: helper.lang, headHtml: helper.headHtml };
});

console.log(JSON.stringify(result));
`,
      );

      // Mirror package publication before compiling the consuming server.
      // In that first build, package dependencies remain external and the
      // old CommonJS require was emitted as a runtime import.meta.require.
      const packaged = await Bun.build({
        entrypoints: [packageEntrypoint],
        target: 'bun',
        format: 'esm',
        packages: 'external',
      });
      if (!packaged.success) {
        throw new Error(packaged.logs.map((log) => log.message).join('\n'));
      }
      const packagedEntrypoint = packaged.outputs.find((output) => output.kind === 'entry-point');
      if (!packagedEntrypoint) throw new Error('packaged document metadata probe was not emitted');
      // Write the bundle ourselves rather than letting `outdir` place it, so
      // the compile below reads the exact bytes this build produced.
      writeFileSync(packagedBundle, await packagedEntrypoint.text());
      // Separates a missing file from a stale resolver view if this regresses.
      expect(existsSync(packagedBundle)).toBe(true);

      const built = await Bun.build({
        entrypoints: [consumerEntrypoint],
        target: 'bun',
        compile: { outfile: executable },
      });
      if (!built.success) {
        throw new Error(built.logs.map((log) => log.message).join('\n'));
      }

      // The executable must not rely on source files or an adjacent
      // node_modules tree when resolving the request context at runtime.
      rmSync(sourceRoot, { recursive: true, force: true });
      expect(existsSync(join(executableRoot, 'node_modules'))).toBe(false);

      const process = Bun.spawnSync({
        cmd: [executable],
        cwd: executableRoot,
        stdout: 'pipe',
        stderr: 'pipe',
      });
      const stderr = process.stderr.toString();
      expect(process.exitCode, stderr).toBe(0);

      const result = JSON.parse(process.stdout.toString()) as { lang: string; headHtml: string };
      expect(result.lang).toBe('fr');
      expect(result.headHtml).toContain('rel="icon"');
      expect(result.headHtml).toContain('href="/favicon.svg"');
      expect(result.headHtml).toContain('rel="preload"');
      expect(result.headHtml).toContain('href="/font.woff2"');
      expect(result.headHtml).toContain('body { color: red; }');
      expect(result.headHtml).toContain('src="/app.js"');
    } finally {
      rmSync(sourceRoot, { recursive: true, force: true });
      rmSync(executableRoot, { recursive: true, force: true });
    }
  }, 20_000);
});
