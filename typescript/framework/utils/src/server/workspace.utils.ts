/**
 * Workspace Utility Functions (Server Only)
 *
 * Utilities for working with Putnami workspaces and projects.
 * These utilities require file system access and are only available
 * in server-side environments.
 *
 * @module @putnami/utils
 */
import './env.declare';

import { sep } from 'node:path';
import { fileExists, readFileContent } from './fs.utils';
import { getDirectoryName, joinPath, resolvePath } from './path.utils';

/**
 * Represents a project in the workspace.
 */
export interface ProjectRef {
  /** The package name from package.json */
  name: string;
  /** Absolute path to the project directory */
  path: string;
  /** List of dependency package names */
  dependencies: string[];
}

/**
 * Returns the current project's root directory.
 *
 * The `PUTNAMI_PROJECT_ROOT` environment variable names it when set, as it is
 * in every task the CLI runs. Otherwise the working directory names it; see
 * {@link resolveProjectRoot}.
 *
 * @returns The absolute path to the current project root
 *
 * @example
 * ```typescript
 * const projectDir = getProjectRoot();
 * // '/Users/dev/myproject/packages/core'
 * ```
 */
export function getProjectRoot(): string {
  return resolveProjectRoot();
}

/**
 * Returns the project root for `env` on `platform`: `PUTNAMI_PROJECT_ROOT`
 * when set, otherwise the working directory. On Linux and macOS the shell's
 * `PWD` names the working directory, keeping the path the user typed through
 * symbolic links, and `cwd()` names it when `PWD` is unset. Windows shells do
 * not maintain `PWD`, and a value inherited from a POSIX shell ancestor can
 * name another directory, so on Windows `cwd()` always names it.
 */
export function resolveProjectRoot(
  env: Readonly<Record<string, string | undefined>> = process.env,
  platform: NodeJS.Platform = process.platform,
  cwd: () => string = () => process.cwd(),
): string {
  const projectRoot = env['PUTNAMI_PROJECT_ROOT'];
  if (projectRoot) {
    return projectRoot;
  }
  const pwd = platform === 'win32' ? undefined : env['PWD'];
  return pwd || cwd();
}

let _currentProject: ProjectRef | undefined;

/**
 * Returns the current project's information.
 *
 * Caches the result after the first call for performance.
 *
 * @returns The current project information
 * @throws Error if no project is found at the current location
 *
 * @example
 * ```typescript
 * const project = getCurrentProject();
 * console.log(project.name);         // '@putnami/runtime'
 * console.log(project.dependencies); // ['@putnami/utils']
 * ```
 */
export function getCurrentProject(): ProjectRef {
  if (_currentProject) {
    return _currentProject;
  }
  _currentProject = getProject(getProjectRoot());
  if (!_currentProject) {
    throw new Error('No project found');
  }
  return _currentProject;
}

let _workspaceRoot: string | undefined;

/**
 * Checks whether a directory is a Putnami workspace root.
 *
 * A directory is a workspace root if it contains a `putnami.workspace.json`
 * or a legacy `.putnamirc.json` file.
 */
function isPutnamiWorkspaceRoot(dir: string): boolean {
  return fileExists(joinPath(dir, 'putnami.workspace.json')) || fileExists(joinPath(dir, '.putnamirc.json'));
}

/**
 * Walks upward from `start`, returning the nearest ancestor directory
 * (including `start` itself) for which `matches` holds, or `undefined` if the
 * filesystem root is reached without a match.
 *
 * The upward step uses `dirname`, which is separator-aware per path flavor, and
 * the walk terminates when `dirname(dir) === dir` (the root). A naive
 * `substring(0, lastIndexOf('/'))` step would be wrong on Windows:
 * `'C:\\foo\\bar'.lastIndexOf('/')` is `-1`, so it collapses to `''` on the
 * first step. `dirname` defaults to {@link getDirectoryName} (the current
 * platform's `path.dirname`); it is injectable so the walk can be exercised
 * against Windows paths (`path.win32.dirname`) on a POSIX host.
 */
export function findAncestorDirectory(
  start: string,
  matches: (dir: string) => boolean,
  dirname: (path: string) => string = getDirectoryName,
): string | undefined {
  let current = start;
  let parent = dirname(current);
  while (!matches(current) && parent !== current) {
    current = parent;
    parent = dirname(current);
  }
  return matches(current) ? current : undefined;
}

