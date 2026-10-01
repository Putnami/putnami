/**
 * Schema publishing plugin.
 *
 * Publishes the workspace's public JSON Schemas — the per-domain protocol
 * schemas under `protocols/<domain>/schemas/*.json` — at the path
 * implied by each schema's own `$id` (see `../lib/schemas/publish`). ~196
 * config files in the workspace `$schema`-reference these URLs, so a broken
 * layout silently 404s every editor/CI validation — this plugin (and its
 * tests) turn that into a build-time failure instead of a silent one.
 *
 * Replaces the legacy `{ from: "/tooling/cli/schemas", to: "public/schemas" }`
 * generate asset, whose source directory was deleted when schemas moved into
 * `protocols/*` and which silently published nothing thereafter.
 *
 * @example
 * ```typescript
 * application()
 *   .use(schemas({ sources: ['protocols/*\/schemas/*.json'] }))
 * ```
 */
import { mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { dirname } from 'node:path';
import type { GenerateResult, Plugin } from '@putnami/application';
import { getProjectRoot, getWorkspaceRoot, joinPath } from '@putnami/utils';
import {
  DEFAULT_SOURCES,
  SCHEMA_ORIGIN,
  type SchemasConfig,
  discoverSchemaSources,
  planSchemaPublications,
} from '../lib/schemas/publish';

export type { SchemasConfig } from '../lib/schemas/publish';

class SchemasPlugin implements Plugin {
  constructor(private config: SchemasConfig) {}

  generate(): GenerateResult {
    const projectRoot = getProjectRoot();
    const workspaceRoot = getWorkspaceRoot();

    const sources = discoverSchemaSources(workspaceRoot, this.config);
    const plan = planSchemaPublications(sources);

    if (plan.length === 0) {
      const globs = (this.config.sources ?? DEFAULT_SOURCES).join(', ');
      throw new Error(
        `schemas plugin: no public schemas found under [${globs}] in ${workspaceRoot}. ` +
          `Expected files declaring a "${SCHEMA_ORIGIN}…" $id.`,
      );
    }

    const genSchemasDir = joinPath(projectRoot, '.gen', 'public', 'schemas');
    const publicSchemasDir = joinPath(projectRoot, 'public', 'schemas');
    rmSync(genSchemasDir, { recursive: true, force: true });
    rmSync(publicSchemasDir, { recursive: true, force: true });

    const assets: Record<string, string> = {};
    for (const { file, urlPath } of plan) {
      const content = readFileSync(joinPath(workspaceRoot, file), 'utf8');
      const distRelative = joinPath('public', 'schemas', urlPath);

      // .gen/<distRelative> — included in the production build output.
      const genPath = joinPath(projectRoot, '.gen', distRelative);
      mkdirSync(dirname(genPath), { recursive: true });
      writeFileSync(genPath, content);

      // public/<...> — local development (the staticFiles plugin serves from public/).
      const publicPath = joinPath(projectRoot, distRelative);
      mkdirSync(dirname(publicPath), { recursive: true });
      writeFileSync(publicPath, content);

      // Register the asset so the build pipeline copies it to dist/.
      assets[distRelative] = genPath;
    }

    return { assets };
  }
}

export function schemas(config: SchemasConfig = {}): SchemasPlugin {
  return new SchemasPlugin(config);
}
