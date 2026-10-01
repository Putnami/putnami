import { afterAll, beforeAll, describe, expect, mock } from 'bun:test';
import { mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { relative, resolve } from 'node:path';
import { CSP_NONCE_CONTEXT_KEY, type HttpPlugin } from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import { runInContext } from '../../../runtime/src/context/context.utils';
import { ReactClientGenerator } from '../../src/ssr/generator/react-client.generator';
import { ReactApplication } from '../../src/ssr/react-application';
import type { HydrationReport } from '../fixtures/hydration-app/browser';

// End to end over the two halves of a page: this process renders the fixture
// app through the page renderer, and a second process hydrates that HTML in a
// DOM with the client entry the generator emits for the same files. React
// hydrates the server HTML in place only when both halves build the same tree.

const packageRoot = resolve(import.meta.dir, '..', '..');
const workspaceRoot = resolve(packageRoot, '..', '..', '..');
const fixtureDir = resolve(import.meta.dir, '..', 'fixtures', 'hydration-app');
const appDir = resolve(fixtureDir, 'app');
// One directory per test process, ignored by the repository's `.gen.*` rule and
// removed after the run.
const genDir = resolve(fixtureDir, `.gen.${process.pid}`);
// Above the time the browser process gives hydration.
const BROWSER_TIMEOUT_MS = 20_000;
const clientEntry = resolve(genDir, '.react-client.gen.tsx');

/** One page of the fixture app: the URL to load and the HTTP route that serves it. */
interface Scenario {
  name: string;
  path: string;
  httpRoute: string;
  text: string;
  status: number;
}

const scenarios: Scenario[] = [
  { name: 'a plain component', path: '/', httpRoute: '/', text: 'Home', status: 200 },
  { name: 'a page definition', path: '/about', httpRoute: '/about', text: 'About', status: 200 },
  { name: 'a page React streams after the shell', path: '/long', httpRoute: '/long', text: 'Long', status: 200 },
  { name: 'the not-found page', path: '/missing', httpRoute: '/*', text: 'Not found', status: 404 },
];

/** The document the server sends for a page, and the nonce its policy allows. */
interface ServerPage {
  html: string;
  nonce: string;
}

const countMarkers = (html: string) => html.match(/<!--\$[?!~]?-->/g)?.length ?? 0;

/** Render a page on the server, the way the SSR generator registers the fixture app. */
async function renderOnServer({ path, httpRoute, status }: Scenario): Promise<ServerPage> {
  const httpPlugin = { get: mock(), post: mock() };
  const app = new ReactApplication(undefined, httpPlugin as unknown as HttpPlugin);
  // The SSR generator imports layouts and not-found pages eagerly, pages lazily.
  app
    .reactLayout('/', { layout: await import(resolve(appDir, 'layout.tsx')) })
    .reactPage('/', { page: () => import(resolve(appDir, 'page.tsx')) })
    .reactPage('/about', { page: () => import(resolve(appDir, 'about/page.tsx')) })
    .reactPage('/long', { page: () => import(resolve(appDir, 'long/page.tsx')) })
    .reactNotFound('/', { notFound: await import(resolve(appDir, 'not-found.tsx')) });

  const pageCall = httpPlugin.get.mock.calls.find((call: unknown[]) => call[0] === httpRoute);
  if (!pageCall) {
    throw new Error(`no page route registered for ${httpRoute}`);
  }
  const ctx = {
    req: new Request(`http://localhost${path}`),
    method: 'GET',
    headers: new Headers(),
    params: {},
    queryParams: () => ({}),
    body: async () => undefined,
    path: () => path,
    statusCode: pageCall[2]?.statusCode ?? 0,
  };
  const response = await runInContext(ctx as never, async () => pageCall[1](ctx));
  expect(response.status).toBe(status);
  const html = await response.get().text();
  // The page renderer publishes the request nonce on the context; the security
  // headers middleware reads it there to build the policy.
  const nonce = (ctx as Record<string, unknown>)[CSP_NONCE_CONTEXT_KEY] as string;
  expect(nonce.length).toBeGreaterThan(0);
  return { html, nonce };
}

/** Write the client entry the generator emits for the fixture app. */
function generateClientEntry(): void {
  const options = { scannedDir: appDir, relativeGenDir: relative(genDir, appDir) };
  const generator = new ReactClientGenerator(clientEntry, options.relativeGenDir, appDir);
  generator.addLayout('layout.tsx', options);
  generator.addPage('page.tsx', options);
  generator.addPage('about/page.tsx', options);
  generator.addPage('long/page.tsx', options);
  generator.addNotFound('not-found.tsx', options);
  generator.write();
}

/** Hydrate the server HTML of a page in a browser process and return its report. */
async function hydrate(scenario: Scenario): Promise<HydrationReport> {
  const htmlFile = resolve(genDir, `${scenario.text.replaceAll(' ', '-')}.html`);
  const { html, nonce } = await renderOnServer(scenario);
  writeFileSync(htmlFile, html);

  // `--conditions=browser` resolves `@putnami/web` to its browser entry, as the
  // client build does.
  const child = Bun.spawnSync({
    cmd: [
      process.execPath,
      '--conditions=browser',
      resolve(fixtureDir, 'browser.ts'),
      `http://localhost${scenario.path}`,
      htmlFile,
      clientEntry,
      nonce,
    ],
    cwd: packageRoot,
    stdout: 'pipe',
    stderr: 'pipe',
    timeout: BROWSER_TIMEOUT_MS,
  });
  if (child.exitCode !== 0) {
    throw new Error(`the browser process failed:\n${child.stderr.toString()}`);
  }
  return JSON.parse(child.stdout.toString()) as HydrationReport;
}

describe('server page hydration', () => {
  beforeAll(() => {
    process.env['PUTNAMI_WORKSPACE_ROOT'] = workspaceRoot;
    process.env['PUTNAMI_PROJECT_ROOT'] = packageRoot;
    process.env['PWD'] = packageRoot;
    mkdirSync(genDir, { recursive: true });
    generateClientEntry();
  });

  afterAll(() => {
    rmSync(genDir, { recursive: true, force: true });
  });

  specTest(
    'hydrates the server HTML of every page shape in place, with no recoverable error',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'hydration-containment',
      check: 'a-server-rendered-page-hydrates-without-a-recoverable-error',
    },
    async () => {
      for (const scenario of scenarios) {
        const report = await hydrate(scenario);

        expect(report.serverHtml).toContain(`>${scenario.text}</section>`);
        // `hydratePage` reports every error React hands it, `onRecoverableError`
        // included, through console.error; React logs a mismatch there too.
        expect({ page: scenario.name, errors: report.errors }).toEqual({ page: scenario.name, errors: [] });
        // React kept the server's element instead of rendering a new one, and
        // removed no boundary it could not match.
        expect({
          page: scenario.name,
          hydrated: report.hydrated,
          pageElementKept: report.pageElementKept,
          suspenseMarkers: report.suspenseMarkers.hydrated,
        }).toEqual({
          page: scenario.name,
          hydrated: true,
          pageElementKept: true,
          suspenseMarkers: report.suspenseMarkers.server,
        });
      }
    },
    // Each page starts one browser process.
    60_000,
  );

  specTest(
    'renders a page inside one Suspense boundary on the server',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'hydration-containment',
      check: 'the-server-renders-a-page-inside-one-suspense-boundary',
    },
    async () => {
      const markers = async (scenario: Scenario) => countMarkers((await renderOnServer(scenario)).html);

      // A page carries the page boundary; the not-found page is a plain element
      // on both sides.
      expect(await Promise.all(scenarios.map(markers))).toEqual([1, 1, 1, 0]);
    },
  );
});