/**
 * Returns the root directory of the Putnami workspace.
 *
 * Searches upward from the current directory for a `package.json` file
 * containing a `"putnami"` field. The result is cached after the first call.
 *
 * Can be overridden by setting the `PUTNAMI_WORKSPACE_ROOT` environment variable.
 *
 * @returns The absolute path to the workspace root
 *
 * @example
 * ```typescript
 * const root = getWorkspaceRoot();
 * // '/Users/dev/myproject'
 * ```
 */
export function getWorkspaceRoot(throwIfMissing = true): string {
  const envRoot = process.env['PUTNAMI_WORKSPACE_ROOT'];
  if (
    (envRoot && envRoot !== _workspaceRoot) ||
    (!envRoot && _workspaceRoot && !isPutnamiWorkspaceRoot(_workspaceRoot))
  ) {
    _workspaceRoot = undefined;
  }
  if (!_workspaceRoot) {
    _workspaceRoot = findAncestorDirectory(envRoot || resolvePath('.'), isPutnamiWorkspaceRoot);
  }

  if (!_workspaceRoot) {
    if (throwIfMissing) {
      throw new Error('No workspace found. Please run the command `putnami workspace init`');
    }

    return resolvePath('.');
  }

  return _workspaceRoot;
}

/**
 * Retrieves project information by path or package name.
 *
 * Searches for a `package.json` file at the given path, or looks up the package
 * in the workspace's `node_modules`.
 *
 * @param pathOrName - Absolute path to a project directory, or a package name
 * @returns The project information, or `undefined` if not found
 *
 * @example
 * ```typescript
 * // By path
 * const project = getProject('/path/to/packages/core');
 *
 * // By package name
 * const project = getProject('@putnami/runtime');
 * ```
 */
export function getProject(pathOrName: string): ProjectRef | undefined {
  const readJsonPath = (projectPath: string): ProjectRef | undefined => {
    let path = projectPath;
    if (!path.endsWith('package.json')) {
      path = joinPath(path, 'package.json');
    }
    if (fileExists(path)) {
      try {
        const packageJson = JSON.parse(readFileContent(path, { encoding: 'utf8' }));
        const dependencies = Object.keys(packageJson.dependencies || {});
        path = manifestDirectory(path);
        return { name: packageJson.name, path, dependencies };
      } catch (e) {
        const detail = e instanceof Error ? e.message : String(e);
        throw new Error(`Failed to read package.json at ${path}: ${detail}`, { cause: e });
      }
    }
    return undefined;
  };

  // First try the path directly
  const project = readJsonPath(pathOrName);
  if (project) {
    return project;
  }

  const root = getWorkspaceRoot();

  // Search in workspace directories defined in root package.json
  const rootPackageJsonPath = joinPath(root, 'package.json');
  if (fileExists(rootPackageJsonPath)) {
    try {
      const rootPackageJson = JSON.parse(readFileContent(rootPackageJsonPath, { encoding: 'utf8' }));
      const workspaces: string[] = rootPackageJson.workspaces || [];
      for (const workspace of workspaces) {
        const candidatePath = joinPath(root, workspace);
        const candidate = readJsonPath(candidatePath);
        if (candidate && candidate.name === pathOrName) {
          return candidate;
        }
      }
    } catch {
      // Ignore errors reading root package.json
    }
  }

  // Fallback: try node_modules
  const altPath = joinPath(root, 'node_modules', pathOrName);
  return readJsonPath(altPath);
}

/**
 * Returns `manifestPath` without its final `/package.json`. When `separator`
 * is a backslash, as on Windows, it also removes a final backslash and
 * `package.json`. It returns any other path unchanged.
 */
export function manifestDirectory(manifestPath: string, separator: string = sep): string {
  for (const suffix of new Set(['/package.json', `${separator}package.json`])) {
    if (manifestPath.endsWith(suffix)) {
      return manifestPath.slice(0, -suffix.length);
    }
  }
  return manifestPath;
}

