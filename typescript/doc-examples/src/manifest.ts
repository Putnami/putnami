import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative, sep } from 'node:path';

/**
 * Names the markdown documents whose examples are checked, by slash-separated
 * glob patterns relative to the workspace root.
 */
export interface Manifest {
  include: string[];
  exclude?: string[];
}

/** Reads a manifest file. */
export function readManifest(file: string): Manifest {
  const manifest = JSON.parse(readFileSync(file, 'utf8')) as Manifest;
  if (!Array.isArray(manifest.include) || manifest.include.length === 0) {
    throw new Error(`${file}: include names no document`);
  }
  return manifest;
}

/**
 * Returns the documents under root the manifest names, as sorted
 * slash-separated paths relative to root. An include pattern that matches no
 * document is an error, so a moved document does not leave the check silently.
 */
export function documents(manifest: Manifest, root: string): string[] {
  const found = new Set<string>();
  const excludes = (manifest.exclude ?? []).map((pattern) => new Bun.Glob(pattern));
  for (const pattern of manifest.include) {
    const glob = new Bun.Glob(pattern);
    let matched = false;
    for (const file of walk(join(root, staticPrefix(pattern)))) {
      const rel = relative(root, file).split(sep).join('/');
      if (!glob.match(rel)) continue;
      matched = true;
      if (!excludes.some((exclude) => exclude.match(rel))) found.add(rel);
    }
    if (!matched) throw new Error(`the pattern "${pattern}" matches no document`);
  }
  return [...found].sort();
}

function staticPrefix(pattern: string): string {
  const segments = pattern.split('/');
  const prefix: string[] = [];
  for (const segment of segments.slice(0, -1)) {
    if (/[*?[{]/.test(segment)) break;
    prefix.push(segment);
  }
  return prefix.join('/');
}

function* walk(path: string): Generator<string> {
  let stat: ReturnType<typeof statSync>;
  try {
    stat = statSync(path);
  } catch {
    return;
  }
  if (!stat.isDirectory()) {
    yield path;
    return;
  }
  for (const entry of readdirSync(path, { withFileTypes: true })) {
    if (entry.isDirectory() && (entry.name === 'node_modules' || entry.name.startsWith('.'))) continue;
    yield* walk(join(path, entry.name));
  }
}
