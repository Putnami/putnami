/**
 * `putnami.documentation-tree.v1` — the carrier the five documentation snapshot
 * contracts name.
 *
 * It is not a published wire schema. It names the thing one `generate.assets`
 * entry copies: the markdown tree under one repository source root, addressed by
 * a deterministic digest over its sorted `(relative path, content digest)`
 * pairs. Two checkouts of the same tree produce the same version; one changed
 * byte produces a different one; a cold build and a warm build produce the same
 * one, because nothing outside the file contents and their paths reaches the
 * hash.
 *
 * The digest deliberately does NOT cover the source root. A version is the
 * identity of a producer STATE, and moving a tree does not change the
 * documentation it holds. What binds a version to a section is the declared
 * import, not the hash.
 */
import { Glob } from 'bun';
import { createHash } from 'node:crypto';
import { readFileSync, statSync } from 'node:fs';
import { fileExists, joinPath } from '@putnami/utils';

/** The transport contract the five documentation snapshot imports declare. */
export const DOCUMENTATION_TREE_CONTRACT = 'putnami.documentation-tree.v1';

/** The glob that decides which files a documentation tree is made of. */
export const DOCUMENTATION_TREE_PATTERN = '**/*.md';

/** One published documentation file: its path inside the source root, and its content digest. */
export interface DocumentationFile {
  /** POSIX path relative to the source root. */
  readonly path: string;
  /** `sha256:<hex>` over the file's exact bytes. */
  readonly digest: string;
}

/** The projected copy a documentation snapshot holds: one section's whole file inventory. */
export interface DocumentationTree {
  /** The workspace-relative source root this inventory was read from. */
  readonly root: string;
  /** Every markdown file under the root, ordered by the UTF-8 bytes of its path. */
  readonly files: readonly DocumentationFile[];
}

/** One producer state: its exact version and the inventory that version addresses. */
export interface ObservedDocumentationTree {
  /** The content-addressed version, `sha256:<hex>`. */
  readonly version: string;
  readonly tree: DocumentationTree;
}

/**
 * Reads one source root's current state, or reports its absence.
 *
 * Injected so a test can simulate a deleted source without deleting a
 * repository file, and so the enforcement path stays a pure function of what
 * the reader returned.
 */
export type DocumentationTreeReader = (root: string) => ObservedDocumentationTree | undefined;

/** `sha256:<hex>` over exact bytes. */
export function contentDigest(content: Uint8Array | string): string {
  return `sha256:${createHash('sha256').update(content).digest('hex')}`;
}

/**
 * Order two paths by their UTF-8 bytes.
 *
 * Byte order rather than JavaScript's `<`, which compares UTF-16 code units and
 * would order a supplementary character differently from every other Putnami
 * canonicalizer. A documentation path is ASCII today; the comparator is stated
 * rather than assumed because a version that depended on the locale would stop
 * being reproducible the day one is not.
 */
export function compareByteOrder(left: string, right: string): number {
  return Buffer.compare(Buffer.from(left, 'utf8'), Buffer.from(right, 'utf8'));
}

/**
 * The version a tree is addressed by.
 *
 * The hash covers the carrier name (so a future carrier revision cannot collide
 * with this one) then one `path\0digest\n` line per file, in byte order. Same
 * pairs in, same version out.
 */
export function documentationTreeVersion(files: readonly DocumentationFile[]): string {
  const hash = createHash('sha256');
  hash.update(`${DOCUMENTATION_TREE_CONTRACT}\n`);
  for (const file of [...files].sort((left, right) => compareByteOrder(left.path, right.path))) {
    hash.update(`${file.path}\0${file.digest}\n`);
  }
  return `sha256:${hash.digest('hex')}`;
}

/** Build an observed state from an already-collected inventory. */
export function observeDocumentationTree(root: string, files: readonly DocumentationFile[]): ObservedDocumentationTree {
  const ordered = [...files].sort((left, right) => compareByteOrder(left.path, right.path));
  return { version: documentationTreeVersion(ordered), tree: { root, files: ordered } };
}

/**
 * Read one source root from the workspace.
 *
 * Returns `undefined` for a root that does not exist, is not a directory, or
 * holds no markdown at all. All three publish an empty documentation section,
 * which is the failure this contract exists to close, so all three are ABSENT
 * rather than "an empty snapshot" — an empty snapshot would be a successful
 * observation of nothing.
 */
export function readDocumentationTree(workspaceRoot: string, root: string): ObservedDocumentationTree | undefined {
  const absolute = joinPath(workspaceRoot, root.replace(/^\/+/, ''));
  if (!fileExists(absolute) || !statSync(absolute).isDirectory()) return undefined;

  const files: DocumentationFile[] = [];
  for (const relative of new Glob(DOCUMENTATION_TREE_PATTERN).scanSync({ cwd: absolute })) {
    const path = relative.replaceAll('\\', '/');
    files.push({ path, digest: contentDigest(readFileSync(joinPath(absolute, path))) });
  }
  if (files.length === 0) return undefined;
  return observeDocumentationTree(root, files);
}
