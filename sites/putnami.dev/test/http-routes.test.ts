import { describe, expect, it } from 'bun:test';
import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { specTest } from '@putnami/spectest';
import {
  type HttpRoute,
  type HttpRoutesManifest,
  parseAndValidateHttpRoutes,
  serializeCanonicalHttpRoutes,
} from '@putnami/application/http-routes';
import { joinPath } from '@putnami/utils';
import {
  assertNoSourceMirror,
  buildGenerateAssetHttpRoutes,
  buildPublicSurfaceHttpRoutes,
} from '../src/plugins/public-surface.plugin';
import { removeSearchSourceMirror } from '../src/plugins/search-index.plugin';

/**
 * Canonical public-route inventory.
 *
 * `.gen/schema/http-routes.json` is the `putnami.http-routes.v1` artifact
 * Putnami Cloud turns into a generated default-deny allowlist, so these tests
 * pin four properties:
 *
 * - **validity** — it parses and validates against the shared v1 validator;
 * - **completeness** — every path this workload actually serves is represented.
 *   The expectations are DERIVED from the app (the generated static loader, the
 *   `src/api` and `src/app` file trees, `putnami.json`, and the plugin sources),
 *   never hand-copied, so adding a page/endpoint/asset without updating the
 *   inventory fails here instead of rotting silently;
 * - **default-deny** — unknown paths, scanner probes, and undeclared methods on
 *   declared paths match nothing;
 * - **determinism** — the bytes and digest are a pure function of the route
 *   facts, independent of fragment and route order.
 *
 * Regenerate with: putnami build --projects putnami.dev
 */
const PROJECT_ROOT = joinPath(import.meta.dir, '..');
const WORKSPACE_ROOT = joinPath(PROJECT_ROOT, '..', '..');
const ARTIFACT_PATH = joinPath(PROJECT_ROOT, '.gen', 'schema', 'http-routes.json');
const FRAGMENT_DIR = joinPath(PROJECT_ROOT, '.gen', 'http-routes.d');
const STATIC_LOADER_PATH = joinPath(PROJECT_ROOT, '.gen', 'src', 'static', '.static.gen.ts');
const README_PATH = joinPath(PROJECT_ROOT, 'README.md');
const PROJECT_CONFIG_PATH = joinPath(PROJECT_ROOT, 'putnami.json');

const artifactText = existsSync(ARTIFACT_PATH) ? readFileSync(ARTIFACT_PATH, 'utf8') : undefined;
const parsed = artifactText ? parseAndValidateHttpRoutes(artifactText) : undefined;
const manifest = parsed?.manifest;

function routes(): HttpRoute[] {
  if (!manifest) throw new Error(`missing or invalid ${ARTIFACT_PATH}; run: putnami build --projects putnami.dev`);
  return manifest.routes;
}

// ---------------------------------------------------------------------------
// A v1 matcher: exact is byte equality, prefix is a byte prefix ending in '/',
// and a template matches whole segments with at most one '{name...}' catch-all
// absorbing one or more of them.
// ---------------------------------------------------------------------------

function isParameter(segment: string): boolean {
  return segment.length >= 3 && segment.startsWith('{') && segment.endsWith('}');
}

function isCatchAll(segment: string): boolean {
  return isParameter(segment) && segment.slice(1, -1).endsWith('...');
}

function matchesTemplate(template: string, path: string): boolean {
  const expected = template.split('/').slice(1);
  const actual = path.split('/').slice(1);
  const catchAll = expected.findIndex(isCatchAll);
  const fixedMatches = (want: string[], got: string[]): boolean =>
    want.every((segment, index) => (isParameter(segment) ? got[index] !== '' : segment === got[index]));

  if (catchAll < 0) {
    return expected.length === actual.length && fixedMatches(expected, actual);
  }
  const head = expected.slice(0, catchAll);
  const tail = expected.slice(catchAll + 1);
  if (actual.length < head.length + tail.length + 1) return false;
  return (
    fixedMatches(head, actual.slice(0, head.length)) && fixedMatches(tail, actual.slice(actual.length - tail.length))
  );
}

