import { fileExists, joinPath, readPackageJson } from '@putnami/utils';

/**
 * How a generated client's `package.json` must pin `@putnami/client`.
 *
 * The framework workspace holds `@putnami/client` as a member, so a generated
 * package there depends on `workspace:*`. A consumer workspace takes the
 * package from its Bun catalog or from a pinned version; emitting `workspace:*`
 * there yields a package the workspace cannot install. The rule reads the
 * workspace root `package.json` once:
 *
 * - `@putnami/client` in `catalog` or any `catalogs.<name>` → `catalog:`;
 * - `@putnami/client` pinned in the root `dependencies` / `devDependencies` →
 *   that exact specifier;
 * - otherwise → `workspace:*`.
 */
export function resolveClientDependencySpec(workspaceRoot: string): string {
  const path = joinPath(workspaceRoot, 'package.json');
  if (!fileExists(path)) return 'workspace:*';
  const pkg = readPackageJson(path) as RootPackageJson | undefined;
  if (!pkg) return 'workspace:*';
  if (pkg.catalog?.[CLIENT_PACKAGE] !== undefined) return 'catalog:';
  for (const catalog of Object.values(pkg.catalogs ?? {})) {
    if (catalog?.[CLIENT_PACKAGE] !== undefined) return 'catalog:';
  }
  const pinned = pkg.dependencies?.[CLIENT_PACKAGE] ?? pkg.devDependencies?.[CLIENT_PACKAGE];
  if (typeof pinned === 'string' && pinned.length > 0 && !pinned.startsWith('workspace:')) return pinned;
  return 'workspace:*';
}

const CLIENT_PACKAGE = '@putnami/client';

interface RootPackageJson {
  catalog?: Record<string, string>;
  catalogs?: Record<string, Record<string, string>>;
  dependencies?: Record<string, string>;
  devDependencies?: Record<string, string>;
}
