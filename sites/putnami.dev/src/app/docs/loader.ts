import { loader } from '@putnami/web';
import { type SupportStatus, statusOf } from '../../lib/support/catalog';
import { readSupportCatalog } from '../../lib/support/catalog.server';
import { TOOL_ORDER, TOOLS } from '../../lib/tools';

/** Reviewed support status per tool id; absent when nothing is classified. */
export interface DocsHubData {
  toolStatus: Record<string, SupportStatus | undefined>;
}

/**
 * Resolve each documented surface's status from the reviewed support catalog.
 *
 * Static: runs at build for the pre-rendered docs hub and reads only the
 * workspace-root catalog — no request data. Doing it here rather than in
 * `lib/tools.ts` keeps that module client-safe (islands import it) and keeps
 * the status itself in exactly one reviewed home.
 */
export default loader()
  .static()
  .handle((): DocsHubData => {
    const catalog = readSupportCatalog();
    const toolStatus: Record<string, SupportStatus | undefined> = {};
    for (const id of TOOL_ORDER) {
      const subject = TOOLS[id]?.supportSubject;
      toolStatus[id] = subject ? statusOf(catalog, subject) : undefined;
    }
    return { toolStatus };
  });