function matches(route: HttpRoute, path: string, method: string): boolean {
  if (!route.methods.includes(method as HttpRoute['methods'][number])) return false;
  if (route.match === 'exact') return route.path === path;
  if (route.match === 'prefix') return path.startsWith(route.path);
  return matchesTemplate(route.path, path);
}

/** Every route representing `path`+`method`, public or not. */
function represented(path: string, method: string): HttpRoute[] {
  return routes().filter((route) => matches(route, path, method));
}

/** Whether the generated allowlist would admit `path`+`method` at the edge. */
function admits(path: string, method: string): boolean {
  return represented(path, method).some((route) => route.publicEdge);
}

function describeRoute(route: HttpRoute): string {
  return `${route.match} ${route.path}`;
}

// ---------------------------------------------------------------------------
// Expectations derived from the workload itself
// ---------------------------------------------------------------------------

/** Static paths the generated loader actually registers with the HttpPlugin. */
function registeredStaticPaths(): string[] {
  const source = readFileSync(STATIC_LOADER_PATH, 'utf8');
  const paths = new Set<string>();
  for (const match of source.matchAll(/\.routeStatic\('([^']*)'/g)) {
    const relative = match[1] as string;
    paths.add(relative.startsWith('/') ? relative : `/${relative}`);
  }
  return [...paths].sort();
}

function walkFiles(root: string, relative = ''): string[] {
  const entries = readdirSync(joinPath(root, relative), { withFileTypes: true });
  return entries.flatMap((entry) => {
    const next = relative ? `${relative}/${entry.name}` : entry.name;
    return entry.isDirectory() ? walkFiles(root, next) : [next];
  });
}

/** Typed API routes, derived from the `src/api` file tree the api() plugin scans. */
function typedApiRoutes(): { path: string; method: string }[] {
  const apiRoot = joinPath(PROJECT_ROOT, 'src', 'api');
  return walkFiles(apiRoot)
    .filter((file) => /(^|\/)(get|post|put|patch|delete|options|head)\.tsx?$/.test(file))
    .map((file) => {
      const segments = file.split('/');
      const method = (segments.pop() as string).replace(/\.tsx?$/, '').toUpperCase();
      const path = `/${segments.map((segment) => segment.replace(/^\[(?:\.\.\.)?([^\]]+)\]$/, '{$1}')).join('/')}`;
      return { path, method };
    })
    .sort((a, b) => (a.path < b.path ? -1 : 1));
}

/** Web page routes, derived from the `src/app` file tree the react() plugin scans. */
function webPagePaths(): string[] {
  const appRoot = joinPath(PROJECT_ROOT, 'src', 'app');
  return walkFiles(appRoot)
    .filter((file) => /(^|\/)page\.tsx$/.test(file))
    .map((file) => {
      const directory = file.replace(/(^|\/)page\.tsx$/, '');
      if (directory === '') return '/';
      // A `[...param]` catch-all page is probed with a concrete path below.
      return `/${directory.replace(/\[\.\.\.[^\]]+\]/g, 'probe-page')}`;
    })
    .sort();
}

/** Routes registered by site plugins at warmup, invisible to generate-time emitters. */
function pluginRuntimeRoutes(): { path: string; method: string }[] {
  const pluginDir = joinPath(PROJECT_ROOT, 'src', 'plugins');
  const found: { path: string; method: string }[] = [];
  for (const file of readdirSync(pluginDir).filter((name) => name.endsWith('.ts'))) {
    const source = readFileSync(joinPath(pluginDir, file), 'utf8');
    for (const match of source.matchAll(/httpPlugin\.route\(\s*'([A-Z]+)',\s*'([^']+)'/g)) {
      found.push({ method: match[1] as string, path: match[2] as string });
    }
  }
  return found.sort((a, b) => (a.path < b.path ? -1 : 1));
}

// ---------------------------------------------------------------------------

