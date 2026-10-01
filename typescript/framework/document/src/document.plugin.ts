import { randomUUID } from 'node:crypto';
import { mkdir, rm, writeFile } from 'node:fs/promises';
import type { GenerateResult, Module, Plugin } from '@putnami/application';
import { useConfig } from '@putnami/runtime';
import { robustRemove, robustRename } from '@putnami/runtime/robustio';
import {
  getCurrentProject,
  getDirectoryName,
  getProject,
  getProjectRoot,
  joinPath,
  listProjectDependencies,
} from '@putnami/utils';
import { type CollectionDefinition, isCollectionDefinition } from './collection';
import { DocumentConfig } from './config';
import { closeAllBackends } from './factory';
import { buildDocumentInfraManifest, DEFAULT_STORE, type InfraManifest } from './infra';

export type DocumentPluginConfig = Record<string, never>;

export function document(_config: DocumentPluginConfig = {}): Plugin {
  return {
    async generate(_: Module): Promise<GenerateResult> {
      const projectPath = getProjectRoot();
      const collections = await discoverCollections(projectPath);
      const manifest = buildDocumentInfraManifest(collections, resolveStoreBackend);

      const sidecarPath = joinPath(projectPath, '.gen/infra/document.json');
      if (manifest) {
        await writeManifestAtomic(sidecarPath, manifest);
      } else {
        // No firestore infrastructure to declare: drop any stale scratch
        // fragment so a prior build's requirements can't linger.
        await robustRemove(sidecarPath);
      }

      return {};
    },
    async stop() {
      await closeAllBackends();
    },
  };
}

/**
 * Resolve the configured backend for a single named store. Runtime resolves a
 * named store's config at `document.<store>` and the default store at
 * `document` (see factory.ts `resolvedPath`); mirror that so a memory default
 * with a firestore named store — or the reverse — is reported correctly.
 * Defaults to memory when config can't be loaded at build time.
 */
function resolveStoreBackend(store: string): string {
  try {
    const path = store === DEFAULT_STORE ? undefined : `document.${store}`;
    return useConfig(DocumentConfig, { path }).backend;
  } catch {
    return 'memory';
  }
}

/**
 * Collect the collection definitions reachable from a workload: its own source
 * plus any dependency library that uses `@putnami/document`. Libraries usually
 * have no Application of their own to run a generate() hook, so the workload
 * that imports them must declare their collections — mirroring how the SQL
 * plugin walks dependencies.
 */
async function discoverCollections(projectPath: string): Promise<CollectionDefinition[]> {
  const collections: CollectionDefinition[] = [];
  const seen = new Set<string>();

  const scanRoot = async (root: string): Promise<void> => {
    const glob = new Bun.Glob('src/**/*.ts');
    for (const file of glob.scanSync({ cwd: root, absolute: false })) {
      const absolute = joinPath(root, file);
      let content: string;
      try {
        content = await Bun.file(absolute).text();
      } catch {
        continue;
      }
      if (!content.includes('@putnami/document') || !/\bCollection\s*\(/.test(content)) {
        continue;
      }

      let mod: Record<string, unknown>;
      try {
        mod = await import(absolute);
      } catch {
        continue;
      }

      for (const value of Object.values(mod)) {
        if (!isCollectionDefinition(value)) {
          continue;
        }
        const key = `${value.options.db ?? DEFAULT_STORE}::${value.collectionName}`;
        if (!seen.has(key)) {
          seen.add(key);
          collections.push(value);
        }
      }
    }
  };

  await scanRoot(projectPath);

  const project = getCurrentProject();
  for (const dependency of listProjectDependencies(project.name)) {
    if (dependency === project.name || dependency === '@putnami/document') {
      continue;
    }
    if (!listProjectDependencies(dependency).includes('@putnami/document')) {
      continue;
    }
    const dep = getProject(dependency);
    if (dep) {
      await scanRoot(dep.path);
    }
  }

  return collections;
}

async function writeManifestAtomic(path: string, manifest: InfraManifest): Promise<void> {
  await mkdir(getDirectoryName(path), { recursive: true });
  const data = `${JSON.stringify(manifest, null, 2)}\n`;
  // Unique temp name per write: the same generate task can run concurrently
  // for the build and test pipelines, so a shared temp file would let one
  // writer rename out from under the other (ENOENT) or publish a partial file.
  const tmp = `${path}.${process.pid}.${randomUUID()}.tmp`;
  try {
    await writeFile(tmp, data, 'utf8');
    await robustRename(tmp, path);
  } catch (error) {
    // Best-effort cleanup: it must not replace the write error.
    await rm(tmp, { force: true }).catch(() => undefined);
    throw error;
  }
}
