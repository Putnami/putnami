import { createHash } from 'node:crypto';
import { mkdir, readdir, readFile, rm, stat, writeFile } from 'node:fs/promises';
import { robustRemove } from '@putnami/runtime/robustio';
import { fileExistsAsync, getBuildInfo, getCurrentProject, getWorkspaceRoot, joinPath } from '@putnami/utils';
import type { CacheEntry, CacheStorage } from './cache-storage.interface';

/** Maximum number of build versions to keep in the cache */
const MAX_CACHE_VERSIONS = 5;

interface DiskCacheEntry<T> {
  value: T;
  expiresAt: number | null;
}

// Global build version for cache isolation between builds (manual override)
let globalBuildVersion: string | undefined;

/**
 * Set the global build version for cache isolation.
 * This overrides the automatic detection from version.json.
 *
 * @param version - Build version/content hash
 */
export function setCacheBuildVersion(version: string): void {
  globalBuildVersion = version;
}

/**
 * Get the current build version for cache isolation.
 *
 * Resolution order:
 * 1. Manually set version via `setCacheBuildVersion()`
 * 2. Auto-detected from `{projectRoot}/.gen/version.json`
 *
 * @returns The build version, or `undefined` if not available
 */
export function getCacheBuildVersion(): string | undefined {
  if (globalBuildVersion) {
    return globalBuildVersion;
  }
  // Auto-detect from version.json
  return getBuildInfo()?.contentHash;
}

/**
 * Builds the cache directory path following the convention:
 * {workspaceRoot}/.putnami/cache/{project}/{buildVersion}/{cacheName}
 *
 * @param cacheName - Name of the cache (e.g., 'loader', 'page')
 * @param buildVersion - Build version/content hash (auto-detected if not provided)
 * @returns The full cache directory path
 */
function buildCacheDir(cacheName: string, buildVersion?: string): string {
  const version = buildVersion ?? getCacheBuildVersion();
  try {
    const workspaceRoot = getWorkspaceRoot(false);
    const project = getCurrentProject();
    const projectName = project.name.replace(/[@/]/g, '-').replace(/^-/, '');

    if (version) {
      return joinPath(workspaceRoot, '.putnami', 'cache', projectName, version, cacheName);
    }
    return joinPath(workspaceRoot, '.putnami', 'cache', projectName, cacheName);
  } catch {
    // Fallback if workspace/project info is not available
    if (version) {
      return joinPath('.putnami', 'cache', version, cacheName);
    }
    return joinPath('.putnami', 'cache', cacheName);
  }
}

/**
 * Get the project's cache base directory (without version).
 * Returns: {workspaceRoot}/.putnami/cache/{project}
 */
function getProjectCacheBaseDir(): string | undefined {
  try {
    const workspaceRoot = getWorkspaceRoot(false);
    const project = getCurrentProject();
    const projectName = project.name.replace(/[@/]/g, '-').replace(/^-/, '');
    return joinPath(workspaceRoot, '.putnami', 'cache', projectName);
  } catch {
    return undefined;
  }
}

/**
 * Evict old cache versions in a directory, keeping only the most recent ones.
 * Exported for testing purposes.
 *
 * @param baseDir - The base directory containing version subdirectories
 */
export async function evictOldVersionsInDir(baseDir: string): Promise<void> {
  try {
    await stat(baseDir);
  } catch {
    return; // Directory does not exist
  }

  try {
    const entries = await readdir(baseDir, { withFileTypes: true });
    const dirs = entries.filter((e) => e.isDirectory());
    const versionDirs: { name: string; path: string; mtime: number }[] = [];
    for (const e of dirs) {
      const fullPath = joinPath(baseDir, e.name);
      try {
        // biome-ignore lint/performance/noAwaitInLoops: Sequential stat for directory listing
        const stats = await stat(fullPath);
        versionDirs.push({ name: e.name, path: fullPath, mtime: stats.mtimeMs });
      } catch {
        // skip
      }
    }

    // Sort by modification time, most recent first
    versionDirs.sort((a, b) => b.mtime - a.mtime);

    // Remove versions beyond the limit
    const toRemove = versionDirs.slice(MAX_CACHE_VERSIONS);
    for (const version of toRemove) {
      try {
        // biome-ignore lint/performance/noAwaitInLoops: Sequential removal for filesystem consistency
        await rm(version.path, { recursive: true, force: true });
      } catch {
        // Ignore removal errors
      }
    }
  } catch {
    // Ignore errors during eviction
  }
}

/**
 * Evict old cache versions, keeping only the most recent ones.
 * Versions are identified by subdirectories in the project cache directory.
 */
async function evictOldVersions(): Promise<void> {
  const baseDir = getProjectCacheBaseDir();
  if (baseDir) {
    await evictOldVersionsInDir(baseDir);
  }
}

