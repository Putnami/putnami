import { readFileSync } from 'node:fs';
import { type HttpRouteProof, readHttpRouteProves } from '@putnami/application/http-routes';

/**
 * A page's declaration of the feature requirements it proves, the keys a test
 * binds with minus `check`. `at` is a concrete path that renders the page with
 * fixture data:
 *
 * ```ts
 * export const proves = [
 *   { feature: 'acme/change-review', requirement: 'tabs-in-mission-order', at: '/w/acme/pr-24/code' },
 * ] satisfies PageProof[];
 * ```
 */
export type PageProof = HttpRouteProof;

// A module that declares `proves` in its own source: an exported binding, or
// a name an export list exports as `proves`. A `proves` that a page re-exports
// with `export *` is read only when the page imports.
const DECLARES_PROVES =
  /\bexport\s+(?:const|let|var)\s+proves\b|\bexport\s*\{[^}]*(?:\bas\s+proves|\bproves(?!\s+as\b))\s*[,}]/;

/**
 * Read the `proves` export of the page module at `absFilePath`. A page without
 * one proves nothing. A malformed export, or a page that declares one but
 * cannot be imported, throws, so the build fails rather than drop it.
 * `displayPath` names the page in the error.
 */
export async function readPageProves(absFilePath: string, displayPath: string): Promise<PageProof[] | undefined> {
  let mod: Record<string, unknown>;
  try {
    mod = (await import(absFilePath)) as Record<string, unknown>;
  } catch (cause) {
    if (!DECLARES_PROVES.test(readFileSync(absFilePath, 'utf8'))) return undefined;
    throw new Error(
      `${displayPath}: cannot read its proves export, the page module failed to import: ${cause instanceof Error ? cause.message : String(cause)}`,
    );
  }
  if (mod['proves'] === undefined) return undefined;
  const { proves, errors } = readHttpRouteProves(mod['proves']);
  if (errors.length > 0) throw new Error(`${displayPath}: invalid proves export:\n  ${errors.join('\n  ')}`);
  return proves;
}
