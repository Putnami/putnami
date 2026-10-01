/**
 * Search index build plugin.
 *
 * Generates a search index from markdown documentation files during
 * the Application build lifecycle. The index is written only to
 * .gen/public/search/index.json: the build pipeline ships it from there, and the
 * content-overlay `GET /search/index.json` route serves it from there.
 *
 * It is never mirrored into the source `public/` folder. The staticFiles plugin
 * declares HTTP route facts from what that folder holds when it scans, during
 * generate(), and this index is only written later, in postGenerate(). A mirror
 * there would reach the route inventory only on a tree an earlier build left it
 * in, so the emitted inventory would depend on the tree's history instead of the
 * build's inputs. The site declares the one file this plugin serves
 * (`exact /search/index.json`, see public-surface.plugin.ts).
 *
 * @example
 * ```typescript
 * application()
 *   .use(searchIndex({ docsDir: 'doc' }))
 * ```
 */
import { mkdirSync, rmSync, writeFileSync } from 'node:fs';
import type { GenerateResult, Plugin } from '@putnami/application';
import { getProjectRoot, joinPath } from '@putnami/utils';
import { buildSearchIndex } from '../lib/search/build-index';

interface SearchIndexConfig {
  /** Directory containing markdown docs, relative to project root. */
  docsDir: string;
}

/**
 * Remove the `public/search` mirror that earlier builds wrote for dev serve.
 * Nothing writes it any more; this clears the copy an older build left behind.
 */
export function removeSearchSourceMirror(projectRoot: string): void {
  rmSync(joinPath(projectRoot, 'public', 'search'), { recursive: true, force: true });
}

class SearchIndexPlugin implements Plugin {
  constructor(private config: SearchIndexConfig) {}

  // Synchronous and registered before staticFiles: the framework calls every
  // generate() in registration order, so this removal finishes before the
  // staticFiles scan reads public/.
  generate(): GenerateResult {
    removeSearchSourceMirror(getProjectRoot());
    return {};
  }

  // postGenerate, not generate: the generate() pass runs every plugin in
  // parallel, and this index reads what the others WRITE (content bundles,
  // the support catalog, prerendered docs). postGenerate is the framework's
  // ordering barrier — by the time it runs, every generate() output is on
  // disk, so a cold build indexes the complete published tree instead of
  // whatever happened to exist when the race resolved.
  async postGenerate(): Promise<GenerateResult> {
    const projectRoot = getProjectRoot();
    const docsRoot = joinPath(projectRoot, this.config.docsDir);

    const index = buildSearchIndex(docsRoot);

    const genOutputDir = joinPath(projectRoot, '.gen', 'public', 'search');
    mkdirSync(genOutputDir, { recursive: true });

    const outputPath = joinPath(genOutputDir, 'index.json');
    writeFileSync(outputPath, JSON.stringify(index));

    // Return as assets so the build pipeline copies them to dist/
    return {
      assets: {
        'public/search/index.json': outputPath,
      },
    };
  }
}

export function searchIndex(config: SearchIndexConfig): SearchIndexPlugin {
  return new SearchIndexPlugin(config);
}
