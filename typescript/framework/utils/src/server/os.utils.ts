/**
 * OS Utility Functions (Server Only)
 *
 * These utilities wrap Node.js os functions and provide consistent
 * access to system information. Only available in server-side environments.
 *
 * @module @putnami/utils
 */

import {
  arch as archNode,
  cpus as cpusNode,
  homedir as homedirNode,
  platform as platformNode,
  tmpdir as tmpdirNode,
  type as typeNode,
} from 'node:os';

/**
 * Returns the operating system's default directory for temporary files.
 *
 * @returns The temp directory path
 *
 * @example
 * ```typescript
 * const temp = getTempDir();
 * // '/tmp' on Linux, '/var/folders/...' on macOS, 'C:\\Users\\...\\Temp' on Windows
 * ```
 */
export const getTempDir = tmpdirNode;

/**
 * Returns the path of the current user's home directory.
 *
 * @returns The home directory path
 *
 * @example
 * ```typescript
 * const home = getHomeDir();
 * // '/home/user' on Linux, '/Users/user' on macOS, 'C:\\Users\\user' on Windows
 * ```
 */
export const getHomeDir = homedirNode;

/**
 * Returns a string identifying the operating system platform.
 *
 * @returns The platform name ('aix', 'darwin', 'freebsd', 'linux', 'openbsd', 'sunos', 'win32')
 *
 * @example
 * ```typescript
 * const os = getPlatform();
 * if (os === 'darwin') {
 *   // macOS specific code
 * }
 * ```
 */
export const getPlatform = platformNode;

/**
 * Returns the operating system CPU architecture.
 *
 * @returns The architecture ('arm', 'arm64', 'ia32', 'mips', 'mipsel', 'ppc', 'ppc64', 's390', 's390x', 'x64')
 *
 * @example
 * ```typescript
 * const arch = getArch();
 * if (arch === 'arm64') {
 *   // ARM64 specific code
 * }
 * ```
 */
export const getArch = archNode;

/**
 * Returns the operating system name.
 *
 * @returns The OS type ('Linux', 'Darwin', 'Windows_NT')
 *
 * @example
 * ```typescript
 * const osType = getOsType();
 * ```
 */
export const getOsType = typeNode;

/**
 * Returns an array of objects containing information about each logical CPU core.
 *
 * @returns Array of CPU info objects
 *
 * @example
 * ```typescript
 * const cores = getCpuInfo();
 * const coreCount = cores.length;
 * ```
 */
export const getCpuInfo = cpusNode;

/**
 * Returns the number of logical CPU cores available.
 *
 * @returns The number of CPU cores
 *
 * @example
 * ```typescript
 * const parallelism = getCpuCount();
 * // Use for setting max parallel jobs
 * ```
 */
export function getCpuCount(): number {
  return cpusNode().length;
}

/**
 * Checks if the current platform is Windows.
 *
 * @returns `true` if running on Windows
 *
 * @example
 * ```typescript
 * if (isWindows()) {
 *   // Windows specific code
 * }
 * ```
 */
export function isWindows(): boolean {
  return process.platform === 'win32';
}

/**
 * Checks if the current platform is macOS.
 *
 * @returns `true` if running on macOS
 *
 * @example
 * ```typescript
 * if (isMacOS()) {
 *   // macOS specific code
 * }
 * ```
 */
export function isMacOS(): boolean {
  return process.platform === 'darwin';
}

/**
 * Checks if the current platform is Linux.
 *
 * @returns `true` if running on Linux
 *
 * @example
 * ```typescript
 * if (isLinux()) {
 *   // Linux specific code
 * }
 * ```
 */
export function isLinux(): boolean {
  return process.platform === 'linux';
}
