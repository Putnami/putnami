/**
 * Canonical public-surface plugin.
 *
 * Contributes the `putnami.http-routes.v1` route facts that no framework
 * emitter can see, so `.gen/schema/http-routes.json` is a COMPLETE inventory of
 * what this workload serves and Putnami Cloud can build a generated default-deny
 * allowlist from it.
 *
 * What the framework already declares (never re-declare it here — the protocol
 * treats two facts with the same match language and intersecting methods as
 * `http_routes.duplicate_route` and fails the build):
 *
 * - `@putnami/application` typed API routes scanned from `src/api/`
 *   (`/llms.txt`, `/sitemap.xml`, `/dl/{artifact}`, `/doc-markdown`);
 * - `@putnami/application` staticFiles, from the files present in the SOURCE
 *   `public/` folder at generate time — `/favicon.ico`, `/robots.txt`,
 *   `/assets/`, `/fonts/`, plus `/schemas/`, whose dev mirror is written
 *   synchronously by the schemas plugin before the staticFiles plugin scans.
 *   The search index is not among them: it is written in postGenerate(), after
 *   the scan, and only under `.gen/public/search/`, so its one file is declared
 *   below with the warmup routes;
 * - `@putnami/web` file-routes (`/`, `/docs`, and the enumerated expansion of
 *   the `/docs/[...page]` catch-all) and the hashed client bundle prefixes
 *   `/react/` and `/react-islands/`.
 *
 * What is left, and why only this plugin can state it:
 *
 * 1. `putnami.json` `generate.assets` copies. They are materialized straight
 *    into `.gen/public/` by the extension before any hook runs, so they never
 *    appear in the staticFiles plugin's source scan even though the staticFiles loader
 *    serves them: `/install.sh`, `/install-commands.txt`, `/install.ps1`,
 *    `/LICENSE.md` and the whole `/docs/` subtree (markdown sources plus the `*.md.rendered.json`
 *    prerender sidecars). Derived from `putnami.json` so a new asset entry
 *    cannot silently escape the inventory. `exact /install.sh` and
 *    `exact /install.ps1` also cover `/install.sh?run=<command>` and
 *    `/install.ps1?run=<command>`, which the installer-run middleware answers:
 *    route facts never carry a query.
 * 2. `.gen/public/static/`, the `@putnami/web` SSG/ISR prerender output. The SSR
 *    reads it from disk; it is only reachable over HTTP because it happens to
 *    live under the public folder, and every byte of it is a duplicate of a page
 *    already served at its canonical URL. Declared `publicEdge: false` so the
 *    edge denies it instead of exposing a second copy of every page.
 * 3. Routes registered during `warmup()` rather than `generate()`, which the
 *    HttpPlugin route emitter (it snapshots the route table at generate time)
 *    cannot observe: the content-overlay endpoints and the platform
 *    probes. The probes are `publicEdge: false` — they are operational surface,
 *    not site content.
 *
 * Deliberately NOT declared: any root `/` prefix or root catch-all. Unmatched
 * paths render a styled 404 page, and `/docs/[...page]` renders one for
 * unmatched docs paths, but that fallback is a rendering decision, not a public
 * route — encoding it as a wildcard would hand the edge an allow-everything
 * matcher. Bounded prefixes plus enumerated exact files keep unknown paths and
 * scanner probes on the reject side of the allowlist. See README.md
 * ("Public route surface").
 */
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import type { GenerateResult, Plugin } from '@putnami/application';
import type { GeneratedHttpRoute } from '@putnami/application/http-routes';
import { getProjectRoot, getWorkspaceRoot, joinPath } from '@putnami/utils';

const GENERATED_PUBLIC_DIR = '.gen/public';

/** The `.gen/public` subtree the web SSG writes and the SSR reads from disk. */
const SSG_OUTPUT_DIRECTORY = 'static';

interface AssetEntry {
  from: string;
  to: string;
}

