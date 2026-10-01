import { afterAll, beforeEach, describe, expect, it } from 'bun:test';
import { existsSync, mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve, win32 } from 'node:path';
import { getBuildInfo, getProject, getProjectRoot, getWorkspaceRoot, listProjectDependencies } from '../src';
import {
  findAncestorDirectory,
  manifestDirectory,
  resetBuildInfoCache,
  resolveProjectRoot,
} from '../src/server/workspace.utils';
import { restoreEnv } from '../src/server/env.utils';

function resolveUtilsProjectPath(): string {
  const root = getWorkspaceRoot();
  const candidates = [
    // An impacted gate can inherit another project's environment. The fixture
    // belongs to this test file, not to that ambient project root.
    resolve(import.meta.dir, '..'),
    getProjectRoot(),
    resolve(root, 'typescript/framework/utils'),
    resolve(root, '../typescript/framework/utils'),
  ];

  for (const candidate of candidates) {
    if (existsSync(resolve(candidate, 'package.json'))) {
      return candidate;
    }
  }

  throw new Error('Could not resolve typescript/framework/utils project path in workspace');
}

describe('workspace.utils', () => {
  describe('getWorkspaceRoot / workspaceRoot', () => {
    it('returns a string path', () => {
      const root = getWorkspaceRoot();
      expect(typeof root).toBe('string');
      expect(root.length).toBeGreaterThan(0);
    });

    it('workspaceRoot is an alias for getWorkspaceRoot', () => {
      expect(getWorkspaceRoot()).toBe(getWorkspaceRoot());
    });

    // Regression tests. getWorkspaceRoot's upward walk must not use
    // `substring(0, lastIndexOf('/'))`, which cannot step up a Windows path
    // (`'C:\\foo\\bar'.lastIndexOf('/')` === -1 → collapses to '' on the first
    // iteration). The walk lives in `findAncestorDirectory` and steps with a
    // separator-aware `dirname`. Injecting `win32.dirname` here exercises the
    // actual production walk against Windows paths on this POSIX host — the
    // case `/`-slicing gets wrong. (A POSIX-path test cannot catch this
    // regression: a `/`-slicing walk works fine on POSIX paths.)
    it('walks upward through Windows ancestors to the matching root', () => {
      expect(findAncestorDirectory('C:\\app\\a\\b\\c', (dir) => dir === 'C:\\app', win32.dirname)).toBe('C:\\app');
    });

    it('matches the start directory itself without walking', () => {
      expect(findAncestorDirectory('C:\\app', (dir) => dir === 'C:\\app', win32.dirname)).toBe('C:\\app');
    });

    it('returns undefined when no ancestor matches, terminating at the drive root', () => {
      // Must terminate at `C:\` (where `dirname(p) === p`) and report no match
      // rather than looping forever or collapsing to an empty string.
      expect(findAncestorDirectory('C:\\foo\\bar', () => false, win32.dirname)).toBeUndefined();
    });
  });

  describe('getProjectRoot / projectRoot', () => {
    const originalPwd = process.env['PWD'];
    const originalProjectRoot = process.env['PUTNAMI_PROJECT_ROOT'];

    afterAll(() => {
      restoreEnv('PWD', originalPwd);
      restoreEnv('PUTNAMI_PROJECT_ROOT', originalProjectRoot);
    });

    it('returns PUTNAMI_PROJECT_ROOT when it is set', () => {
      process.env['PUTNAMI_PROJECT_ROOT'] = '/project/root';
      process.env['PWD'] = '/test/path';
      expect(getProjectRoot()).toBe('/project/root');
    });

    it('returns the working directory when PUTNAMI_PROJECT_ROOT and PWD are unset', () => {
      delete process.env.PUTNAMI_PROJECT_ROOT;
      delete process.env.PWD;
      expect(getProjectRoot()).toBe(process.cwd());
    });

    it('returns PWD on Linux and macOS', () => {
      const cwd = () => '/private/real/path';
      expect(resolveProjectRoot({ PWD: '/test/path' }, 'darwin', cwd)).toBe('/test/path');
      expect(resolveProjectRoot({ PWD: '/test/path' }, 'linux', cwd)).toBe('/test/path');
      expect(resolveProjectRoot({}, 'linux', cwd)).toBe('/private/real/path');
    });

    it('ignores an inherited PWD on Windows', () => {
      const cwd = () => 'C:\\Users\\dev\\app';
      expect(resolveProjectRoot({ PWD: 'C:/Users/dev/other' }, 'win32', cwd)).toBe('C:\\Users\\dev\\app');
      expect(resolveProjectRoot({}, 'win32', cwd)).toBe('C:\\Users\\dev\\app');
      expect(resolveProjectRoot({ PUTNAMI_PROJECT_ROOT: 'C:\\ws\\api', PWD: '/c/ws' }, 'win32', cwd)).toBe(
        'C:\\ws\\api',
      );
    });

    // Regression for the Bun 1.4 migration: restoring a snapshot of an
    // originally absent variable must leave it absent — assigning the
    // snapshot back unconditionally would store the string "undefined".
    it('keeps an originally absent variable absent after snapshot restore', () => {
      const name = 'PUTNAMI_TEST_ABSENT_ENV_PROBE';
      delete process.env[name];
      const snapshot = process.env[name];
      process.env[name] = 'transient';
      restoreEnv(name, snapshot);
      expect(name in process.env).toBe(false);
      expect(process.env[name]).toBeUndefined();
    });
  });

  describe('getProject', () => {
    it('returns undefined for non-existent path', () => {
      const project = getProject('/non/existent/path');
      expect(project).toBeUndefined();
    });

    it('returns project info for valid path', () => {
      const project = getProject(resolveUtilsProjectPath());

      expect(project).toBeDefined();
      expect(project?.name).toBe('@putnami/utils');
      expect(project?.path).toContain(join('typescript', 'framework', 'utils'));
      expect(project?.path).not.toEndWith('package.json');
      expect(Array.isArray(project?.dependencies)).toBe(true);
    });

    it('names the directory of a manifest with either separator', () => {
      expect(manifestDirectory('/ws/node_modules/@putnami/runtime/package.json', '/')).toBe(
        '/ws/node_modules/@putnami/runtime',
      );
      expect(manifestDirectory('C:\\ws\\node_modules\\@putnami\\runtime\\package.json', '\\')).toBe(
        'C:\\ws\\node_modules\\@putnami\\runtime',
      );
      expect(manifestDirectory('C:/ws/app/package.json', '\\')).toBe('C:/ws/app');
      // A backslash is a file name character on Linux and macOS.
      expect(manifestDirectory('/ws/app\\package.json', '/')).toBe('/ws/app\\package.json');
      expect(manifestDirectory('/ws/app/mypackage.json', '/')).toBe('/ws/app/mypackage.json');
    });

    it('throws with the underlying cause for a malformed package.json', () => {
      const dir = mkdtempSync(join(tmpdir(), 'putnami-getproject-'));
      writeFileSync(join(dir, 'package.json'), '{ not valid json');

      let caught: unknown;
      try {
        getProject(dir);
      } catch (e) {
        caught = e;
      }

      expect(caught).toBeInstanceOf(Error);
      const error = caught as Error;
      expect(error.message).toContain(join(dir, 'package.json'));
      expect(error.cause).toBeInstanceOf(Error);
      expect(error.message).toContain((error.cause as Error).message);
    });
  });

  describe('listProjectDependencies / listDependencies', () => {
    it('returns an array of dependencies', () => {
      const deps = listProjectDependencies(resolveUtilsProjectPath());

      expect(Array.isArray(deps)).toBe(true);
      expect(deps.length).toBeGreaterThan(0);
      expect(deps.some((d) => d.includes('utils'))).toBe(true);
    });

    it('returns array with project name for minimal dependencies', () => {
      const deps = listProjectDependencies('@putnami/utils');
      expect(Array.isArray(deps)).toBe(true);
    });
  });

  describe('getBuildInfo', () => {
    const originalProjectRoot = process.env['PUTNAMI_PROJECT_ROOT'];
    const originalVersion = process.env['PUTNAMI_VERSION'];
    const originalRevision = process.env['PUTNAMI_REVISION'];

    function makeProjectRoot(versionJson?: Record<string, unknown>): string {
      const dir = mkdtempSync(join(tmpdir(), 'putnami-buildinfo-'));
      if (versionJson) {
        mkdirSync(join(dir, '.gen'), { recursive: true });
        writeFileSync(join(dir, '.gen', 'version.json'), JSON.stringify(versionJson));
      }
      return dir;
    }

    beforeEach(() => {
      resetBuildInfoCache();
      delete process.env.PUTNAMI_VERSION;
      delete process.env.PUTNAMI_REVISION;
    });

    afterAll(() => {
      resetBuildInfoCache();
      restoreEnv('PUTNAMI_PROJECT_ROOT', originalProjectRoot);
      restoreEnv('PUTNAMI_VERSION', originalVersion);
      restoreEnv('PUTNAMI_REVISION', originalRevision);
    });

    it('reads the version.json file when present', () => {
      process.env['PUTNAMI_PROJECT_ROOT'] = makeProjectRoot({
        name: 'my-app',
        version: '0.1.0-abc1234',
        sha: 'abc1234',
        contentHash: '2981c22',
      });

      const info = getBuildInfo();
      expect(info?.name).toBe('my-app');
      expect(info?.version).toBe('0.1.0-abc1234');
      expect(info?.contentHash).toBe('2981c22');
    });

    it('overlays deploy-injected identity env on the baked stamp', () => {
      // A deployed container carries a content-only stamp; the control plane
      // injects the release identity as environment at deploy time.
      process.env['PUTNAMI_PROJECT_ROOT'] = makeProjectRoot({
        name: 'my-app',
        contentHash: '2981c22',
      });
      process.env['PUTNAMI_VERSION'] = '0.2.0-def5678';
      process.env['PUTNAMI_REVISION'] = 'def5678';

      const info = getBuildInfo();
      expect(info?.name).toBe('my-app');
      expect(info?.contentHash).toBe('2981c22');
      expect(info?.version).toBe('0.2.0-def5678');
      expect(info?.sha).toBe('def5678');
    });

    it('env identity wins over a file-provided version', () => {
      process.env['PUTNAMI_PROJECT_ROOT'] = makeProjectRoot({
        name: 'my-app',
        version: '0.1.0-stale',
      });
      process.env['PUTNAMI_VERSION'] = '0.1.0-current';

      expect(getBuildInfo()?.version).toBe('0.1.0-current');
    });

    it('returns env-only identity when no file exists', () => {
      process.env['PUTNAMI_PROJECT_ROOT'] = makeProjectRoot();
      process.env['PUTNAMI_VERSION'] = '0.3.0-fff0000';

      const info = getBuildInfo();
      expect(info?.version).toBe('0.3.0-fff0000');
    });

    it('returns undefined without file or identity env', () => {
      process.env['PUTNAMI_PROJECT_ROOT'] = makeProjectRoot();

      expect(getBuildInfo()).toBeUndefined();
    });
  });
});
