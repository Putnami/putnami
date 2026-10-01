/**
 * Schema publication planning.
 *
 * Pure(-ish) logic for publishing the workspace's public JSON Schemas at the
 * path implied by each schema's own `$id`. Kept free of framework imports so it
 * is unit-testable in isolation; the `schemas` plugin is a thin wrapper that
 * resolves the workspace/project roots and writes the planned files.
 *
 *   $id "https://putnami.dev/schemas/putnami-project.json"
 *     → public/schemas/putnami-project.json
 *   $id "https://putnami.dev/schemas/protocol/runtime/event.json"
 *     → public/schemas/protocol/runtime/event.json
 *
 * Keying off `$id` (not directory layout or file name) keeps publishing robust
 * across protocol reorganizations: a schema can move on disk, or be renamed,
 * without changing its canonical URL.
 */
import { Glob } from 'bun';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

/** Canonical origin every published Putnami schema declares in its `$id`. */
export const SCHEMA_ORIGIN = 'https://putnami.dev/schemas/';

export interface SchemasConfig {
  /**
   * Glob patterns (workspace-root-relative) locating candidate schema files.
   * Files whose `$id` is not under {@link SCHEMA_ORIGIN} are ignored.
   */
  sources?: string[];
  /**
   * Glob patterns (workspace-root-relative) to exclude from discovery. Use this
   * to resolve an intentional `$id` collision by dropping the non-canonical
   * source, rather than letting the build fail.
   */
  exclude?: string[];
}

/** A discovered candidate: its workspace-relative path and declared `$id`. */
export interface SchemaSource {
  file: string;
  id: string | undefined;
}

/** A resolved publication: which file lands at which `public/schemas`-relative URL path. */
export interface SchemaPublication {
  file: string;
  urlPath: string;
}

/** Default discovery globs, relative to the workspace root. */
export const DEFAULT_SOURCES = ['protocols/*/schemas/*.json'];

/**
 * Map a schema `$id` to its publish path under `public/schemas/`, or `null`
 * when the `$id` is missing or does not belong to {@link SCHEMA_ORIGIN}.
 */
export function publishPathForId(id: string | undefined): string | null {
  if (!id?.startsWith(SCHEMA_ORIGIN)) return null;
  const rel = id.slice(SCHEMA_ORIGIN.length);
  // Reject empty, absolute, or traversing paths — a malformed `$id` must never
  // be able to write outside public/schemas/.
  if (!rel || rel.startsWith('/') || rel.split('/').includes('..')) return null;
  return rel;
}

/** Parse a schema document's `$id`, throwing a clear error on invalid JSON. */
export function parseSchemaId(text: string, file: string): string | undefined {
  let doc: unknown;
  try {
    doc = JSON.parse(text);
  } catch (error) {
    throw new Error(`schemas: ${file} is not valid JSON: ${(error as Error).message}`);
  }
  const id = (doc as { $id?: unknown }).$id;
  return typeof id === 'string' ? id : undefined;
}

/**
 * Resolve discovered sources into a publication plan. Throws on an unresolved
 * `$id` collision (the same canonical URL declared by two different files) so a
 * duplicate cannot silently shadow another schema — exclude one source or give
 * it a distinct `$id` to resolve it deliberately.
 */
export function planSchemaPublications(sources: SchemaSource[]): SchemaPublication[] {
  const byUrlPath = new Map<string, string>();
  for (const { file, id } of sources) {
    const urlPath = publishPathForId(id);
    if (!urlPath) continue;
    const existing = byUrlPath.get(urlPath);
    if (existing && existing !== file) {
      throw new Error(
        `schemas: duplicate $id "${SCHEMA_ORIGIN}${urlPath}" declared by both ` +
          `"${existing}" and "${file}". Each published schema must have a unique $id; ` +
          `exclude one source or give it a distinct $id.`,
      );
    }
    byUrlPath.set(urlPath, file);
  }
  return [...byUrlPath].map(([urlPath, file]) => ({ file, urlPath }));
}

/**
 * Discover candidate schema files under `config.sources`, honoring
 * `config.exclude`, and read each one's `$id`. Read-only filesystem access.
 */
export function discoverSchemaSources(workspaceRoot: string, config: SchemasConfig = {}): SchemaSource[] {
  const sourceGlobs = config.sources ?? DEFAULT_SOURCES;
  const excludeGlobs = (config.exclude ?? []).map((pattern) => new Glob(pattern));

  const seen = new Set<string>();
  const sources: SchemaSource[] = [];
  for (const pattern of sourceGlobs) {
    for (const rel of new Glob(pattern).scanSync({ cwd: workspaceRoot })) {
      if (seen.has(rel) || excludeGlobs.some((glob) => glob.match(rel))) continue;
      seen.add(rel);
      const text = readFileSync(join(workspaceRoot, rel), 'utf8');
      sources.push({ file: rel, id: parseSchemaId(text, rel) });
    }
  }
  return sources;
}
