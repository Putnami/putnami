import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { type BuildInfo, joinPath } from '@putnami/utils';

/**
 * Records, beside the pre-rendered HTML, the build version that static loaders
 * read while the pages rendered. No page renders to this name: every page file
 * ends in `.html`.
 *
 * Pre-rendered HTML is generation output, and generation is cached by content:
 * a cache hit restores pages rendered at another commit, and a container image
 * runs under the version its deployer injects. The server therefore replaces
 * the recorded version with its own `getBuildInfo()` version when it serves
 * a pre-rendered page at its route. Other identity fields (sha, branch, build
 * time) are not rewritten and stay as they were at render time.
 *
 * The record exists only when a page shows a version that carries its commit
 * suffix (see {@link restampableVersion}); otherwise the static output holds
 * nothing that depends on the commit.
 */
export const STATIC_BUILD_VERSION_FILE = '.build-version.json';

interface StaticBuildVersionRecord {
  version?: string;
}

/**
 * The build version when it can be found in rendered HTML without matching page
 * content: a version that ends with its commit suffix, such as
 * `0.1.0-20260927164104-94392288`. A bare release tag such as `1.0.0` can
 * appear in any page, so it is never restamped.
 */
export function restampableVersion(info: BuildInfo | undefined): string | undefined {
  const version = info?.version;
  const suffix = info?.suffix;
  return version && suffix && version.endsWith(`-${suffix}`) ? version : undefined;
}

/**
 * Record the build version the pre-rendered pages show, or remove a stale
 * record when no page shows one.
 */
export function writeStaticBuildVersion(staticDir: string, version: string | undefined): void {
  const recordPath = joinPath(staticDir, STATIC_BUILD_VERSION_FILE);
  if (!version) {
    rmSync(recordPath, { force: true });
    return;
  }
  mkdirSync(staticDir, { recursive: true });
  const record: StaticBuildVersionRecord = { version };
  writeFileSync(recordPath, `${JSON.stringify(record)}\n`);
}

/** Read the build version recorded beside the pre-rendered HTML. */
export function readStaticBuildVersion(staticDir: string): string | undefined {
  const recordPath = joinPath(staticDir, STATIC_BUILD_VERSION_FILE);
  if (!existsSync(recordPath)) return undefined;
  try {
    const record = JSON.parse(readFileSync(recordPath, 'utf8')) as StaticBuildVersionRecord;
    return typeof record.version === 'string' && record.version !== '' ? record.version : undefined;
  } catch {
    return undefined;
  }
}

/**
 * Replace every occurrence of the version a page was rendered with by the
 * version of the running build. Without both versions, the HTML is unchanged.
 */
export function restampBuildVersion(
  html: string,
  renderedVersion: string | undefined,
  currentVersion: string | undefined,
): string {
  if (!renderedVersion || !currentVersion || renderedVersion === currentVersion) return html;
  return html.replaceAll(renderedVersion, currentVersion);
}
