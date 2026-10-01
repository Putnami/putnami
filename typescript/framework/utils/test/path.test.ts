import { describe, expect, it, test } from 'bun:test';
import { resolve as nodeResolve, sep } from 'node:path';
import {
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
} from '../src';

/** `path` with the host separator: these functions return native paths. */
const native = (path: string): string => path.replaceAll('/', sep);

describe('path.utils', () => {
  describe('joinPath / join', () => {
    it('joins path segments with the default separator', () => {
      expect(joinPath('folder', 'subfolder', 'file.txt')).toMatch(/folder[/\\]subfolder[/\\]file.txt/);
    });

    it('removes redundant separators', () => {
      expect(joinPath('folder/', '/subfolder/', '/file.txt')).toBe(native('folder/subfolder/file.txt'));
    });

    it('ignores empty segments', () => {
      expect(joinPath('', 'folder', '', 'file.txt')).toBe(native('folder/file.txt'));
    });

    it('handles relative paths correctly', () => {
      expect(joinPath('folder', '..', 'anotherFolder')).toBe('anotherFolder');
    });

    it('handles current directory symbol (.)', () => {
      expect(joinPath('.', 'folder', 'file.txt')).toBe(native('folder/file.txt'));
    });
  });

  describe('toPosixPath / joinPosixPath', () => {
    it('converts Windows separators to forward slashes', () => {
      expect(toPosixPath('..\\..\\src\\api\\get')).toBe('../../src/api/get');
      expect(toPosixPath('src/app/page.tsx')).toBe('src/app/page.tsx');
    });

    it('joins Windows-separated segments with forward slashes on every platform', () => {
      expect(joinPosixPath('..\\..\\src\\app', './not-found')).toBe('../../src/app/not-found');
      expect(joinPosixPath('..\\..', 'src\\api', 'get.ts')).toBe('../../src/api/get.ts');
      expect(joinPosixPath('', '../src/api/get')).toBe('../src/api/get');
    });
  });

  describe('getDirectoryName / dirname', () => {
    test('returns the directory name of a simple path', () => {
      expect(getDirectoryName('folder/subfolder/file.txt')).toBe('folder/subfolder');
    });

    test('returns the root if the path is a root directory', () => {
      const rootPath = process.platform === 'win32' ? 'C:\\' : '/';
      expect(getDirectoryName(`${rootPath}file.txt`)).toBe(rootPath.trim());
    });

    test('returns "." for a bare filename', () => {
      expect(getDirectoryName('file.txt')).toBe('.');
    });

    test('handles paths with trailing separators', () => {
      expect(getDirectoryName('folder/subfolder/')).toBe('folder');
    });

    test('returns "." if no directories are given', () => {
      expect(getDirectoryName('')).toBe('.');
    });

    test('handles complex paths correctly', () => {
      expect(getDirectoryName('/folder/subfolder/anotherFolder/file.txt')).toBe('/folder/subfolder/anotherFolder');
    });

    test('returns the parent directory for relative paths', () => {
      expect(getDirectoryName('./folder/file.txt')).toBe('./folder');
    });
  });

  describe('relativePath / relative', () => {
    it('returns an empty string if both paths are the same', () => {
      expect(relativePath('/path/to/directory', '/path/to/directory')).toBe('');
    });

    it('returns relative path to a subdirectory', () => {
      expect(relativePath('/path/to', '/path/to/directory')).toBe('directory');
    });

    it('returns relative path to a parent directory', () => {
      expect(relativePath('/path/to/directory', '/path')).toBe(native('../..'));
    });

    it('returns relative path between siblings', () => {
      expect(relativePath('/path/to/dir1', '/path/to/dir2')).toBe(native('../dir2'));
    });

    it('returns relative path with complex paths', () => {
      expect(relativePath('/path/to/some/longer/directory', '/path/to/another/dir')).toBe(
        native('../../../another/dir'),
      );
    });

    it('handles paths with trailing separators', () => {
      expect(relativePath('/path/to/directory/', '/path/to/directory/file')).toBe('file');
    });

    it('handles relative paths', () => {
      expect(relativePath('path/to/directory', 'path/to/another')).toBe(native('../another'));
    });

    it('relative is an alias for relativePath', () => {
      expect(relativePath('/a/b', '/a/c')).toBe(relativePath('/a/b', '/a/c'));
    });
  });

  describe('resolvePath / resolve', () => {
    it('resolves a sequence of paths into an absolute path', () => {
      expect(resolvePath('/path', 'to', 'directory')).toBe(nodeResolve('/path', 'to', 'directory'));
    });

    it('resolves an absolute path', () => {
      expect(resolvePath('/absolute', '/path', 'to', 'directory')).toBe(
        nodeResolve('/absolute', '/path', 'to', 'directory'),
      );
    });

    it('ignores previous paths when an absolute path is encountered', () => {
      expect(resolvePath('/first', '/second')).toBe(nodeResolve('/first', '/second'));
    });

    it('handles ".." to move up directories', () => {
      expect(resolvePath('/path/to', '..', 'directory')).toBe(nodeResolve('/path/to', '..', 'directory'));
    });

    it('handles "." as the current directory', () => {
      expect(resolvePath('/path', '.', 'directory')).toBe(nodeResolve('/path', '.', 'directory'));
    });

    it('returns current working dir when no paths are given', () => {
      expect(resolvePath()).toBe(nodeResolve('.'));
    });

    it('handles empty path segments as ignored', () => {
      expect(resolvePath('/path', '', 'to', '', 'directory')).toBe(nodeResolve('/path', '', 'to', '', 'directory'));
    });
  });

  describe('getBaseName / basename', () => {
    it('returns the filename', () => {
      expect(getBaseName('/path/to/file.txt')).toBe('file.txt');
    });

    it('removes extension when suffix provided', () => {
      expect(getBaseName('/path/to/file.txt', '.txt')).toBe('file');
    });
  });

  describe('getExtension / extname', () => {
    it('returns the file extension', () => {
      expect(getExtension('/path/to/file.txt')).toBe('.txt');
    });

    it('returns empty string for no extension', () => {
      expect(getExtension('/path/to/file')).toBe('');
    });
  });

  describe('normalizePath / normalize', () => {
    it('resolves .. segments', () => {
      expect(normalizePath('/path/to/../file.txt')).toBe(native('/path/file.txt'));
    });

    it('resolves . segments', () => {
      expect(normalizePath('/path/./to/file.txt')).toBe(native('/path/to/file.txt'));
    });
  });

  describe('isAbsolutePath / isAbsolute', () => {
    it('returns true for absolute paths', () => {
      expect(isAbsolutePath('/path/to/file')).toBe(true);
    });

    it('returns false for relative paths', () => {
      expect(isAbsolutePath('relative/path')).toBe(false);
    });
  });
});