export interface PublicSurfaceInput {
  projectRoot: string;
  workspaceRoot: string;
}

/**
 * Routes registered at warmup, which the generate-time emitters cannot see.
 *
 * `methods` mirror the handlers exactly: the platform middleware only answers
 * `GET`, and neither the framework emitters nor this list declare the
 * auto-derived `OPTIONS` response, so an `OPTIONS` probe stays a reject.
 */
const RUNTIME_ROUTES: readonly GeneratedHttpRoute[] = [
  {
    match: 'exact',
    path: '/search/index.json',
    methods: ['GET'],
    publicEdge: true,
    provenance: { sourceKind: 'manual', evidencePath: 'src/plugins/content-overlay.plugin.ts' },
  },
  {
    match: 'exact',
    path: '/api/content/refresh',
    methods: ['POST'],
    publicEdge: true,
    provenance: { sourceKind: 'manual', evidencePath: 'src/plugins/content-overlay.plugin.ts' },
  },
  // Operational endpoints: served by the platform plugin's prepended middleware, and
  // never part of the site's public content.
  {
    match: 'exact',
    path: '/_/livez',
    methods: ['GET'],
    publicEdge: false,
    provenance: { sourceKind: 'manual', evidencePath: 'src/main.ts' },
  },
  {
    match: 'exact',
    path: '/_/healthz',
    methods: ['GET'],
    publicEdge: false,
    provenance: { sourceKind: 'manual', evidencePath: 'src/main.ts' },
  },
  {
    match: 'exact',
    path: '/_/readyz',
    methods: ['GET'],
    publicEdge: false,
    provenance: { sourceKind: 'manual', evidencePath: 'src/main.ts' },
  },
  {
    match: 'exact',
    path: '/_/version',
    methods: ['GET'],
    publicEdge: false,
    provenance: { sourceKind: 'manual', evidencePath: 'src/main.ts' },
  },
];

function readGenerateAssets(projectRoot: string): AssetEntry[] {
  const config = JSON.parse(readFileSync(joinPath(projectRoot, 'putnami.json'), 'utf8')) as {
    options?: { generate?: { assets?: AssetEntry[] } };
  };
  return (config.options?.generate?.assets ?? []).filter(
    (entry) => typeof entry?.from === 'string' && typeof entry?.to === 'string',
  );
}

/**
 * Whether a `generate.assets` entry copies a directory. Resolved from the
 * committed source when it exists (exact), otherwise from the target shape — a
 * copy target with no extension is a directory.
 */
function copiesDirectory(workspaceRoot: string, entry: AssetEntry, target: string): boolean {
  const source = joinPath(workspaceRoot, entry.from.replace(/^\/+/, ''));
  if (existsSync(source)) return statSync(source).isDirectory();
  return !target.includes('.');
}

/**
 * Route facts for the `putnami.json` `generate.assets` entries that land under
 * `public/`. A directory copy collapses to the bounded prefix of its top-level
 * segment (so the four `public/docs/<section>` entries all fold into `/docs/`),
 * a file copy becomes one exact public file.
 */
export function buildGenerateAssetHttpRoutes(input: PublicSurfaceInput): GeneratedHttpRoute[] {
  const routes = new Map<string, GeneratedHttpRoute>();
  for (const entry of readGenerateAssets(input.projectRoot)) {
    const target = entry.to.replace(/^\/+/, '');
    if (!target.startsWith('public/')) continue;
    const relative = target.slice('public/'.length).replace(/\/+$/, '');
    if (relative === '') continue;

    const top = relative.split('/')[0] as string;
    const nested = relative !== top;
    const directory = nested || copiesDirectory(input.workspaceRoot, entry, relative);

    const route: GeneratedHttpRoute = directory
      ? {
          match: 'prefix',
          path: `/${top}/`,
          methods: ['GET', 'HEAD'],
          publicEdge: true,
          provenance: { sourceKind: 'static-mount', evidencePath: `${GENERATED_PUBLIC_DIR}/${top}` },
        }
      : {
          match: 'exact',
          path: `/${relative}`,
          methods: ['GET', 'HEAD'],
          publicEdge: true,
          provenance: { sourceKind: 'public-file', evidencePath: `${GENERATED_PUBLIC_DIR}/${relative}` },
        };
    routes.set(`${route.match}\0${route.path}`, route);
  }
  return [...routes.values()].sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
}