/**
 * Lists all dependencies of a project, including transitive dependencies.
 *
 * Recursively traverses the dependency tree to collect all package names.
 *
 * @param projectName - The package name or path of the project
 * @param dependencies - Optional set to accumulate dependencies (used for recursion)
 * @returns Array of all dependency package names (including the project itself)
 *
 * @example
 * ```typescript
 * const deps = listProjectDependencies('@putnami/runtime');
 * // ['@putnami/runtime', '@putnami/utils', 'reflect-metadata', ...]
 * ```
 */
export function listProjectDependencies(projectName: string, dependencies = new Set<string>()): string[] {
  if (dependencies.has(projectName)) {
    return [...dependencies];
  }
  dependencies.add(projectName);

  const project = getProject(projectName);
  if (!project) {
    return [...dependencies];
  }

  for (const dependency of project.dependencies) {
    listProjectDependencies(dependency, dependencies);
  }

  return [...dependencies];
}

/**
 * Build information assembled from the generated version.json file and
 * deploy-time environment.
 *
 * Which fields are present depends on the source: a local dev workspace has
 * the full scheduler-written version.json; a deployed container image carries
 * only a content-addressed stamp (`name` + `contentHash`) and receives its
 * release identity (`version`, `sha`) as environment injected at deploy time.
 */
export interface BuildInfo {
  /** Git short SHA. From `PUTNAMI_REVISION` when deploy-injected. */
  sha?: string;
  /** Git branch name */
  branch?: string;
  /** Whether there are uncommitted changes */
  isDirty?: boolean;
  /** Project name */
  name?: string;
  /**
   * Full deployable version (e.g. `"0.1.0-abc1234"`, or `"0.0.0-abc1234-deadbee"`
   * when the working tree is dirty). Matches the docker tag / npm version
   * produced by the publish jobs. From `PUTNAMI_VERSION` when deploy-injected.
   */
  version?: string;
  /**
   * Version suffix appended after the base semver — typically the short SHA,
   * or `"<sha>-<dirtyHash>"` when the working tree is dirty. Empty when no git
   * metadata is available.
   */
  suffix?: string;
  /** Content hash identifying the built artifact bytes */
  contentHash?: string;
  /** Build timestamp. Absent in container images (it would break digest stability). */
  buildTime?: string;
}

let _buildInfo: BuildInfo | undefined;

/**
 * Returns the build information for the running artifact.
 *
 * Reads `{projectRoot}/.gen/version.json` (the scheduler-written file in a
 * workspace, or the content stamp baked into a container image), then
 * overlays the deploy-time identity environment on top: `PUTNAMI_VERSION`
 * wins as `version` and `PUTNAMI_REVISION` as `sha`. Container images are
 * content-addressed and carry no git-derived bytes, so the environment —
 * injected by the deployer, which assigned the release id — is authoritative
 * for identity. The result is cached after the first call.
 *
 * @returns The build information, or `undefined` if neither the file nor the
 * identity environment exists
 *
 * @example
 * ```typescript
 * const buildInfo = getBuildInfo();
 * if (buildInfo) {
 *   console.log(buildInfo.contentHash); // '2981c22'
 *   console.log(buildInfo.version);     // '0.0.3'
 * }
 * ```
 */
export function getBuildInfo(): BuildInfo | undefined {
  if (_buildInfo) {
    return _buildInfo;
  }

  let info: BuildInfo | undefined;

  const projectRoot = getProjectRoot();
  const versionJsonPath = joinPath(projectRoot, '.gen', 'version.json');
  if (fileExists(versionJsonPath)) {
    try {
      const content = readFileContent(versionJsonPath, { encoding: 'utf8' });
      info = JSON.parse(content) as BuildInfo;
    } catch {
      info = undefined;
    }
  }

  const envVersion = process.env['PUTNAMI_VERSION'];
  const envRevision = process.env['PUTNAMI_REVISION'];
  if (envVersion || envRevision) {
    info = { ...(info ?? {}) };
    if (envVersion) {
      info.version = envVersion;
    }
    if (envRevision) {
      info.sha = envRevision;
    }
  }

  if (info) {
    _buildInfo = info;
  }
  return info;
}

/**
 * Clears the cached build info so the next {@link getBuildInfo} call re-reads
 * the file and environment.
 *
 * @internal Exposed for tests; not part of the public API surface.
 */
export function resetBuildInfoCache(): void {
  _buildInfo = undefined;
}
