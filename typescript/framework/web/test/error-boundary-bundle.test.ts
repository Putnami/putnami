import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { restoreEnv } from '@putnami/utils';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';

/**
 * Bundle-level regression guard for the error boundaries.
 *
 * Source-level tests cannot see this failure mode. `bun build` substitutes the
 * literal member expression `process.env.NODE_ENV` with its build-time value on
 * every target, so a boundary written as
 * `const isDev = process.env.NODE_ENV !== 'production'` ships as
 * `const isDev = true` in the published tarball. The guard is then frozen open
 * and the consumer's own `NODE_ENV` can never close it — the boundary renders
 * `error.message` and `<pre>{error.stack}</pre>` in production. Tests that
 * import the TypeScript source never bundle, so they always pass.
 *
 * These tests therefore build the boundaries the way publication does, then
 * change `NODE_ENV` **after** the build and render the built module.
 */

/** Distinct strings so a leak is unambiguous in the assertion output. */
const SENTINEL_MESSAGE = 'sentinel-boundary-message';
const SENTINEL_STACK = 'sentinel-boundary-stack';

const packageRoot = join(import.meta.dir, '..');

/**
 * Key of the route error handed to the built boundary. It travels through a
 * global because the built module links a stubbed `react-router`, not the
 * test's own module instance.
 */
const ROUTE_ERROR_KEY = '__putnamiBoundaryRouteError';

/** The global bag both this file and the emitted stub read the error from. */
const globalBag = globalThis as unknown as Record<string, unknown>;

interface BuiltBoundaries {
  /** Emitted entry source, for assertions about the artifact itself. */
  source: string;
  /** The boundary component, loaded from the emitted file. */
  Boundary: () => React.ReactElement;
  cleanup: () => void;
}

/**
 * Builds one boundary entry the way `putnami build` publishes it, and loads the
 * emitted module.
 *
 * `react-router` is replaced by a stub: `useRouteError` refuses to run outside
 * a data router, and the stub keeps the probe to the guard under test.
 * `react`, `react-dom` and `@putnami/runtime` stay external exactly as they do
 * in publication, so `@putnami/runtime` is resolved from `node_modules` at
 * import time — which is why the output directory lives under the package's
 * own `node_modules`.
 */
const buildBoundary = async (
  entry: string,
  target: 'bun' | 'browser',
  exportName: string,
): Promise<BuiltBoundaries> => {
  const stubRoot = mkdtempSync(join(packageRoot, '.boundary-bundle-'));
  const testRoot = join(packageRoot, 'node_modules', '.putnami-test');
  mkdirSync(testRoot, { recursive: true });
  const outputRoot = mkdtempSync(join(testRoot, 'boundary-bundle-'));
  const cleanup = () => {
    rmSync(stubRoot, { recursive: true, force: true });
    rmSync(outputRoot, { recursive: true, force: true });
  };

  try {
    const stub = join(stubRoot, 'react-router.stub.ts');
    writeFileSync(
      stub,
      `const bag = globalThis as unknown as Record<string, unknown>;
export function useRouteError(): unknown {
  return bag[${JSON.stringify(ROUTE_ERROR_KEY)}];
}
export function isRouteErrorResponse(error: unknown): boolean {
  return Boolean((error as { status?: number } | undefined)?.status);
}
`,
    );

    const built = await Bun.build({
      entrypoints: [entry],
      target,
      format: 'esm',
      outdir: outputRoot,
      external: ['react', 'react-dom', 'react/jsx-runtime', 'react/jsx-dev-runtime', '@putnami/runtime'],
      plugins: [
        {
          name: 'stub-react-router',
          setup(build) {
            build.onResolve({ filter: /^react-router$/ }, () => ({ path: stub }));
          },
        },
      ],
    });
    if (!built.success) {
      throw new Error(built.logs.map((log) => log.message).join('\n'));
    }

    const entryOutput = built.outputs.find((output) => output.kind === 'entry-point');
    if (!entryOutput) throw new Error(`no entry-point emitted for ${entry}`);
    const module = (await import(entryOutput.path)) as Record<string, () => React.ReactElement>;
    const Boundary = module[exportName];
    if (!Boundary) throw new Error(`${exportName} is not exported by the built ${entry}`);

    return { source: await entryOutput.text(), Boundary, cleanup };
  } catch (error) {
    cleanup();
    throw error;
  }
};

/** Renders a built boundary with `NODE_ENV` set only after the build. */
const renderAt = (Boundary: () => React.ReactElement, nodeEnv: string | undefined): string => {
  const previousNodeEnv = process.env.NODE_ENV;
  const previousService = process.env.K_SERVICE;
  globalBag[ROUTE_ERROR_KEY] = Object.assign(new Error(SENTINEL_MESSAGE), { stack: SENTINEL_STACK });
  restoreEnv('NODE_ENV', nodeEnv);
  // Cloud Run marks production too; leaving it set would make the dev
  // assertions below fail for a reason unrelated to the guard under test.
  delete process.env.K_SERVICE;
  try {
    return renderToStaticMarkup(React.createElement(Boundary));
  } finally {
    restoreEnv('NODE_ENV', previousNodeEnv);
    restoreEnv('K_SERVICE', previousService);
    delete globalBag[ROUTE_ERROR_KEY];
  }
};

/**
 * Renders a built boundary as a *browser* would run it.
 *
 * Rendering under Bun with a real `process` is not a browser: it lets an
 * environment read answer, which is exactly the signal a browser does not have.
 * This installs a `window` (the only thing the client guard consults there) and
 * a `process.env` shim that looks like a developer machine — `NODE_ENV` absent,
 * the shape a bundler-injected `process.env = {}` produces. A boundary that
 * still trusts the environment in the browser exposes here.
 */
