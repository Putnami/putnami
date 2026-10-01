import { mkdirSync, writeFileSync } from 'node:fs';
import { fileExists, getDirectoryName, isPlainObject, joinPath, readFileContent } from './index';

export type PackageJsonExportConditions = {
  types?: string;
  require?: string;
  import?: string;
  bun?: string;
  browser?: string;
  node?: string;
  default?: string;
};

export type PackageJson = {
  name: string;
  version: string;
  description?: string;
  author?: string;
  files?: string[];
  publishConfig?: {
    access?: 'public' | 'restricted';
  };
  engines?: {
    node?: string;
    bun?: string;
  };
  bugs?: {
    url?: string;
    email?: string;
  };
  funding?: {
    type?: string;
    url?: string;
  };
  repository?: {
    type: string;
    url: string;
  };
  packageManager?: string;
  homepage?: string;
  license?: string;
  private?: boolean;
  scripts?: Record<string, string>;
  type?: 'module' | 'commonjs';
  main?: string;
  types?: string;
  module?: string;
  exports?: string | Record<string, string | PackageJsonExportConditions>;
  dependencies?: Record<string, string>;
  devDependencies?: Record<string, string>;
  optionalDependencies?: Record<string, string>;
  peerDependencies?: Record<string, string>;
  peerDependenciesMeta?: Record<string, { optional: boolean }>;
  resolutions?: Record<string, string>;
  bin?: Record<string, string> | string;
  workspaces?: string[] | { packages: string[] };
  schematics?: string;
  putnami?:
    | true
    | {
        tags?: string[];
        type?: string;
        disabledJobs?: string[];
        build?: {
          assets?: { from?: string }[];
          compile?: Record<string, string>;
        };
        runsWith?: string[];
        publish?: string[];
      };
};

const PACKAGE_JSON_CACHE = new Map<string, PackageJson | undefined>();

function normalizePackageJsonPath(jsonPath: string): string {
  if (!jsonPath.endsWith('package.json')) {
    return joinPath(jsonPath, 'package.json');
  }
  return jsonPath;
}

export function clearPackageJsonCache(jsonPath?: string): void {
  if (!jsonPath) {
    PACKAGE_JSON_CACHE.clear();
    return;
  }

  PACKAGE_JSON_CACHE.delete(normalizePackageJsonPath(jsonPath));
}

export function readPackageJson(_jsonPath: string, options: { useCache?: boolean } = {}): PackageJson | undefined {
  const jsonPath = normalizePackageJsonPath(_jsonPath);
  const useCache = options.useCache !== false;

  if (useCache && PACKAGE_JSON_CACHE.has(jsonPath)) {
    return PACKAGE_JSON_CACHE.get(jsonPath);
  }

  let parsed: PackageJson | undefined;
  if (fileExists(jsonPath)) {
    const packageData = readFileContent(jsonPath, { encoding: 'utf8' });
    let raw: unknown;
    try {
      raw = JSON.parse(packageData);
    } catch (error) {
      throw new Error(`Failed to read package.json from ${jsonPath}: ${error}`);
    }
    // The file is untrusted input cast to PackageJson and consumed downstream;
    // reject anything that is not a JSON object (array, string, number, null).
    if (!isPlainObject(raw)) {
      throw new Error(`Failed to read package.json from ${jsonPath}: expected a JSON object`);
    }
    parsed = raw as PackageJson;
  }

  if (useCache) {
    PACKAGE_JSON_CACHE.set(jsonPath, parsed);
  }

  return parsed;
}

const sortKeys = <T extends Record<string, unknown> = Record<string, unknown>>(obj?: T): T => {
  if (!obj) {
    return undefined as unknown as T;
  }
  const sortedKeys = Object.keys(obj).sort();
  const sorted = sortedKeys.reduce((ac: T, key) => {
    (ac as Record<string, unknown>)[key] = obj[key];

    return ac;
  }, {} as T);

  return sorted;
};

export function updatePackageJson(path: string, data: Partial<PackageJson>) {
  const toUpdate = readPackageJson(path, { useCache: false }) || ({} as PackageJson);
  for (const [k, v] of Object.entries(data)) {
    (toUpdate as Record<string, unknown>)[k] = v;
  }

  toUpdate.dependencies = sortKeys(toUpdate.dependencies);
  toUpdate.devDependencies = sortKeys(toUpdate.devDependencies);
  toUpdate.peerDependencies = sortKeys(toUpdate.peerDependencies);
  toUpdate.peerDependenciesMeta = sortKeys(toUpdate.peerDependenciesMeta);

  delete toUpdate.dependencies?.[toUpdate.name];
  if (toUpdate.name !== '@putnami/typescript') {
    delete toUpdate.devDependencies?.[toUpdate.name];
    delete toUpdate.peerDependencies?.[toUpdate.name];
    delete toUpdate.peerDependenciesMeta?.[toUpdate.name];
  }

  mkdirSync(getDirectoryName(path), { recursive: true });
  writeFileSync(path, `${JSON.stringify(toUpdate, null, 2)}\n`);
  clearPackageJsonCache(path);
}

export function resolvePackageExportPath(exportsField: PackageJson['exports'], modulePath: string): string | undefined {
  if (!exportsField || typeof exportsField === 'string') {
    return undefined;
  }

  const exportEntry = exportsField[modulePath] ?? exportsField[`.${modulePath}`];
  if (!exportEntry) {
    return undefined;
  }
  if (typeof exportEntry === 'string') {
    return exportEntry;
  }

  return (
    exportEntry.import ??
    exportEntry.default ??
    exportEntry.require ??
    exportEntry.node ??
    exportEntry.bun ??
    exportEntry.browser ??
    exportEntry.types
  );
}