/**
 * Disk-based cache storage implementation.
 * Stores cache entries as JSON files in the specified directory.
 *
 * Path convention: {workspaceRoot}/.putnami/cache/{project}/{buildVersion}/{cacheName}
 */
export class DiskCacheStorage implements CacheStorage {
  private _cacheDir: string | undefined;
  private readonly _cacheName: string;
  private readonly _buildVersion: string | undefined;
  private readonly _customCacheDir: string | undefined;

  /**
   * Create a new disk cache storage.
   *
   * @param cacheName - Name of the cache (e.g., 'loader', 'page')
   * @param buildVersion - Optional build version (uses global if not provided)
   * @param customCacheDir - Optional custom cache directory (for testing only)
   */
  constructor(cacheName: string, buildVersion?: string, customCacheDir?: string) {
    this._cacheName = cacheName;
    this._buildVersion = buildVersion;
    this._customCacheDir = customCacheDir;
  }

  /**
   * Get the cache directory, lazily resolved on first access.
   * This allows the global build version to be set after module initialization.
   */
  private get cacheDir(): string {
    if (!this._cacheDir) {
      this._cacheDir = this._customCacheDir ?? buildCacheDir(this._cacheName, this._buildVersion);
    }
    return this._cacheDir;
  }

  private _cacheDirCreated = false;

  private async ensureCacheDir(): Promise<void> {
    if (this._cacheDirCreated) return;
    await mkdir(this.cacheDir, { recursive: true });
    this._cacheDirCreated = true;
    // Evict old versions after creating a new cache directory
    await evictOldVersions();
  }

  private hashKey(key: string): string {
    return createHash('sha256').update(key).digest('hex').substring(0, 16);
  }

  private getFilePath(key: string): string {
    return joinPath(this.cacheDir, `${this.hashKey(key)}.json`);
  }

  async get<T>(key: string): Promise<CacheEntry<T> | undefined> {
    const filePath = this.getFilePath(key);

    if (!(await fileExistsAsync(filePath))) {
      return undefined;
    }

    try {
      const content = await readFile(filePath, 'utf-8');
      const entry: DiskCacheEntry<T> = JSON.parse(content);

      // Check if entry has expired
      if (entry.expiresAt !== null && entry.expiresAt < Date.now()) {
        // Remove expired entry
        await this.deleteFile(filePath);
        return undefined;
      }

      return {
        value: entry.value,
        expiresAt: entry.expiresAt,
      };
    } catch {
      // Invalid or corrupted cache file, remove it
      await this.deleteFile(filePath);
      return undefined;
    }
  }

  async set<T>(key: string, value: T, ttl?: number): Promise<void> {
    await this.ensureCacheDir();

    const filePath = this.getFilePath(key);
    const expiresAt = ttl ? Date.now() + ttl : null;

    const entry: DiskCacheEntry<T> = {
      value,
      expiresAt,
    };

    await writeFile(filePath, JSON.stringify(entry), 'utf-8');
  }

  async delete(key: string): Promise<void> {
    const filePath = this.getFilePath(key);
    await this.deleteFile(filePath);
  }

  async clear(): Promise<void> {
    await rm(this.cacheDir, { recursive: true, force: true }).catch(() => {});
    this._cacheDirCreated = false;
  }

  /**
   * Delete all cache entries matching a glob pattern.
   * Note: Pattern matching is limited for disk cache since we hash keys.
   * Only supports exact key deletion or full clear.
   * For pattern-based eviction, consider using memory cache layer.
   * @returns Number of deleted entries (0 for pattern-based deletion on disk)
   */
  async deletePattern(pattern: string): Promise<number> {
    // For disk cache, pattern deletion is expensive as we'd need to
    // store original keys. For now, if pattern contains wildcards,
    // we clear the entire cache. For exact patterns, delete directly.
    if (pattern.includes('*') || pattern.includes('?')) {
      // Clear all - this is a limitation of hashed key storage
      const files = await this.listCacheFiles();
      const count = files.length;
      await this.clear();
      return count;
    }

    // Exact key deletion
    const filePath = this.getFilePath(pattern);
    if (await fileExistsAsync(filePath)) {
      await this.deleteFile(filePath);
      return 1;
    }
    return 0;
  }

  private async deleteFile(filePath: string): Promise<void> {
    try {
      if (await fileExistsAsync(filePath)) {
        // On Windows a concurrent read of the entry holds the file for a
        // moment; robustRemove waits it out, so the deletion is not lost.
        await robustRemove(filePath);
      }
    } catch {
      // Ignore deletion errors
    }
  }

  private async listCacheFiles(): Promise<string[]> {
    try {
      return (await readdir(this.cacheDir)).filter((f) => f.endsWith('.json'));
    } catch {
      return [];
    }
  }

  /**
   * Get the number of cached entries (for testing/debugging)
   */
  async size(): Promise<number> {
    return (await this.listCacheFiles()).length;
  }
}
