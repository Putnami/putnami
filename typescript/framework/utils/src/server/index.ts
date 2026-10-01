/**
 * Server Utilities
 *
 * This module exports all utilities available in server-side environments.
 * It includes both shared utilities (browser-compatible) and server-only utilities
 * that depend on Node.js modules.
 *
 * @module @putnami/utils
 */

import './env.declare';

// Re-export all shared utilities (browser-compatible)
export * from '../shared';
export { GeneratorHelper } from './generator.helper';
// SSR-only file system utilities
export {
  copyDir,
  copyFile,
  deleteFile,
  fileExists,
  fileExistsAsync,
  fileSizeAsync,
  fileStat,
  fileStatAsync,
  listDir,
  listDirAsync,
  makeDir,
  makeDirAsync,
  makeTempDir,
  readFileAsync,
  readFileBytesAsync,
  readFileContent,
  removeDir,
  removeDirAsync,
  watchPath,
  writeFileAsync,
  writeFileContent,
} from './fs.utils';
// SSR-only OS utilities
export {
  getArch,
  getCpuCount,
  getCpuInfo,
  getHomeDir,
  getOsType,
  getPlatform,
  getTempDir,
  isLinux,
  isMacOS,
  isWindows,
} from './os.utils';
// SSR-only path utilities
export {
  getBaseName,
  getDirectoryName,
  getExtension,
  isAbsolutePath,
  joinPath,
  joinPosixPath,
  normalizePath,
  relativePath,
  resolvePath,
  toPosixPath,
} from './path.utils';
// SSR-only stack trace utilities
export {
  getCallerInfo,
  getExternalCaller,
  parseStackTrace,
  type StackElement,
} from './stack.utils';
// SSR-only workspace utilities
export {
  type BuildInfo,
  getBuildInfo,
  getCurrentProject,
  getProject,
  getProjectRoot,
  getWorkspaceRoot,
  listProjectDependencies,
  type ProjectRef,
} from './workspace.utils';
// SSR-only simple logger
export {
  clearLoggerCache,
  createLogger,
  getLogger,
  type LogLevel,
  type SimpleLogger,
} from './logger';
// SSR-only environment utilities
export { getEnv, restoreEnv } from './env.utils';
// Package.json utilities
export {
  clearPackageJsonCache,
  type PackageJson,
  type PackageJsonExportConditions,
  readPackageJson,
  resolvePackageExportPath,
  updatePackageJson,
} from './package-json';
// Project configuration utilities
export { type ProjectConfig, readProjectConfigFile } from './project-config';

// The JSONL hook protocol (event emitters, command runner, HookContext/HookEvent
// types) is a specialized extension-system SDK and is intentionally NOT part of
// this generic-utilities barrel. Import it from the dedicated subpath instead:
//
//   import { runHookCommand, standardHookModel } from '@putnami/utils/hooks';
//
// See `./hooks.ts`.
