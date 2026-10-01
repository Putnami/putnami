/**
 * Build-time docs path enumeration.
 *
 * The web generate hook asks the page for its static paths before the
 * application generate hook runs. Materialize the lock-pinned bundles and write
 * the generated support page here so a cold build sees the same docs tree as
 * every later build.
 */
import { getProjectRoot, getWorkspaceRoot } from '@putnami/utils';
import { materializeContentBundles } from '../content/materialize';
import { publishSupportPage } from '../support/publish';
import { getDocsPaths, invalidateNavCache } from './navigation.server';

export interface DocsStaticPathDependencies {
  materialize: typeof materializeContentBundles;
  publishSupport: typeof publishSupportPage;
  invalidate: typeof invalidateNavCache;
  enumerate: typeof getDocsPaths;
  projectRoot: string;
  workspaceRoot: string;
}

export async function getMaterializedDocsPaths(overrides: Partial<DocsStaticPathDependencies> = {}): Promise<string[]> {
  const projectRoot = overrides.projectRoot ?? getProjectRoot();
  const workspaceRoot = overrides.workspaceRoot ?? getWorkspaceRoot();

  await (overrides.materialize ?? materializeContentBundles)({ projectRoot, workspaceRoot });
  (overrides.publishSupport ?? publishSupportPage)({ projectRoot, workspaceRoot });

  // A path reader may already have populated the process-local cache before
  // this callback. Always rescan the now-complete tree.
  (overrides.invalidate ?? invalidateNavCache)();
  return (overrides.enumerate ?? getDocsPaths)();
}
