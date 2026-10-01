import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { cpSync, mkdirSync, readFileSync } from 'node:fs';
import { fileExists, getBuildInfo, joinPath } from '@putnami/utils';
import { HttpPlugin, application, http, staticFiles } from '@putnami/application';
import { specTest } from '@putnami/spectest';
import { ReactPlugin, react } from '@putnami/web';
import { statusLabel, statusOf } from '../src/lib/support/catalog';
import { installerRun } from '../src/plugins/installer-run.plugin';
import { readSupportCatalog } from '../src/lib/support/catalog.server';
import { toolList } from '../src/lib/tools';

const PROJECT_ROOT = joinPath(import.meta.dir, '..');
const WORKSPACE_ROOT = joinPath(PROJECT_ROOT, '..', '..');
/**
 * The max-age src/main.ts passes to staticFiles(). The test app passes it too,
 * so install.sh?run= is compared with /install.sh as the site serves it.
 */
const STATIC_CACHE_MAX_AGE = 2_592_000;

function productSurfaceSection(html: string): string {
  const start = html.indexOf('aria-label="Product surfaces"');
  if (start < 0) return '';
  const end = html.indexOf('</section>', start);
  return end < 0 ? '' : html.slice(start, end);
}

function toolCard(section: string, href: string): string {
  const start = section.indexOf(`href="${href}"`);
  if (start < 0) return '';
  const end = section.indexOf('</a>', start);
  return end < 0 ? '' : section.slice(start, end);
}

function statusMarkup(card: string): string {
  const marker = '<div class="tc-status">';
  const start = card.indexOf(marker);
  if (start < 0) return '';
  const contentStart = start + marker.length;
  const end = card.indexOf('</div>', contentStart);
  return end < 0 ? '' : card.slice(contentStart, end);
}

/**
 * Start an Application skipping the generate phase.
 *
 * The generate phase spawns `bun build` for client hydration scripts. When
 * tests are executed through the putnami CLI test runner, the child process
 * environment may not include PATH, causing `bun` to be unresolvable. Since
 * `putnami build` already runs before tests and produces all generated files,
 * we skip generate and only run warmup → start.
 */
async function startSkipGenerate(app: Application, generatedReactAppPath: string): Promise<void> {
  // Set reactApplicationPath on the ReactPlugin so warmup() can find the generated routes
  const reactPlugin = app.findPlugin(ReactPlugin);
  if (reactPlugin) {
    reactPlugin.reactApplicationPath = generatedReactAppPath;
  }

  // Run warmup → start on all plugins, skipping the generate phase
  const plugins = app.collectPlugins();
  for (const { plugin, owner } of plugins) {
    // biome-ignore lint/performance/noAwaitInLoops: Sequential warmup
    await plugin.warmup?.(owner);
  }

  const startPromises = plugins
    .filter(({ plugin }) => typeof plugin.start === 'function')
    .map(({ plugin, owner }) => plugin.start?.(owner));
  await Promise.all(startPromises);

  app.markAsRunning();
}

