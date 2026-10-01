import { describe, expect, it } from 'bun:test';
import { Glob } from 'bun';
import { readFileSync } from 'node:fs';
import { specTest } from '@putnami/spectest';
import { fileExists, joinPath } from '@putnami/utils';
import { getDocsPaths, getDocsRoot } from '../src/lib/docs/navigation.server';

/**
 * Internal-link guard over every published documentation section.
 *
 * A published page that links to `/docs/how-to/create-an-extension` or to a
 * repository path like `/typescript/samples/03-web` renders fine and 404s for
 * the reader — nothing else in the build notices. This suite resolves every
 * site-internal link against the same path map the docs loader uses, so a
 * renamed or deleted page fails here instead of in production.
 *
 * Scope is the whole published tree, including the sections copied from
 * `tooling/doc` and `<language>/doc/framework` and the lock-pinned content
 * bundle. It was originally narrowed to this project's own `doc/`, on the
 * reasoning that failing here on content this project may not edit would only
 * move the breakage. That exclusion hid 64 dead links: 59 in
 * `go/doc/framework/**` using the `/docs/go/frameworks/<page>` prefix instead
 * of the served `/docs/frameworks/go/<page>`, and 5 in
 * `typescript/doc/framework/**`. Every one of them 404'd in production while
 * this suite reported green, and two verticals closed without fixing them
 * because nothing failed. The reader does not care which project owns a page,
 * so the guard now covers what the site actually serves; a broken cross-link
 * fails the build that publishes it.
 */

/** Routes the site serves that are not docs pages. */
const NON_DOCS_ROUTES = new Set([
  '/',
  '/docs',
  '/install.sh',
  '/install.ps1',
  '/LICENSE.md',
  '/llms.txt',
  '/sitemap.xml',
  '/doc-markdown',
  '/robots.txt',
  '/favicon.ico',
]);

interface DocLink {
  file: string;
  target: string;
}

function publishedDocFiles(docsRoot: string): string[] {
  return [...new Glob('**/*.md').scanSync({ cwd: docsRoot })];
}

/** Markdown inline links, excluding images and reference definitions. */
function internalLinks(docsRoot: string, files: string[]): DocLink[] {
  const links: DocLink[] = [];
  for (const file of files) {
    const source = readFileSync(joinPath(docsRoot, file), 'utf8');
    for (const match of source.matchAll(/(!?)\[[^\]]*\]\(([^)\s]+)[^)]*\)/g)) {
      if (match[1] === '!') continue;
      const target = match[2] as string;
      // Site-internal links only: absolute URLs and pure fragments are out of
      // scope for this guard.
      if (!target.startsWith('/')) continue;
      links.push({ file, target });
    }
  }
  return links;
}

function withoutFragment(target: string): string {
  const hash = target.indexOf('#');
  return hash === -1 ? target : target.slice(0, hash);
}

describe('documentation internal links', () => {
  const docsRoot = getDocsRoot();
  const available = fileExists(docsRoot);

  it('has a generated docs tree to check', () => {
    // The generate phase populates .gen/public/docs before tests run. A missing
    // tree would silently make every assertion below vacuous.
    expect(available).toBe(true);
    expect(publishedDocFiles(docsRoot).length).toBeGreaterThan(0);
  });

  it('resolves every /docs/ link to a page the loader can serve', async () => {
    const splats = new Set(await getDocsPaths());
    const broken: string[] = [];

    for (const link of internalLinks(docsRoot, publishedDocFiles(docsRoot))) {
      const path = withoutFragment(link.target);
      if (!path.startsWith('/docs/')) continue;
      const splat = path.slice('/docs/'.length).replace(/\/+$/, '');
      if (splat === '' || splats.has(splat)) continue;
      broken.push(`${link.file} -> ${link.target}`);
    }

    expect(broken).toEqual([]);
  });

  it('never links to a repository path the site does not serve', () => {
    // `/typescript/samples/03-web` and `/samples` are repository directories,
    // not site routes. Linking one produces a 404 for every reader.
    const offenders: string[] = [];

    for (const link of internalLinks(docsRoot, publishedDocFiles(docsRoot))) {
      const path = withoutFragment(link.target);
      if (path.startsWith('/docs/') || NON_DOCS_ROUTES.has(path)) continue;
      if (path.startsWith('/dl/') || path.startsWith('/schemas/') || path.startsWith('/assets/')) continue;
      offenders.push(`${link.file} -> ${link.target}`);
    }

    expect(offenders).toEqual([]);
  });

  it('publishes the generated support page as a resolvable route', async () => {
    const splats = new Set(await getDocsPaths());
    expect(splats.has('support')).toBe(true);
  });

  it('keeps every documented section reachable from an index page', async () => {
    // Each top-level section must expose an index the nav can point at, or the
    // section exists in the tree but has no front door.
    const splats = new Set(await getDocsPaths());
    for (const section of ['getting-started', 'concepts', 'how-to', 'principles', 'frameworks', 'support']) {
      expect(splats.has(section)).toBe(true);
    }
  });

  specTest(
    'publishes the complete cross-cutting system model in the docs tree',
    {
      feature: 'putnami-dev/public-documentation',
      requirement: 'system-model-is-published',
      check: 'system-model-pages-are-published',
    },
    async () => {
      const splats = new Set(await getDocsPaths());
      for (const path of ['why', 'concepts', 'protocols', 'agents']) {
        expect(splats.has(path)).toBe(true);
      }

      expect(readFileSync(joinPath(docsRoot, '00-why', 'index.md'), 'utf8')).toContain('What follows from the bet');
      expect(readFileSync(joinPath(docsRoot, '02-concepts', 'index.md'), 'utf8')).toContain(
        'the layers that make the system legible',
      );
      expect(readFileSync(joinPath(docsRoot, '05-protocols', 'index.md'), 'utf8')).toContain(
        'A protocol is a wire contract',
      );
      expect(readFileSync(joinPath(docsRoot, '06-agents', 'index.md'), 'utf8')).toContain('It can **operate** it');
    },
  );
});
