import { loader } from '@putnami/web';
import { type SupportStatus, statusOf } from '../../lib/support/catalog';
import { bundledSupportCatalog } from '../../lib/support/catalog.bundled';
import { TOOL_ORDER, TOOLS } from '../../lib/tools';

/** Reviewed support status per tool id; absent when nothing is classified. */
export interface DocsHubData {
  toolStatus: Record<string, SupportStatus | undefined>;
}

/**
 * Resolve each documented surface's status from the reviewed support catalog.
 *
 * Static: runs at build for the pre-rendered docs hub, and again in the
 * deployed server whenever the hub's ISR entry is refreshed. It reads no
 * request data and no file: the catalog is compiled into the server, because
 * the image has no workspace to read it from. Doing it here rather than in
 * `lib/tools.ts` keeps that module client-safe (islands import it) and keeps
 * the status itself in exactly one reviewed home.
 */
export default loader()
  .static()
  .handle((): DocsHubData => {
    const catalog = bundledSupportCatalog();
    const toolStatus: Record<string, SupportStatus | undefined> = {};
    for (const id of TOOL_ORDER) {
      const subject = TOOLS[id]?.supportSubject;
      toolStatus[id] = subject ? statusOf(catalog, subject) : undefined;
    }
    return { toolStatus };
  });
