import { afterEach, describe, expect, it } from 'bun:test';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { clearPackageJsonCache, readPackageJson, resolvePackageExportPath, updatePackageJson } from '../src';

const tempDirs: string[] = [];

function makeTempDir(): string {
  const dir = mkdtempSync(join(tmpdir(), 'putnami-utils-package-json-'));
  tempDirs.push(dir);
  return dir;
}

afterEach(() => {
  clearPackageJsonCache();
  while (tempDirs.length > 0) {
    const dir = tempDirs.pop();
    if (dir) {
      rmSync(dir, { recursive: true, force: true });
    }
  }
});

describe('package-json utils', () => {
  it('reads package.json from a directory path and supports cache invalidation', () => {
    const dir = makeTempDir();
    const filePath = join(dir, 'package.json');

    writeFileSync(filePath, JSON.stringify({ name: '@demo/pkg', version: '1.0.0' }));
    expect(readPackageJson(dir)?.version).toBe('1.0.0');

    writeFileSync(filePath, JSON.stringify({ name: '@demo/pkg', version: '2.0.0' }));
    expect(readPackageJson(dir)?.version).toBe('1.0.0');
    expect(readPackageJson(dir, { useCache: false })?.version).toBe('2.0.0');

    clearPackageJsonCache(dir);
    expect(readPackageJson(filePath)?.version).toBe('2.0.0');
  });

  it('returns undefined when package.json does not exist', () => {
    const dir = makeTempDir();
    expect(readPackageJson(dir)).toBeUndefined();
  });

  it('throws a readable error on invalid package.json', () => {
    const dir = makeTempDir();
    const filePath = join(dir, 'package.json');
    writeFileSync(filePath, '{ invalid-json');

    expect(() => readPackageJson(filePath, { useCache: false })).toThrow(
      `Failed to read package.json from ${filePath}`,
    );
  });

  it('rejects a package.json whose JSON is not an object', () => {
    const dir = makeTempDir();
    const filePath = join(dir, 'package.json');

    for (const payload of ['[]', '"a string"', '42', 'null']) {
      writeFileSync(filePath, payload);
      expect(() => readPackageJson(filePath, { useCache: false })).toThrow('expected a JSON object');
    }
  });

  it('updates, sorts, and removes self-references before writing', () => {
    const dir = makeTempDir();
    const filePath = join(dir, 'nested', 'package.json');

    updatePackageJson(filePath, {
      name: '@demo/pkg',
      version: '1.0.0',
      dependencies: {
        zed: '1.0.0',
        '@demo/pkg': '1.0.0',
        alpha: '1.0.0',
      },
      devDependencies: {
        '@demo/pkg': '1.0.0',
        eslint: '9.0.0',
      },
      peerDependencies: {
        '@demo/pkg': '1.0.0',
        react: '^19.0.0',
      },
      peerDependenciesMeta: {
        '@demo/pkg': { optional: true },
        react: { optional: false },
      },
    });

    const saved = JSON.parse(readFileSync(filePath, 'utf8')) as {
      dependencies: Record<string, string>;
      devDependencies: Record<string, string>;
      peerDependencies: Record<string, string>;
      peerDependenciesMeta: Record<string, { optional: boolean }>;
    };

    expect(Object.keys(saved.dependencies)).toEqual(['alpha', 'zed']);
    expect(saved.devDependencies).toEqual({ eslint: '9.0.0' });
    expect(saved.peerDependencies).toEqual({ react: '^19.0.0' });
    expect(saved.peerDependenciesMeta).toEqual({ react: { optional: false } });
  });

  it('keeps self dev/peer dependencies for @putnami/typescript', () => {
    const dir = makeTempDir();
    const filePath = join(dir, 'package.json');

    updatePackageJson(filePath, {
      name: '@putnami/typescript',
      version: '1.0.0',
      devDependencies: {
        '@putnami/typescript': '1.0.0',
      },
      peerDependencies: {
        '@putnami/typescript': '1.0.0',
      },
      peerDependenciesMeta: {
        '@putnami/typescript': { optional: true },
      },
    });

    const saved = JSON.parse(readFileSync(filePath, 'utf8')) as {
      devDependencies: Record<string, string>;
      peerDependencies: Record<string, string>;
      peerDependenciesMeta: Record<string, { optional: boolean }>;
    };

    expect(saved.devDependencies['@putnami/typescript']).toBe('1.0.0');
    expect(saved.peerDependencies['@putnami/typescript']).toBe('1.0.0');
    expect(saved.peerDependenciesMeta['@putnami/typescript']).toEqual({ optional: true });
  });

  it('resolves package export paths with condition priority', () => {
    expect(resolvePackageExportPath(undefined, '/x')).toBeUndefined();
    expect(resolvePackageExportPath('./dist/index.js', '/x')).toBeUndefined();

    const exportsField = {
      './feature': './dist/feature.js',
      './condition-a': {
        import: './dist/import.js',
        default: './dist/default.js',
        require: './dist/require.js',
        node: './dist/node.js',
        bun: './dist/bun.js',
        browser: './dist/browser.js',
        types: './dist/types.d.ts',
      },
      './condition-b': {
        default: './dist/default-only.js',
      },
      './condition-c': {
        require: './dist/require-only.js',
      },
      './condition-d': {
        types: './dist/types-only.d.ts',
      },
    } as const;

    expect(resolvePackageExportPath(exportsField, '/feature')).toBe('./dist/feature.js');
    expect(resolvePackageExportPath(exportsField, '/condition-a')).toBe('./dist/import.js');
    expect(resolvePackageExportPath(exportsField, '/condition-b')).toBe('./dist/default-only.js');
    expect(resolvePackageExportPath(exportsField, '/condition-c')).toBe('./dist/require-only.js');
    expect(resolvePackageExportPath(exportsField, '/condition-d')).toBe('./dist/types-only.d.ts');
    expect(resolvePackageExportPath(exportsField, '/missing')).toBeUndefined();
  });
});