function containsFile(path: string): boolean {
  if (!existsSync(path)) return false;
  if (!statSync(path).isDirectory()) return true;
  return readdirSync(path).some((entry) => containsFile(joinPath(path, entry)));
}

/**
 * Fail loudly on the one state that would make this plugin and the staticFiles
 * plugin declare the same fact.
 *
 * These copies exist only under `.gen/public/`, so the staticFiles plugin — which
 * declares from the SOURCE `public/` folder — never claims them and this plugin
 * owns them. `public/docs/` is the exception: the content-bundle plugin mirrors
 * bundle mounts there for dev serve and synchronously prunes them at the start
 * of every generate, using the `.gen` sidecar as its record. Wiping `.gen`
 * without wiping `public/` leaves the mirror with nothing to prune it, the
 * staticFiles scan then finds it, and the aggregate dies on
 * `http_routes.duplicate_route` with no explanation. Say the actual fix here.
 */
export function assertNoSourceMirror(projectRoot: string, declared: readonly GeneratedHttpRoute[]): void {
  for (const route of declared) {
    const relative = route.path.replace(/^\/+|\/+$/g, '');
    const mirror = joinPath(projectRoot, 'public', relative);
    if (!containsFile(mirror)) continue;
    throw new Error(
      `public/${relative} holds a stale dev mirror of build output that lives in .gen/public/${relative}. ` +
        'The staticFiles plugin would declare it as well, and two emitters declaring the same route fact fail the ' +
        `HTTP route inventory with http_routes.duplicate_route. Remove it: rm -rf ${joinPath('public', relative)}`,
    );
  }
}

/** The complete set of facts this site owns, in deterministic order. */
export function buildPublicSurfaceHttpRoutes(input: PublicSurfaceInput): GeneratedHttpRoute[] {
  return [
    ...buildGenerateAssetHttpRoutes(input),
    {
      match: 'prefix',
      path: `/${SSG_OUTPUT_DIRECTORY}/`,
      methods: ['GET', 'HEAD'],
      publicEdge: false,
      provenance: {
        sourceKind: 'static-mount',
        evidencePath: `${GENERATED_PUBLIC_DIR}/${SSG_OUTPUT_DIRECTORY}`,
      },
    },
    // The staticFiles loader also aliases `<dir>/index.html` to the extension-less,
    // slash-less path, so the mirror's root is reachable as `/static` too.
    {
      match: 'exact',
      path: `/${SSG_OUTPUT_DIRECTORY}`,
      methods: ['GET', 'HEAD'],
      publicEdge: false,
      provenance: {
        sourceKind: 'public-file',
        evidencePath: `${GENERATED_PUBLIC_DIR}/${SSG_OUTPUT_DIRECTORY}/index.html`,
      },
    },
    ...RUNTIME_ROUTES,
  ];
}

class PublicSurfacePlugin implements Plugin {
  generate(): GenerateResult {
    const input = { projectRoot: getProjectRoot(), workspaceRoot: getWorkspaceRoot() };
    // Runs in this plugin's synchronous generate() body, so it observes public/
    // exactly as the staticFiles plugin's scan did earlier in the same tick.
    assertNoSourceMirror(input.projectRoot, buildGenerateAssetHttpRoutes(input));
    return { httpRoutes: buildPublicSurfaceHttpRoutes(input) };
  }
}

export function publicSurface(): PublicSurfacePlugin {
  return new PublicSurfacePlugin();
}
