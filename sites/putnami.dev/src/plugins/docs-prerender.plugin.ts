/**
 * Docs prerender plugin — and the enforcement point of the `public-docs`
 * domain's documentation snapshot contracts.
 *
 * At build time, walks all markdown sources contributing to `public/docs/` and
 * emits a sibling `<file>.md.rendered.json` containing the highlighted HTML and
 * TOC. The runtime loader prefers this cached output, so syntax highlighting
 * (shiki) never has to run in production — shiki becomes a devDependency.
 *
 * Source mapping is read from `putnami.json`'s `generate.assets` config; any
 * entry whose `to` starts with `public/docs/` is considered a docs source.
 *
 * # Why the contracts are enforced here
 *
 * This is the pass that actually aggregates the documentation: it is the one
 * place that walks every declared source root and turns its markdown into the
 * published section. A component that existed only to emit an evidence row
 * would attest to nothing.
 *
 * So for the five REPOSITORY-OWNED sources (the ones another domain writes),
 * this plugin observes the source tree into its declared `Snapshot` and then
 * asks `latest()` for the file inventory it publishes. A source that is gone was
 * never attached, `latest()` applies the declared `onMissing: fail-closed`, and
 * the build fails with `DarcError('missing')` instead of the extension's
 * `generate asset source not found, skipping` warning and a section that
 * silently disappears.
 *
 * The site's own `/sites/putnami.dev/doc` and any single-file copy stay on the
 * plain filesystem walk: a domain does not import from itself.
 */
import { Glob } from 'bun';
import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname } from 'node:path';
import type { GenerateResult, Plugin } from '@putnami/application';
import { fileExists, getProjectRoot, getWorkspaceRoot, joinPath } from '@putnami/utils';
import { type PublicDocumentationAccess, publicDocumentationAccess } from '../lib/darc/public-docs';

// Loaded lazily so the build-time module (and its shiki dep) is never bundled
// into the production server. The specifier is read from a variable so the
// bundler can't statically resolve and inline it.
type MarkdownBuild = typeof import('../lib/markdown-build');
const MARKDOWN_BUILD_MODULE = '../lib/markdown-build';
async function loadMarkdownBuild(): Promise<MarkdownBuild> {
  return (await import(MARKDOWN_BUILD_MODULE)) as MarkdownBuild;
}

interface AssetEntry {
  from: string;
  to: string;
}

interface PutnamiProject {
  options?: {
    generate?: {
      assets?: AssetEntry[];
    };
  };
}

const DOCS_PREFIX = 'public/docs';

async function readDocsAssets(projectRoot: string): Promise<AssetEntry[]> {
  const configPath = joinPath(projectRoot, 'putnami.json');
  const config = JSON.parse(await Bun.file(configPath).text()) as PutnamiProject;
  return (config.options?.generate?.assets ?? []).filter((entry) => entry.to.startsWith(DOCS_PREFIX));
}

/** One markdown file to prerender: where to read it, and its path inside the section. */
export interface PrerenderSource {
  source: string;
  relative: string;
}

/**
 * The unmodelled walk: this domain's own documentation, and single-file copies.
 *
 * A missing source here still publishes nothing, exactly as before. It is not a
 * cross-domain fact, so there is no contract to enforce and nothing to fail.
 */
function expandLocalEntry(workspaceRoot: string, entry: AssetEntry): PrerenderSource[] {
  const sourcePath = joinPath(workspaceRoot, entry.from);
  if (!fileExists(sourcePath)) return [];

  if (entry.from.toLowerCase().endsWith('.md')) {
    return [{ source: sourcePath, relative: '' }];
  }

  const files = [...new Glob('**/*.md').scanSync({ cwd: sourcePath })];
  return files.map((rel) => ({ source: joinPath(sourcePath, rel), relative: rel }));
}

/**
 * Resolve one asset entry to the files that will be published.
 *
 * For a declared source, the answer comes out of the snapshot: `resolve()` reads
 * `latest()` under the declared contract, so an absent source raises
 * `DarcError('missing')` here rather than yielding an empty list.
 */
export function prerenderSourcesFor(
  access: PublicDocumentationAccess,
  workspaceRoot: string,
  entry: AssetEntry,
): PrerenderSource[] {
  if (!access.declares(entry.from)) return expandLocalEntry(workspaceRoot, entry);

  const record = access.resolve(entry.from);
  const sourcePath = joinPath(workspaceRoot, entry.from);
  return record.value.files.map((file) => ({
    source: joinPath(sourcePath, file.path),
    relative: file.path,
  }));
}

function destinationFor(
  projectRoot: string,
  entry: AssetEntry,
  relative: string,
): { genPath: string; assetKey: string } {
  const destSubpath = relative ? `${entry.to}/${relative}` : entry.to;
  return {
    genPath: joinPath(projectRoot, '.gen', `${destSubpath}.rendered.json`),
    assetKey: `${destSubpath}.rendered.json`,
  };
}

async function prerenderOne(mb: MarkdownBuild, source: string): Promise<string> {
  const content = await Bun.file(source).text();
  const [html, toc] = await Promise.all([
    mb.renderMarkdownWithHighlighting(content),
    Promise.resolve(mb.extractHeadings(content)),
  ]);
  return JSON.stringify({ html, toc });
}

class DocsPrerenderPlugin implements Plugin {
  constructor(readonly access: PublicDocumentationAccess) {}

  async generate(): Promise<GenerateResult> {
    const projectRoot = getProjectRoot();
    const workspaceRoot = getWorkspaceRoot();
    const entries = await readDocsAssets(projectRoot);

    // Observe every declared source root once, then resolve what each section
    // publishes. Both halves run before the renderer is even loaded, so a
    // missing source fails the build before it does any work or writes a
    // partial docs tree.
    this.access.observeAll();
    const expanded = entries.map((entry) => ({
      entry,
      files: prerenderSourcesFor(this.access, workspaceRoot, entry),
    }));

    const assets: Record<string, string> = {};
    const mb = await loadMarkdownBuild();

    await Promise.all(
      expanded.flatMap(({ entry, files }) =>
        files.map(async ({ source, relative }) => {
          const json = await prerenderOne(mb, source);
          const { genPath, assetKey } = destinationFor(projectRoot, entry, relative);
          mkdirSync(dirname(genPath), { recursive: true });
          writeFileSync(genPath, json);
          assets[assetKey] = genPath;
        }),
      ),
    );

    return { assets };
  }
}

/**
 * Build the prerender plugin over the domain's enforced contracts.
 *
 * The caller passes the same {@link PublicDocumentationAccess} it registers with
 * `DarcPlugin`, so the components that produce the evidence rows are the
 * components that governed the build.
 */
export function prerenderDocs(access: PublicDocumentationAccess = publicDocumentationAccess()): DocsPrerenderPlugin {
  return new DocsPrerenderPlugin(access);
}
