/**
 * Site-content bundle plugin (baked consumption).
 *
 * At generate time, materializes every bundle pinned by the committed
 * `content.lock.json` into `.gen/public/<mount>` (production build output) and
 * `public/<mount>` (local dev static serve), registering each written file as
 * a generate asset so the build pipeline ships it (schemas-plugin precedent —
 * a file written only to `.gen` but not registered can be masked by the serve
 * fallback yet 404 in the packaged image).
 *
 * Everything is fetch-by-digest: the lock is the only pointer, `content bump`
 * (scripts/content-bump.ts) is the only place a channel is ever resolved, and
 * the lock participates in the generate cache key via `putnami.json`
 * `filePatterns`. With an empty lock the plugin is a clean no-op.
 *
 * Mount convention: bundles mount ordered docs sections, e.g.
 * `/docs/<nn-section>` — the nav tree scanned from `.gen/public/docs` orders
 * them by the same `NN-` prefix rule as site-local sections. A mount may never
 * overlap (equal/parent/child) a site-local `generate.assets` target or
 * another bundle's mount.
 */
import type { GenerateResult, Plugin } from '@putnami/application';
import { getProjectRoot, getWorkspaceRoot } from '@putnami/utils';
import { type MaterializeOptions, materializeContentBundles } from '../lib/content/materialize';

export type ContentBundlesConfig = Partial<Omit<MaterializeOptions, 'projectRoot' | 'workspaceRoot'>>;

class ContentBundlePlugin implements Plugin {
  constructor(private config: ContentBundlesConfig) {}

  async generate(): Promise<GenerateResult> {
    const result = await materializeContentBundles({
      projectRoot: getProjectRoot(),
      workspaceRoot: getWorkspaceRoot(),
      ...this.config,
    });
    return Object.keys(result.assets).length > 0 ? { assets: result.assets } : {};
  }
}

export function contentBundles(config: ContentBundlesConfig = {}): ContentBundlePlugin {
  return new ContentBundlePlugin(config);
}