describe('putnami.dev canonical HTTP route inventory', () => {
  it('exists (run `putnami build --projects putnami.dev` before tests)', () => {
    expect(existsSync(ARTIFACT_PATH)).toBe(true);
  });

  describe('validity', () => {
    it('validates as putnami.http-routes.v1', () => {
      expect(parsed?.diagnostics ?? []).toEqual([]);
      expect(manifest?.protocol).toBe('putnami.http-routes.v1');
      expect(manifest?.$schema).toBe('https://putnami.dev/schemas/putnami-http-routes-v1.json');
    });

    it('enables generated edge enforcement after the capacity fix is deployed', () => {
      const project = JSON.parse(readFileSync(PROJECT_CONFIG_PATH, 'utf8')) as {
        options?: { '@putnami/cloud'?: { deploy?: { enforceHttpRoutes?: boolean } } };
      };

      expect(project.options?.['@putnami/cloud']?.deploy?.enforceHttpRoutes).toBe(true);
    });

    specTest(
      'never declares a root prefix or an unbounded catch-all',
      {
        feature: 'putnami-dev/public-documentation',
        requirement: 'unmatched-paths-are-not-routed',
        check: 'root-and-catch-all-routes-are-absent',
      },
      () => {
        for (const route of routes()) {
          expect(route.match === 'prefix' && route.path === '/').toBe(false);
          // A catch-all is legal in v1, but this workload declares none: an
          // unbounded `/{rest...}` would hand the edge an allow-everything matcher.
          expect(route.path.split('/').some(isCatchAll)).toBe(false);
        }
      },
    );

    it('carries provenance on every route', () => {
      for (const route of routes()) {
        expect(route.provenance.project).toBe('putnami.dev');
        expect(typeof route.provenance.sourceKind).toBe('string');
      }
    });
  });

  describe('completeness (derived from the workload)', () => {
    specTest(
      'represents every static path the generated loader registers',
      {
        feature: 'putnami-dev/public-documentation',
        requirement: 'route-inventory-is-complete',
        check: 'static-doc-paths-are-inventoried',
      },
      () => {
        expect(existsSync(STATIC_LOADER_PATH)).toBe(true);
        const registered = registeredStaticPaths();
        expect(registered.length).toBeGreaterThan(100);
        const missing = registered.filter((path) => represented(path, 'GET').length === 0);
        expect(missing).toEqual([]);
      },
    );

    specTest(
      'represents every typed API route in src/api',
      {
        feature: 'putnami-dev/public-documentation',
        requirement: 'route-inventory-is-complete',
        check: 'typed-api-routes-are-inventoried',
      },
      () => {
        const missing = typedApiRoutes().filter(({ path, method }) => !admits(path, method));
        expect(missing).toEqual([]);
        // The tree is the source of truth, but a truncated scan must not pass.
        expect(typedApiRoutes().map(({ path }) => path)).toContain('/llms.txt');
      },
    );

    specTest(
      'represents every web page route in src/app',
      {
        feature: 'putnami-dev/public-documentation',
        requirement: 'route-inventory-is-complete',
        check: 'web-page-routes-are-inventoried',
      },
      () => {
        const pages = webPagePaths();
        expect(pages).toContain('/');
        expect(pages).toContain('/docs');
        const missing = pages.filter((path) => !admits(path, 'GET'));
        expect(missing).toEqual([]);
      },
    );

    it('admits the docs catch-all subtree at any depth', () => {
      for (const path of ['/docs/getting-started', '/docs/frameworks/go/caching', '/docs/a/b/c/d']) {
        expect(admits(path, 'GET')).toBe(true);
      }
    });

    specTest(
      'represents every route the site plugins register at warmup',
      {
        feature: 'putnami-dev/public-documentation',
        requirement: 'route-inventory-is-complete',
        check: 'warmup-endpoints-are-inventoried',
      },
      () => {
        const runtime = pluginRuntimeRoutes();
        expect(runtime).toContainEqual({ method: 'POST', path: '/api/content/refresh' });
        expect(runtime).toContainEqual({ method: 'GET', path: '/search/index.json' });
        const missing = runtime.filter(({ path, method }) => represented(path, method).length === 0);
        expect(missing).toEqual([]);
      },
    );

    specTest(
      'represents every committed file under public/',
      {
        feature: 'putnami-dev/public-documentation',
        requirement: 'route-inventory-is-complete',
        check: 'committed-public-files-are-inventoried',
      },
      () => {
        const publicRoot = joinPath(PROJECT_ROOT, 'public');
        const committed = walkFiles(publicRoot).filter(
          // public/docs, public/schemas and public/search are gitignored dev
          // mirrors regenerated on every build; the committed surface is the rest.
          (file) => !/^(docs|schemas|search)\//.test(file),
        );
        expect(committed.length).toBeGreaterThan(0);
        const missing = committed.filter((file) => !admits(`/${file}`, 'GET'));
        expect(missing).toEqual([]);
      },
    );

    specTest(
      'represents every generate.assets copy that lands under public/',
      {
        feature: 'putnami-dev/public-documentation',
        requirement: 'route-inventory-is-complete',
        check: 'asset-copy-routes-are-inventoried',
      },
      () => {
        const assets = buildGenerateAssetHttpRoutes({ projectRoot: PROJECT_ROOT, workspaceRoot: WORKSPACE_ROOT });
        expect(assets.map((route) => `${route.match} ${route.path}`).sort()).toEqual([
          'exact /LICENSE.md',
          'exact /install-commands.txt',
          'exact /install.ps1',
          'exact /install.sh',
          'prefix /docs/',
        ]);
        for (const asset of assets) {
          const probe = asset.match === 'prefix' ? `${asset.path}probe` : asset.path;
          expect(admits(probe, 'GET')).toBe(true);
        }
      },
    );

    it('declares the operational probes and the SSG mirror as non-public', () => {
      for (const path of ['/_/livez', '/_/healthz', '/_/readyz', '/_/version']) {
        expect(represented(path, 'GET').map(describeRoute)).toEqual([`exact ${path}`]);
        expect(admits(path, 'GET')).toBe(false);
      }
      // .gen/public/static is the SSG output the SSR reads from disk; it is a
      // byte-duplicate of pages already served at their canonical URL.
      expect(represented('/static/docs/concepts.html', 'GET').map(describeRoute)).toEqual(['prefix /static/']);
      expect(admits('/static/docs/concepts.html', 'GET')).toBe(false);
      expect(admits('/static', 'GET')).toBe(false);
    });

    it('keeps the staticFiles plugin out of the subtrees this site declares itself', () => {
      // The site owns /docs/ and /static/ because the staticFiles plugin only scans
      // the SOURCE public/ folder, which never holds them at scan time. If that
      // ever changes the aggregate fails with http_routes.duplicate_route, so
      // pin the split here to fail with an explanation instead. Framework facts
      // are the ones stamped with the emitting package.
      const fragment = JSON.parse(readFileSync(joinPath(FRAGMENT_DIR, 'application.json'), 'utf8')) as {
        routes: HttpRoute[];
      };
      const framework = fragment.routes
        .filter((route) => route.provenance.package === '@putnami/application')
        .map(describeRoute);
      expect(framework).not.toContain('prefix /docs/');
      expect(framework).not.toContain('prefix /static/');
      expect(framework).toContain('prefix /schemas/');
      // The search index is written in postGenerate(), after the scan, and only
      // under .gen/public/search/. A `prefix /search/` here means a stale
      // public/search mirror reached the scan, so the inventory depended on what
      // an earlier build left in the tree instead of on this build's inputs.
      expect(framework).not.toContain('prefix /search/');

      const site = fragment.routes.filter((route) => route.provenance.package === undefined).map(describeRoute);
      expect(site).toContain('prefix /docs/');
      expect(site).toContain('prefix /static/');
      expect(site).toContain('exact /search/index.json');
    });

    it('removes a stale public/search mirror before the staticFiles plugin scans', () => {
      const projectRoot = mkdtempSync(joinPath(tmpdir(), 'putnami-search-mirror-'));
      try {
        mkdirSync(joinPath(projectRoot, 'public', 'search'), { recursive: true });
        writeFileSync(joinPath(projectRoot, 'public', 'search', 'index.json'), '{}');
        writeFileSync(joinPath(projectRoot, 'public', 'robots.txt'), 'User-agent: *');

        removeSearchSourceMirror(projectRoot);

        expect(existsSync(joinPath(projectRoot, 'public', 'search'))).toBe(false);
        expect(existsSync(joinPath(projectRoot, 'public', 'robots.txt'))).toBe(true);
        // Idempotent: the steady state has no mirror at all.
        expect(() => removeSearchSourceMirror(projectRoot)).not.toThrow();
      } finally {
        rmSync(projectRoot, { recursive: true, force: true });
      }
    });

    specTest(
      'refuses to emit when a stale dev mirror would make the staticFiles plugin declare the same fact',
      {
        feature: 'putnami-dev/public-documentation',
        requirement: 'no-generated-mirror-in-source',
        check: 'generated-source-mirror-fails-route-emission',
      },
      () => {
        const projectRoot = mkdtempSync(joinPath(tmpdir(), 'putnami-public-surface-'));
        try {
          const declared = buildGenerateAssetHttpRoutes({ projectRoot: PROJECT_ROOT, workspaceRoot: WORKSPACE_ROOT });
          expect(() => assertNoSourceMirror(projectRoot, declared)).not.toThrow();

          mkdirSync(joinPath(projectRoot, 'public', 'docs', 'platform'), { recursive: true });
          writeFileSync(joinPath(projectRoot, 'public', 'docs', 'platform', 'index.md'), '# stale mirror');
          expect(() => assertNoSourceMirror(projectRoot, declared)).toThrow(/public\/docs.*duplicate_route/s);
        } finally {
          rmSync(projectRoot, { recursive: true, force: true });
        }
      },
    );

    it('accepts an empty dev mirror directory, which is the steady state', () => {
      const projectRoot = mkdtempSync(joinPath(tmpdir(), 'putnami-public-surface-'));
      try {
        mkdirSync(joinPath(projectRoot, 'public', 'docs'), { recursive: true });
        const declared = buildGenerateAssetHttpRoutes({ projectRoot: PROJECT_ROOT, workspaceRoot: WORKSPACE_ROOT });
        expect(() => assertNoSourceMirror(projectRoot, declared)).not.toThrow();
      } finally {
        rmSync(projectRoot, { recursive: true, force: true });
      }
    });
  });

  describe('default deny', () => {
    const unknownPaths = [
      '/nope',
      '/index.php',
      '/.env',
      '/.env.local',
      '/.git/config',
      '/.aws/credentials',
      '/wp-admin',
      '/wp-login.php',
      '/admin',
      '/administrator/index.php',
      '/phpmyadmin',
      '/config.json',
      '/server-status',
      '/actuator/health',
      '/.well-known/security.txt',
      '/%2E%2E%2Fetc%2Fpasswd',
      '/docs2',
      '/assets',
      '/fonts',
      '/schemas',
      '/searchindex',
      '/react',
      '/dl',
      '/api/content',
      '/api/content/refresh/extra',
    ];

    it.each(unknownPaths)('rejects GET %s', (path) => {
      expect(admits(path, 'GET')).toBe(false);
    });

    it.each(['POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS', 'HEAD'])('rejects %s on the health probe', (method) => {
      expect(admits('/_/healthz', method)).toBe(false);
    });

    const undeclaredMethods: [string, string][] = [
      ['POST', '/'],
      ['DELETE', '/docs'],
      ['PUT', '/install.sh'],
      ['PUT', '/install.ps1'],
      ['PATCH', '/llms.txt'],
      ['OPTIONS', '/'],
      ['POST', '/docs/getting-started'],
      ['GET', '/api/content/refresh'],
      ['DELETE', '/api/content/refresh'],
      ['POST', '/search/index.json'],
      ['POST', '/dl/putnami'],
    ];

    it.each(undeclaredMethods)('rejects %s %s', (method, path) => {
      expect(admits(path, method)).toBe(false);
    });

    it('admits the declared method on those same paths', () => {
      expect(admits('/', 'GET')).toBe(true);
      expect(admits('/docs', 'GET')).toBe(true);
      expect(admits('/install.sh', 'GET')).toBe(true);
      expect(admits('/install.ps1', 'GET')).toBe(true);
      expect(admits('/llms.txt', 'GET')).toBe(true);
      expect(admits('/dl/putnami', 'GET')).toBe(true);
      expect(admits('/api/content/refresh', 'POST')).toBe(true);
    });
  });

  describe('determinism', () => {
    it('is the canonical serialization of its own route facts', () => {
      const canonical = serializeCanonicalHttpRoutes({ routes: routes() });
      expect(canonical.diagnostics).toEqual([]);
      expect(canonical.text).toBe(artifactText as string);
    });

    it('produces the same bytes and digest regardless of route order', () => {
      const reversed = [...routes()].reverse();
      const rotated = [...routes().slice(7), ...routes().slice(0, 7)];
      const baseline = serializeCanonicalHttpRoutes({ routes: routes() });
      for (const order of [reversed, rotated]) {
        const shuffled = serializeCanonicalHttpRoutes({ routes: order });
        expect(shuffled.text).toBe(baseline.text as string);
        expect(shuffled.manifest?.digest).toBe(baseline.manifest?.digest as string);
      }
    });

    it('produces the same digest regardless of fragment order', () => {
      const fragments = readdirSync(FRAGMENT_DIR)
        .filter((name) => name.endsWith('.json'))
        .sort();
      expect(fragments.length).toBeGreaterThan(1);
      const load = (name: string): HttpRoute[] =>
        (JSON.parse(readFileSync(joinPath(FRAGMENT_DIR, name), 'utf8')) as { routes: HttpRoute[] }).routes;
      const forward = fragments.flatMap(load);
      const backward = [...fragments].reverse().flatMap(load);
      expect(serializeCanonicalHttpRoutes({ routes: forward }).manifest?.digest).toBe(
        (manifest as HttpRoutesManifest).digest,
      );
      expect(serializeCanonicalHttpRoutes({ routes: backward }).manifest?.digest).toBe(
        (manifest as HttpRoutesManifest).digest,
      );
    });

    it('emits site-owned facts in a stable order for identical inputs', () => {
      const once = buildPublicSurfaceHttpRoutes({ projectRoot: PROJECT_ROOT, workspaceRoot: WORKSPACE_ROOT });
      const twice = buildPublicSurfaceHttpRoutes({ projectRoot: PROJECT_ROOT, workspaceRoot: WORKSPACE_ROOT });
      expect(JSON.stringify(twice)).toBe(JSON.stringify(once));
    });
  });

  describe('probe matrix (README.md)', () => {
    interface ProbeRow {
      path: string;
      method: string;
      disposition: string;
      admittedBy: string;
    }

    function readProbeMatrix(): ProbeRow[] {
      const readme = readFileSync(README_PATH, 'utf8');
      const rows: ProbeRow[] = [];
      for (const line of readme.split('\n')) {
        const cells = line.split('|').map((cell) => cell.trim());
        // | `path` | METHOD | allow/reject | `match path` or — |
        if (cells.length !== 6 || cells[0] !== '' || cells[5] !== '') continue;
        const disposition = cells[3] as string;
        if (disposition !== 'allow' && disposition !== 'reject') continue;
        rows.push({
          path: (cells[1] as string).replace(/`/g, ''),
          method: cells[2] as string,
          disposition,
          admittedBy: (cells[4] as string).replace(/`/g, ''),
        });
      }
      return rows;
    }

    it('documents both dispositions with real probes', () => {
      const matrix = readProbeMatrix();
      expect(matrix.length).toBeGreaterThanOrEqual(20);
      expect(matrix.some((row) => row.disposition === 'allow')).toBe(true);
      expect(matrix.some((row) => row.disposition === 'reject')).toBe(true);
    });

    it('agrees with the emitted artifact on every row', () => {
      const mismatches = readProbeMatrix().filter((row) => {
        const matched = represented(row.path, row.method).filter((route) => route.publicEdge);
        if (row.disposition === 'reject') return matched.length > 0 || row.admittedBy !== '—';
        return matched.length === 0 || !matched.map(describeRoute).includes(row.admittedBy);
      });
      expect(mismatches).toEqual([]);
    });
  });
});