describe('putnami.dev', () => {
  let app: Application;
  let baseUrl: string;

  beforeAll(async () => {
    const scanPath = joinPath(PROJECT_ROOT, 'src', 'app');
    const generatedDocsDir = joinPath(PROJECT_ROOT, '.gen', 'public', 'docs');

    // Copy doc files to .gen/public/docs if not already present
    // (normally done by the build pipeline's asset copy step)
    if (!fileExists(generatedDocsDir)) {
      const sourceDocsDir = joinPath(PROJECT_ROOT, 'doc');
      mkdirSync(generatedDocsDir, { recursive: true });
      cpSync(sourceDocsDir, generatedDocsDir, { recursive: true });
    }

    app = application()
      .use(http({ port: 0 }))
      .use(react({ scanPath }))
      .use(installerRun())
      .use(staticFiles({ publicFolder: 'public', cacheMaxAge: STATIC_CACHE_MAX_AGE }));

    // Use startSkipGenerate when generated files already exist (from putnami build),
    // otherwise use the full lifecycle for standalone bun test runs.
    const generatedReactApp = joinPath(PROJECT_ROOT, '.gen', 'src', 'app', '.react-application.gen.tsx');
    if (fileExists(generatedReactApp)) {
      // ReactPlugin.warmup() uses joinPath(getProjectRoot(), reactApplicationPath),
      // so the path must be relative to the project root.
      const relativeReactApp = '.gen/src/app/.react-application.gen.tsx';
      await startSkipGenerate(app, relativeReactApp);
    } else {
      // Never publish from a test: .gen/schema/capabilities.json belongs to
      // the scheduler generate step, and this app is not the site.
      await app.build({ publishCapabilityManifest: false });
      await app.start();
    }

    const port = app.getPlugin(HttpPlugin).getServer()?.port ?? 3001;
    baseUrl = `http://localhost:${port}`;
  });

  afterAll(async () => {
    await app.stop();
  });

  describe('Static Files', () => {
    it('should serve install.sh', async () => {
      const res = await fetch(`${baseUrl}/install.sh`);
      expect(res.status).toBe(200);

      const script = await res.text();
      expect(script).toContain('#!/bin/bash');
      expect(script).toContain('Putnami CLI');
    });

    /**
     * The served script is the public install path: whatever this site publishes
     * is what `curl -fsSL https://putnami.dev/install.sh | bash` executes on a
     * machine with no way to audit it first. The behavior is specified in
     * tooling/cli/doc/22-installing-the-cli.md and decided in ADR 0012, and it
     * is pinned by tooling/cli/internal/installscript — but that suite reads the
     * repository copy. These assertions cover the publishing seam: a
     * `generate.assets` change that shipped a stale or unverifying installer
     * would pass every CLI test and fail here.
     */
    it('should serve an install.sh that still verifies what it downloads', async () => {
      const res = await fetch(`${baseUrl}/install.sh`);
      expect(res.status).toBe(200);
      const script = await res.text();

      // Reads the digest the registry advertised, both header forms.
      expect(script).toContain('X-Integrity');
      expect(script).toContain('sha-256=');
      // Computes a local digest to compare it against.
      expect(script).toMatch(/sha256sum|shasum -a 256/);
      // Says "verified" only on the branch where the comparison passed, and
      // refuses when there is nothing to compare.
      expect(script).toContain('Integrity verified');
      expect(script).toContain('refusing to install an unverified binary');
      expect(script).toContain('PUTNAMI_UNSAFE_INSTALL');
      // Binds the install to the version the registry resolved.
      expect(script).toContain('X-Resolved-Version');
      expect(script).toContain('refusing to install a stale build');
      // Points at the URL that actually serves this file.
      expect(script).toContain('https://putnami.dev/install.sh');
      expect(script).not.toContain('put.putnami.dev/install.sh');
    });

    it('should serve an install.sh that never escalates privileges', async () => {
      const res = await fetch(`${baseUrl}/install.sh`);
      expect(res.status).toBe(200);
      const script = await res.text();

      // Strip comments and quoted strings: the script promises in prose that it
      // never escalates, and a scan that cannot tell prose from a command would
      // force that promise out of the output.
      const executable = script
        .split('\n')
        .map((line) =>
          line
            .replace(/'[^']*'/g, ' ')
            .replace(/"[^"]*"/g, ' ')
            .replace(/#.*$/, ''),
        )
        .join('\n');

      // Non-vacuity: stripping must leave the script's executable half behind,
      // otherwise the scan below passes by having nothing to read. Counted as
      // surviving code lines against a fixed floor, not as a fraction of the
      // file: stripping removes comments, so added prose shrinks the ratio
      // without weakening the scan and would fail this for no reason. 795 lines
      // survive today.
      expect(executable).toContain('verify_download_integrity');
      expect(executable).toContain('ensure_writable_install_dir');
      const executableLines = executable.split('\n').filter((line) => line.trim()).length;
      expect(executableLines).toBeGreaterThan(400);

      for (const escalator of ['sudo', 'doas', 'pkexec', 'run0']) {
        expect(executable).not.toMatch(new RegExp(`(^|[^\\w.-])${escalator}([^\\w.-]|$)`));
      }
    });

    /**
     * `curl -fsSL "https://putnami.dev/install.sh?run=<command>" | bash`: the
     * same script with the command set in its one placeholder line. The
     * installer side is pinned by tooling/cli/internal/installscript.
     */
    describe('install.sh?run=<command>', () => {
      const repositoryScript = () =>
        readFileSync(joinPath(WORKSPACE_ROOT, 'tooling', 'cli', 'scripts', 'install.sh'), 'utf8');

      it('serves the repository script unchanged without ?run=, whatever else the query holds', async () => {
        const plain = await fetch(`${baseUrl}/install.sh`);
        const plainBody = await plain.text();
        expect(plain.status).toBe(200);
        expect(plainBody).toBe(repositoryScript());
        expect(plainBody).toContain('\nRUN_COMMAND_DEFAULT=""\n');

        const other = await fetch(`${baseUrl}/install.sh?foo=bar`);
        expect(other.status).toBe(200);
        expect(await other.text()).toBe(plainBody);
        for (const header of ['content-type', 'cache-control', 'etag']) {
          expect({ header, value: other.headers.get(header) }).toEqual({ header, value: plain.headers.get(header) });
        }
      });

      it('sets the command in the placeholder line and changes nothing else', async () => {
        const plain = await fetch(`${baseUrl}/install.sh`);
        const plainBody = await plain.text();
        const res = await fetch(`${baseUrl}/install.sh?run=deploy`);
        expect(res.status).toBe(200);
        const body = await res.text();

        const before = plainBody.split('\n');
        const after = body.split('\n');
        expect(after.length).toBe(before.length);
        const changed = before.flatMap((line, index) => (line === after[index] ? [] : [[line, after[index]]]));
        expect(changed).toEqual([['RUN_COMMAND_DEFAULT=""', 'RUN_COMMAND_DEFAULT="deploy"']]);

        // Served like the static script: same type, same cache policy.
        expect(res.headers.get('content-type')).toBe(plain.headers.get('content-type'));
        expect(res.headers.get('content-type')).toContain('application/x-sh');
        expect(res.headers.get('cache-control')).toBe(plain.headers.get('cache-control'));
        expect(res.headers.get('cache-control')).toMatch(/^public, max-age=\d+$/);
        // Each variant has its own validator.
        const etag = res.headers.get('etag');
        expect(etag).toBeTruthy();
        expect(etag).not.toBe(plain.headers.get('etag'));
        const other = await fetch(`${baseUrl}/install.sh?run=preview`);
        expect(other.headers.get('etag')).not.toBe(etag);
        await other.text();
      });

      it('answers HEAD and a matching If-None-Match like the static route', async () => {
        const res = await fetch(`${baseUrl}/install.sh?run=deploy`);
        const body = await res.text();
        const etag = res.headers.get('etag') as string;

        const head = await fetch(`${baseUrl}/install.sh?run=deploy`, { method: 'HEAD' });
        expect(head.status).toBe(200);
        expect(head.headers.get('content-length')).toBe(String(Buffer.byteLength(body, 'utf8')));
        expect(head.headers.get('etag')).toBe(etag);
        expect(await head.text()).toBe('');

        const cached = await fetch(`${baseUrl}/install.sh?run=deploy`, { headers: { 'If-None-Match': etag } });
        expect(cached.status).toBe(304);
        expect(await cached.text()).toBe('');
      });

      it('rejects every run value that is not one command name, without echoing it', async () => {
        for (const query of [
          'run=',
          'run=Deploy',
          'run=-deploy',
          `run=a${'b'.repeat(64)}`,
          'run=deploy&run=preview',
          'run=deploy;id',
          'run=deploy%3Bid',
          'run=$(id)',
          'run=%24(id)',
          'run=`id`',
          'run=deploy%22',
          'run=deploy%27',
          'run=deploy%0aid',
          'run=deploy%0A',
          'run=deploy+now',
          'run=deploy%20now',
        ]) {
          const res = await fetch(`${baseUrl}/install.sh?${query}`);
          const body = await res.text();
          expect({ query, status: res.status }).toEqual({ query, status: 400 });
          expect(res.headers.get('content-type')).toContain('text/plain');
          expect(res.headers.get('cache-control')).toBe('no-store');
          expect(body).not.toContain('#!/bin/bash');
          expect(body).not.toContain('id');
        }
      });
    });

    it('should serve the command map the installer reads for ?run=', async () => {
      const res = await fetch(`${baseUrl}/install-commands.txt`);
      expect(res.status).toBe(200);
      expect(res.headers.get('content-type')).toContain('text/plain');
      const body = await res.text();
      expect(body).toBe(
        readFileSync(joinPath(WORKSPACE_ROOT, 'tooling', 'cli', 'scripts', 'install-commands.txt'), 'utf8'),
      );
      expect(body.split('\n')[0]).toBe('putnami.install-commands.v1');
    });

    /**
     * The Windows counterpart of install.sh, what `irm https://putnami.dev/install.ps1 | iex`
     * executes. tooling/cli/internal/installscript pins its behavior on the
     * repository copy; these assertions cover the publishing seam.
     */
    it('should serve an install.ps1 that still verifies what it downloads', async () => {
      const res = await fetch(`${baseUrl}/install.ps1`);
      expect(res.status).toBe(200);
      const bytes = new Uint8Array(await res.arrayBuffer());
      // Windows PowerShell 5.1 decodes a response without a charset in the ANSI
      // code page, which reads ASCII unchanged and nothing else reliably.
      expect(bytes.every((byte) => byte < 0x80)).toBe(true);
      const script = new TextDecoder().decode(bytes);

      expect(script).toContain('Set-StrictMode -Version 3.0');
      // Hashes the download and compares it with the advertised digest, and
      // says "verified" only after that.
      expect(script).toContain('$actual = Get-FileSha256 $AssetFile');
      expect(script).toContain('if ($actual -cne $expected) {');
      expect(script).toContain('Integrity verified');
      expect(script).toContain('refusing to install an unverified binary');
      expect(script).toContain('PUTNAMI_UNSAFE_INSTALL');
      expect(script).toContain('refusing to install a stale build');
      expect(script).toContain('https://putnami.dev/install.ps1');
      expect(script).not.toContain('put.putnami.dev/install.ps1');
    });

    it('should serve an install.ps1 that never escalates privileges', async () => {
      const res = await fetch(`${baseUrl}/install.ps1`);
      expect(res.status).toBe(200);
      const script = await res.text();

      const executable = script
        .split('\n')
        .map((line) =>
          line
            .replace(/'[^']*'/g, ' ')
            .replace(/"[^"]*"/g, ' ')
            .replace(/#.*$/, ''),
        )
        .join('\n')
        .toLowerCase();

      // Non-vacuity: the stripped text still holds the script's code.
      expect(executable).toContain('function confirm-downloadintegrity');
      expect(executable).toContain('function add-userpathentry');

      for (const escalator of ['runas', 'start-process', 'set-executionpolicy', 'hklm:', 'gsudo', 'sudo']) {
        expect(executable).not.toMatch(new RegExp(`(^|[^\\w.-])${escalator}([^\\w.-]|$)`));
      }
    });

    /**
     * `irm "https://putnami.dev/install.ps1?run=<command>" | iex`: the Windows
     * installer with the command set in its one placeholder line, served like
     * the static script. The installer side is pinned by
     * tooling/cli/internal/installscript.
     */
    describe('install.ps1?run=<command>', () => {
      const repositoryScript = () =>
        readFileSync(joinPath(WORKSPACE_ROOT, 'tooling', 'cli', 'scripts', 'install.ps1'), 'utf8');

      it('serves the repository script unchanged without ?run=, whatever else the query holds', async () => {
        const plain = await fetch(`${baseUrl}/install.ps1`);
        const plainBody = await plain.text();
        expect(plain.status).toBe(200);
        expect(plainBody).toBe(repositoryScript());
        expect(plainBody).toContain("\n$RunCommandDefault = ''\n");

        const other = await fetch(`${baseUrl}/install.ps1?foo=bar`);
        expect(other.status).toBe(200);
        expect(await other.text()).toBe(plainBody);
        for (const header of ['content-type', 'cache-control', 'etag']) {
          expect({ header, value: other.headers.get(header) }).toEqual({ header, value: plain.headers.get(header) });
        }
      });

      it('sets the command in the placeholder line and changes nothing else', async () => {
        const plain = await fetch(`${baseUrl}/install.ps1`);
        const plainBody = await plain.text();
        const res = await fetch(`${baseUrl}/install.ps1?run=deploy`);
        expect(res.status).toBe(200);
        const bytes = new Uint8Array(await res.arrayBuffer());
        // Windows PowerShell 5.1 decodes a response without a charset in the
        // ANSI code page, which reads ASCII unchanged.
        expect(bytes.every((byte) => byte < 0x80)).toBe(true);
        const body = new TextDecoder().decode(bytes);

        const before = plainBody.split('\n');
        const after = body.split('\n');
        expect(after.length).toBe(before.length);
        const changed = before.flatMap((line, index) => (line === after[index] ? [] : [[line, after[index]]]));
        expect(changed).toEqual([["$RunCommandDefault = ''", "$RunCommandDefault = 'deploy'"]]);

        // Served like the static script: same type, same cache policy.
        expect(res.headers.get('content-type')).toBe(plain.headers.get('content-type'));
        expect(res.headers.get('cache-control')).toBe(plain.headers.get('cache-control'));
        expect(res.headers.get('cache-control')).toMatch(/^public, max-age=\d+$/);
        // Each variant has its own validator, distinct from install.sh's.
        const etag = res.headers.get('etag');
        expect(etag).toBeTruthy();
        expect(etag).not.toBe(plain.headers.get('etag'));
        const shell = await fetch(`${baseUrl}/install.sh?run=deploy`);
        expect(shell.headers.get('etag')).not.toBe(etag);
        await shell.text();
      });

      it('answers HEAD and a matching If-None-Match like the static route', async () => {
        const res = await fetch(`${baseUrl}/install.ps1?run=deploy`);
        const body = await res.text();
        const etag = res.headers.get('etag') as string;

        const head = await fetch(`${baseUrl}/install.ps1?run=deploy`, { method: 'HEAD' });
        expect(head.status).toBe(200);
        expect(head.headers.get('content-length')).toBe(String(Buffer.byteLength(body, 'utf8')));
        expect(head.headers.get('etag')).toBe(etag);
        expect(await head.text()).toBe('');

        const cached = await fetch(`${baseUrl}/install.ps1?run=deploy`, { headers: { 'If-None-Match': etag } });
        expect(cached.status).toBe(304);
        expect(await cached.text()).toBe('');
      });

      it('rejects every run value that is not one command name, without echoing it', async () => {
        for (const query of [
          'run=',
          'run=Deploy',
          'run=-deploy',
          `run=a${'b'.repeat(64)}`,
          'run=deploy&run=preview',
          'run=deploy;id',
          'run=deploy%3Bid',
          'run=$(id)',
          'run=%24(id)',
          'run=`id`',
          'run=deploy%22',
          'run=deploy%27',
          "run=deploy'%3Bid",
          'run=deploy%0aid',
          'run=deploy%0A',
          'run=deploy+now',
          'run=deploy%20now',
        ]) {
          // biome-ignore lint/performance/noAwaitInLoops: one request at a time, so a failure names its query
          const res = await fetch(`${baseUrl}/install.ps1?${query}`);
          const body = await res.text();
          expect({ query, status: res.status }).toEqual({ query, status: 400 });
          expect(res.headers.get('content-type')).toContain('text/plain');
          expect(res.headers.get('cache-control')).toBe('no-store');
          expect(body).not.toContain('Set-StrictMode');
          expect(body).not.toContain('id');
        }
      });
    });

    it('should serve favicon SVG', async () => {
      const res = await fetch(`${baseUrl}/assets/favico.svg`);
      expect(res.status).toBe(200);

      const contentType = res.headers.get('content-type');
      expect(contentType).toContain('svg');
    });

    it('should serve logo assets', async () => {
      const res = await fetch(`${baseUrl}/assets/putnami-logo.svg`);
      expect(res.status).toBe(200);
    });
  });

  describe('SSR - Home Page', () => {
    it('should render the home page at /', async () => {
      const res = await fetch(`${baseUrl}/`);
      expect(res.status).toBe(200);

      const html = await res.text();
      expect(html).toContain('<!DOCTYPE html>');
      expect(html).toContain('</html>');
    });

    it('should return HTML content type', async () => {
      const res = await fetch(`${baseUrl}/`);
      const contentType = res.headers.get('content-type');
      expect(contentType).toContain('text/html');
    });

    it('should include the page title in the rendered HTML', async () => {
      const res = await fetch(`${baseUrl}/`);
      const html = await res.text();
      expect(html).toContain('Putnami');
    });

    it('should include the hero section content', async () => {
      const res = await fetch(`${baseUrl}/`);
      const html = await res.text();
      expect(html).toContain('Declare the system once.');
    });

    it('should include navigation links', async () => {
      const res = await fetch(`${baseUrl}/`);
      const html = await res.text();
      expect(html).toContain('/docs');
      expect(html).toContain('Get started');
    });

    it('should be statically rendered with the islands runtime (no full-page hydration)', async () => {
      const res = await fetch(`${baseUrl}/`);
      const html = await res.text();
      // SSG: zero base JS — no React Router full-page hydration payload.
      expect(html).not.toContain('__staticRouterHydrationData');
      // Interactive parts hydrate as islands via the islands runtime.
      expect(html).toContain('<putnami-island');
      expect(html).toContain('/react-islands/islands');
    });

    it('should include meta viewport tag', async () => {
      const res = await fetch(`${baseUrl}/`);
      const html = await res.text();
      expect(html).toContain('viewport');
      expect(html).toContain('width=device-width');
    });

    it('should include the favicon link', async () => {
      const res = await fetch(`${baseUrl}/`);
      const html = await res.text();
      expect(html).toContain('favico.svg');
    });

    it('should include the footer', async () => {
      const res = await fetch(`${baseUrl}/`);
      const html = await res.text();
      expect(html).toContain('FSL-1.1-MIT');
    });

    it('should include self-hosted font preload', async () => {
      const res = await fetch(`${baseUrl}/`);
      const html = await res.text();
      expect(html).toContain('/fonts/inter-latin.woff2');
    });
  });

  describe('SSR - Docs Overview Page', () => {
    it('should render the docs overview at /docs', async () => {
      const res = await fetch(`${baseUrl}/docs`);
      expect(res.status).toBe(200);

      const html = await res.text();
      expect(html).toContain('<!DOCTYPE html>');
    });

    it('should return HTML content type for docs', async () => {
      const res = await fetch(`${baseUrl}/docs`);
      const contentType = res.headers.get('content-type');
      expect(contentType).toContain('text/html');
    });

    it('should include docs overview content', async () => {
      const res = await fetch(`${baseUrl}/docs`);
      const html = await res.text();
      expect(html).toContain('Overview');
    });

    it('should include the sidebar navigation', async () => {
      const res = await fetch(`${baseUrl}/docs`);
      const html = await res.text();
      // The docs layout includes a Sidebar component with navItems
      expect(html).toContain('Getting Started');
    });

    it('should serialize navigation items into the navbar island props', async () => {
      const res = await fetch(`${baseUrl}/docs`);
      const html = await res.text();
      // navItems are passed to the navbar island as serialized props so the
      // mobile menu can render the docs tree after hydration.
      expect(html).toContain('data-island="navbar"');
      expect(html).toContain('navItems');
    });

    specTest(
      'renders every classified product-surface badge from the reviewed catalog',
      {
        feature: 'putnami-dev/published-support-catalog',
        requirement: 'catalog-is-the-only-source',
        check: 'documented-surface-badges-resolve-from-reviewed-catalog',
      },
      async () => {
        const response = await fetch(`${baseUrl}/docs`);
        expect(response.status).toBe(200);
        const surfaces = productSurfaceSection(await response.text());
        expect(surfaces).not.toBe('');

        const catalog = readSupportCatalog(WORKSPACE_ROOT);
        for (const tool of toolList()) {
          if (!tool.supportSubject) continue;
          const status = statusOf(catalog, tool.supportSubject);
          expect(status).toBeDefined();

          const card = toolCard(surfaces, tool.href);
          expect(card).not.toBe('');
          expect(statusMarkup(card)).toContain(statusLabel(status ?? 'stable'));
        }
      },
    );

    specTest(
      'renders no badge on the actual documented surface the catalog does not classify',
      {
        feature: 'putnami-dev/published-support-catalog',
        requirement: 'unclassified-subject-shows-no-status',
        check: 'unclassified-surface-renders-no-badge',
      },
      async () => {
        const response = await fetch(`${baseUrl}/docs`);
        expect(response.status).toBe(200);
        const surfaces = productSurfaceSection(await response.text());
        const unclassified = toolList().filter((tool) => tool.supportSubject === null);
        expect(unclassified.length).toBeGreaterThan(0);

        for (const tool of unclassified) {
          const card = toolCard(surfaces, tool.href);
          expect(card).not.toBe('');
          expect(statusMarkup(card)).toBe('');
        }
      },
    );

    specTest(
      'presents system-model pages separately from product surfaces',
      {
        feature: 'putnami-dev/public-documentation',
        requirement: 'model-pages-are-not-surfaces',
        check: 'system-model-is-separated-from-product-surfaces',
      },
      async () => {
        const res = await fetch(`${baseUrl}/docs`);
        expect(res.status).toBe(200);
        const html = await res.text();
        const modelStart = html.indexOf('aria-label="The system model"');
        const surfacesStart = html.indexOf('aria-label="Product surfaces"', modelStart);
        expect(modelStart).toBeGreaterThan(-1);
        expect(surfacesStart).toBeGreaterThan(modelStart);

        const modelSection = html.slice(modelStart, surfacesStart);
        for (const href of ['/docs/why', '/docs/concepts', '/docs/protocols', '/docs/agents']) {
          expect(modelSection).toContain(`href="${href}"`);
        }
        expect(modelSection).not.toContain('tc-pages');
        expect(modelSection).not.toContain('tc-status');
        for (const glyph of ['>CLI<', '>TS<', '>GO<', '>PY<', '>PLT<']) {
          expect(modelSection).not.toContain(glyph);
        }
      },
    );
  });

  describe('SSR - Dynamic Doc Pages', () => {
    it('should render a getting-started doc page', async () => {
      const res = await fetch(`${baseUrl}/docs/getting-started`);
      expect(res.status).toBe(200);

      const html = await res.text();
      expect(html).toContain('<!DOCTYPE html>');
    });

    it('should render doc page with markdown content converted to HTML', async () => {
      const res = await fetch(`${baseUrl}/docs/getting-started`);
      const html = await res.text();
      // Rendered markdown should produce HTML tags
      expect(html).toContain('<h');
    });

    /**
     * Microsoft Defender blocks the install one-liner passed to powershell on
     * a command line (Trojan:Win32/Commando.A!ml). The page gives the one-liner
     * to type in a PowerShell window, and gives cmd.exe a download run with
     * -File.
     */
    it('should give Windows install commands that Defender lets run', async () => {
      const res = await fetch(`${baseUrl}/docs/getting-started`);
      expect(res.status).toBe(200);
      const text = (await res.text()).replace(/<[^>]+>/g, '');

      expect(text).toContain('PowerShell window');
      expect(text).toContain('curl.exe -fsSLo install.ps1 https://putnami.dev/install.ps1');
      expect(text).toContain('powershell -NoProfile -ExecutionPolicy Bypass -File install.ps1');
      expect(text).not.toMatch(/powershell(\.exe)?\s[^\n]*-c(ommand)?\s+(["']|&quot;|&#39;|&#x27;)?[^\n]*\birm\b/i);
    });

    it('should include breadcrumb navigation on doc pages', async () => {
      const res = await fetch(`${baseUrl}/docs/getting-started`);
      const html = await res.text();
      expect(html).toContain('Docs');
    });

    it('should render nested doc pages (framework section)', async () => {
      const res = await fetch(`${baseUrl}/docs/frameworks/typescript/overview`);
      expect(res.status).toBe(200);

      const html = await res.text();
      expect(html).toContain('<!DOCTYPE html>');
      expect(html).toContain('<h');
    });

    it('should render the TypeScript document storage page', async () => {
      const res = await fetch(`${baseUrl}/docs/frameworks/typescript/document`);
      expect(res.status).toBe(200);

      const html = await res.text();
      expect(html).toContain('@putnami/document');
      expect(html).toContain('Document Storage');
    });

    it('should render the TypeScript UI system page with its unsafe-content boundary', async () => {
      const res = await fetch(`${baseUrl}/docs/frameworks/typescript/ui`);
      expect(res.status).toBe(200);

      const html = await res.text();
      expect(html).toContain('@putnami/ui');
      expect(html).toContain('MarkdownRenderer');
      expect(html).toContain('authorization on the server');
    });

    it('should render the TypeScript web page with rendering and request security boundaries', async () => {
      const res = await fetch(`${baseUrl}/docs/frameworks/typescript/web`);
      expect(res.status).toBe(200);

      const html = await res.text();
      expect(html).toContain('@putnami/web');
      expect(html).toContain('Rendering modes');
      expect(html).toContain('Security defaults');
    });

    it('should reference document storage from the persistence how-to', async () => {
      const res = await fetch(`${baseUrl}/docs/how-to/add-persistence`);
      expect(res.status).toBe(200);

      const html = await res.text();
      expect(html).toContain('@putnami/document');
      expect(html).toContain('/docs/frameworks/typescript/document');
    });

    it('should redirect legacy framework URL paths to TypeScript docs', async () => {
      const res = await fetch(`${baseUrl}/docs/framework/overview`, { redirect: 'manual' });
      expect(res.status).toBe(301);
      expect(res.headers.get('location')).toBe('/docs/frameworks/typescript/overview');
    });

    it('should redirect legacy go-framework URL path to a Go docs page that exists', async () => {
      const res = await fetch(`${baseUrl}/docs/framework/go-framework`, { redirect: 'manual' });
      expect(res.status).toBe(301);
      // Go docs are published under /docs/frameworks/go; the previous target
      // redirected one 404 to another.
      expect(res.headers.get('location')).toBe('/docs/frameworks/go');

      const followed = await fetch(`${baseUrl}${res.headers.get('location')}`);
      expect(followed.status).toBe(200);
    });

    it('should render principles doc page', async () => {
      const res = await fetch(`${baseUrl}/docs/principles`);
      expect(res.status).toBe(200);

      const html = await res.text();
      expect(html).toContain('<!DOCTYPE html>');
    });

    it('should render tooling & workspace doc pages', async () => {
      const res = await fetch(`${baseUrl}/docs/tooling-%26-workspace/workspace`);
      expect(res.status).toBe(200);

      const html = await res.text();
      expect(html).toContain('<!DOCTYPE html>');
    });

    it('should include syntax-highlighted code blocks in doc pages', async () => {
      const res = await fetch(`${baseUrl}/docs/frameworks/typescript/overview`);
      const html = await res.text();
      // Shiki generates <pre> elements with highlighted code
      expect(html).toContain('<pre');
    });

    it('should include table of contents for doc pages', async () => {
      const res = await fetch(`${baseUrl}/docs/frameworks/typescript/overview`);
      const html = await res.text();
      // TOC items are rendered with heading ids
      expect(html).toContain('id="');
    });

    it('should include page meta tags for doc pages', async () => {
      const res = await fetch(`${baseUrl}/docs/getting-started`);
      const html = await res.text();
      expect(html).toContain('Putnami Docs');
    });
  });

  describe('SSR - HTML Document Structure', () => {
    it('should have a complete HTML document with head and body', async () => {
      const res = await fetch(`${baseUrl}/`);
      const html = await res.text();
      expect(html).toContain('<head>');
      expect(html).toContain('</head>');
      expect(html).toContain('<body>');
      expect(html).toContain('</body>');
    });

    it('should include styles in the head', async () => {
      const res = await fetch(`${baseUrl}/`);
      const html = await res.text();
      expect(html).toContain('<style');
      expect(html).toContain('@font-face');
    });
  });

  describe('SSR - Loader Data', () => {
    it('should load navigation items in the root layout', async () => {
      const res = await fetch(`${baseUrl}/`);
      const html = await res.text();
      // Root layout loader returns navItems, used in Navbar
      expect(html).toContain('Docs');
    });

    it('does not render the build version in the footer', async () => {
      // A deployed image runs under its release id (`cm_<hex>`), not a Putnami
      // release; the footer island fetches the latest version instead.
      const version = getBuildInfo()?.version;
      expect(version).toBeString();
      const res = await fetch(`${baseUrl}/`);
      const html = await res.text();
      expect(html).toContain('FSL-1.1-MIT');
      expect(html).not.toContain(version as string);
    });

    it('should load navigation items in docs layout', async () => {
      const res = await fetch(`${baseUrl}/docs`);
      const html = await res.text();
      // Docs layout loader returns navItems for sidebar
      expect(html).toContain('Frameworks');
    });

    it('should load and render markdown content for doc pages', async () => {
      const res = await fetch(`${baseUrl}/docs/getting-started`);
      const html = await res.text();
      // The loader reads markdown, converts to HTML, and provides it as loader data
      expect(html).toContain('<p');
    });
  });

  describe('Error Handling', () => {
    it('should return 404 for non-existent files', async () => {
      const res = await fetch(`${baseUrl}/nonexistent.txt`);
      expect(res.status).toBe(404);
    });

    it('should render the not-found page for non-existent doc pages', async () => {
      const res = await fetch(`${baseUrl}/docs/this-page-does-not-exist`);
      // The static catch-all renders live for unenumerated paths; the loader's
      // thrown 404 Response is caught and the not-found boundary renders.
      expect(res.status).toBe(404);
      const html = await res.text();
      expect(html).toContain('Page not found');
    });

    it('should render the not-found page for deeply nested non-existent doc paths', async () => {
      const res = await fetch(`${baseUrl}/docs/foo/bar/baz/nonexistent`);
      expect(res.status).toBe(404);
      const html = await res.text();
      expect(html).toContain('Page not found');
    });
  });

  describe('SSR - Response Headers', () => {
    it('should set content-type to text/html for SSR pages', async () => {
      const res = await fetch(`${baseUrl}/`);
      expect(res.headers.get('content-type')).toContain('text/html');
    });

    it('should set content-type to text/html for doc pages', async () => {
      const res = await fetch(`${baseUrl}/docs`);
      expect(res.headers.get('content-type')).toContain('text/html');
    });

    it('should set content-type to text/html for dynamic doc pages', async () => {
      const res = await fetch(`${baseUrl}/docs/getting-started`);
      expect(res.headers.get('content-type')).toContain('text/html');
    });
  });

  describe('SSG - Islands hydration', () => {
    it('home page is static (no full-page hydration data) with island markers', async () => {
      const res = await fetch(`${baseUrl}/`);
      const html = await res.text();
      expect(html).not.toContain('__staticRouterHydrationData');
      expect(html).toContain('<putnami-island');
    });

    it('docs overview page is static with island markers', async () => {
      const res = await fetch(`${baseUrl}/docs`);
      const html = await res.text();
      expect(html).not.toContain('__staticRouterHydrationData');
      expect(html).toContain('<putnami-island');
    });

    it('dynamic doc page is static with island markers', async () => {
      const res = await fetch(`${baseUrl}/docs/getting-started`);
      const html = await res.text();
      expect(html).not.toContain('__staticRouterHydrationData');
      expect(html).toContain('<putnami-island');
    });

    it('should reference the islands hydration runtime', async () => {
      const res = await fetch(`${baseUrl}/`);
      const html = await res.text();
      expect(html).toContain('<script');
      expect(html).toContain('/react-islands/islands');
    });
  });

  describe('SSR - Multiple Pages Consistency', () => {
    it('should render the root layout on all pages', async () => {
      const pages = ['/', '/docs', '/docs/getting-started'];
      for (const page of pages) {
        const res = await fetch(`${baseUrl}${page}`);
        const html = await res.text();
        // All pages should have the navbar from root layout
        expect(html).toContain('Putnami');
        // All pages should have the footer from root layout
        expect(html).toContain('footer');
      }
    });

    it('should include the ThemeProvider on all pages', async () => {
      const pages = ['/', '/docs'];
      for (const page of pages) {
        const res = await fetch(`${baseUrl}${page}`);
        const html = await res.text();
        // ThemeProvider renders CSS variables or theme attributes
        expect(html).toContain('<!DOCTYPE html>');
      }
    });
  });

  describe('SEO', () => {
    it('should serve robots.txt', async () => {
      const res = await fetch(`${baseUrl}/robots.txt`);
      expect(res.status).toBe(200);

      const text = await res.text();
      expect(text).toContain('User-agent');
      expect(text).toContain('Sitemap: https://putnami.dev/sitemap.xml');
    });

    it('should include canonical link on home page', async () => {
      const res = await fetch(`${baseUrl}/`);
      const html = await res.text();
      expect(html).toContain('rel="canonical"');
      expect(html).toContain('https://putnami.dev/');
    });

    it('should include canonical link on doc pages', async () => {
      const res = await fetch(`${baseUrl}/docs/getting-started`);
      const html = await res.text();
      expect(html).toContain('rel="canonical"');
      expect(html).toContain('https://putnami.dev/docs/getting-started');
    });

    it('should include JSON-LD structured data on home page', async () => {
      const res = await fetch(`${baseUrl}/`);
      const html = await res.text();
      expect(html).toContain('application/ld+json');
      expect(html).toContain('"@type":"WebSite"');
      expect(html).toContain('"name":"Putnami"');
    });

    it('should include JSON-LD structured data on doc pages', async () => {
      const res = await fetch(`${baseUrl}/docs/getting-started`);
      const html = await res.text();
      expect(html).toContain('application/ld+json');
      expect(html).toContain('"@type":"Article"');
      expect(html).toContain('"@type":"BreadcrumbList"');
    });

    it('should include Open Graph tags on home page', async () => {
      const res = await fetch(`${baseUrl}/`);
      const html = await res.text();
      expect(html).toContain('og:type');
      expect(html).toContain('og:title');
      expect(html).toContain('og:description');
      expect(html).toContain('og:url');
      expect(html).toContain('og:image');
    });

    it('should include Twitter Card tags on home page', async () => {
      const res = await fetch(`${baseUrl}/`);
      const html = await res.text();
      expect(html).toContain('twitter:card');
      expect(html).toContain('twitter:title');
      expect(html).toContain('twitter:description');
    });

    it('should set noindex on 404 page', async () => {
      const res = await fetch(`${baseUrl}/this-page-does-not-exist`);
      const html = await res.text();
      expect(html).toContain('noindex');
    });
  });
});
