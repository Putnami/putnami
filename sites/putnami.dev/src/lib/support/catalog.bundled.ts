/**
 * The reviewed support catalog, compiled into the server.
 *
 * Code that runs while the site serves reads the catalog from here, never from
 * the workspace. The deployed image holds the compiled server and its `.gen`
 * output only: it has no workspace root and no `putnami.support.json`. An ISR
 * page re-runs its loader in that image when its TTL lapses or its tag is
 * revalidated, so a loader that read the file there would fail every refresh.
 *
 * The import inlines the catalog the build compiled, so the statuses served are
 * the ones the build published. The build validates the exact file first: the
 * generated support page fails the build on an invalid catalog (see
 * `publish.ts`). Parsing again here keeps one typed shape for every reader.
 *
 * Server-only: never import it from a client bundle.
 */
import reviewedCatalog from '../../../../../putnami.support.json';
import { type SupportCatalog, parseSupportCatalog } from './catalog';

let parsed: SupportCatalog | undefined;

/** The reviewed catalog the server was compiled with. */
export function bundledSupportCatalog(): SupportCatalog {
  parsed ??= parseSupportCatalog(JSON.stringify(reviewedCatalog));
  return parsed;
}