const renderInBrowser = (Boundary: () => React.ReactElement, exposeFlag: boolean | undefined): string => {
  const previousWindow = Object.getOwnPropertyDescriptor(globalThis, 'window');
  const previousNodeEnv = process.env.NODE_ENV;
  const previousService = process.env.K_SERVICE;
  globalBag[ROUTE_ERROR_KEY] = Object.assign(new Error(SENTINEL_MESSAGE), { stack: SENTINEL_STACK });
  // The hostile shape: an environment that reads as development.
  delete process.env.NODE_ENV;
  delete process.env.K_SERVICE;
  Object.defineProperty(globalThis, 'window', {
    value: exposeFlag === undefined ? {} : { __putnamiExposeErrors: exposeFlag },
    configurable: true,
    writable: true,
  });
  try {
    return renderToStaticMarkup(React.createElement(Boundary));
  } finally {
    if (previousWindow) Object.defineProperty(globalThis, 'window', previousWindow);
    else (globalThis as { window?: unknown }).window = undefined;
    restoreEnv('NODE_ENV', previousNodeEnv);
    restoreEnv('K_SERVICE', previousService);
    delete globalBag[ROUTE_ERROR_KEY];
  }
};

const boundaries = [
  {
    name: 'client ErrorBoundary',
    entry: join(packageRoot, 'src', 'client', 'error', 'error-boundary.tsx'),
    target: 'browser' as const,
    exportName: 'ErrorBoundary',
    // The published browser graph must not reach the runtime barrel: it
    // re-exports the filesystem-backed config loader. The client guard is
    // therefore self-contained, and `test/client/expose-errors.test.ts` pins it
    // to the same answers as `shouldExposeErrorStack()`.
    reachesRuntimeBarrel: false,
  },
  {
    name: 'SSR DefaultErrorBoundary',
    entry: join(packageRoot, 'src', 'ssr', 'error-boundaries.ts'),
    target: 'bun' as const,
    exportName: 'DefaultErrorBoundary',
    reachesRuntimeBarrel: true,
  },
];

describe('published error boundaries', () => {
  for (const boundary of boundaries) {
    describe(boundary.name, () => {
      it('decides at render time, not at bundle time', async () => {
        const { source, Boundary, cleanup } = await buildBoundary(boundary.entry, boundary.target, boundary.exportName);
        try {
          // Production: no message, no stack. This is the assertion the
          // constant-folded artifact fails.
          const productionHtml = renderAt(Boundary, 'production');
          expect(productionHtml).toContain('An unexpected error occurred');
          expect(productionHtml).not.toContain(SENTINEL_MESSAGE);
          expect(productionHtml).not.toContain(SENTINEL_STACK);
          expect(productionHtml).not.toContain('<pre>');

          // Liveness: without this, a build that hid the details unconditionally
          // (or a runner that already exports NODE_ENV=production) would satisfy
          // the assertions above for the wrong reason.
          const developmentHtml = renderAt(Boundary, 'development');
          expect(developmentHtml).toContain(SENTINEL_MESSAGE);
          expect(developmentHtml).toContain(SENTINEL_STACK);

          // And the artifact itself still decides at run time. A boundary that
          // inlined `process.env.NODE_ENV` emits a bare `const isDev = true`,
          // with both the call and the variable name gone.
          if (boundary.reachesRuntimeBarrel) {
            // Server side: delegates to the shared guard, kept external here
            // exactly as publication keeps it.
            expect(source).toContain('shouldExposeErrorStack');
            expect(source).toContain('@putnami/runtime');
          } else {
            // Client side: self-contained, so the read is in the artifact —
            // and the runtime barrel, which carries the filesystem config
            // loader, must not be in the browser graph.
            expect(source).toContain('NODE_ENV');
            expect(source).toContain('__putnamiExposeErrors');
            expect(source).not.toContain('@putnami/runtime');
          }
        } finally {
          cleanup();
        }
      }, 60_000);
    });
  }
});

describe('published client boundary in a browser', () => {
  /**
   * The regression the first version of this fix still had. A browser has no
   * `NODE_ENV`, so a guard that reads the environment there answers
   * "development" and renders the message and stack on a production page. The
   * server has to say so explicitly.
   */
  const client = boundaries[0];
  let built: BuiltBoundaries;

  // One build for all three cases: the artifact is identical, only the injected
  // flag differs, and the bundler spawn is the expensive part.
  beforeAll(async () => {
    built = await buildBoundary(client.entry, client.target, client.exportName);
  }, 60_000);
  afterAll(() => built?.cleanup());

  it('hides diagnostics when the server injected no flag', () => {
    const html = renderInBrowser(built.Boundary, undefined);
    expect(html).toContain('An unexpected error occurred');
    expect(html).not.toContain(SENTINEL_MESSAGE);
    expect(html).not.toContain(SENTINEL_STACK);
    expect(html).not.toContain('<pre>');
  });

  it('hides diagnostics when the server injected false', () => {
    const html = renderInBrowser(built.Boundary, false);
    expect(html).not.toContain(SENTINEL_MESSAGE);
    expect(html).not.toContain(SENTINEL_STACK);
  });

  it('shows diagnostics when the server injected true', () => {
    // Liveness: without this the two assertions above would pass on a boundary
    // that hid everything unconditionally.
    const html = renderInBrowser(built.Boundary, true);
    expect(html).toContain(SENTINEL_MESSAGE);
    expect(html).toContain(SENTINEL_STACK);
  });
});
