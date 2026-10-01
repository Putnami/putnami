import { describe, expect, it } from 'bun:test';
import { existsSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  readStaticBuildVersion,
  restampableVersion,
  restampBuildVersion,
  STATIC_BUILD_VERSION_FILE,
  writeStaticBuildVersion,
} from '../../src/ssr/static-build-version';

function tempDir(): string {
  return mkdtempSync(join(tmpdir(), 'putnami-static-version-'));
}

describe('static build version record', () => {
  it('reads back the version it records', () => {
    const dir = tempDir();
    writeStaticBuildVersion(dir, '0.0.0-20260927164104-94392288');
    expect(readStaticBuildVersion(dir)).toBe('0.0.0-20260927164104-94392288');
    rmSync(dir, { recursive: true, force: true });
  });

  it('creates the static directory when no page was written yet', () => {
    const dir = join(tempDir(), 'static');
    writeStaticBuildVersion(dir, '0.1.0-abc1234');
    expect(readStaticBuildVersion(dir)).toBe('0.1.0-abc1234');
    rmSync(join(dir, '..'), { recursive: true, force: true });
  });

  it('removes a stale record when the build has no version', () => {
    const dir = tempDir();
    writeStaticBuildVersion(dir, '0.1.0-abc1234');
    writeStaticBuildVersion(dir, undefined);
    expect(existsSync(join(dir, STATIC_BUILD_VERSION_FILE))).toBe(false);
    expect(readStaticBuildVersion(dir)).toBeUndefined();
    rmSync(dir, { recursive: true, force: true });
  });

  it('reads no version from a missing or malformed record', () => {
    const dir = tempDir();
    expect(readStaticBuildVersion(dir)).toBeUndefined();
    writeFileSync(join(dir, STATIC_BUILD_VERSION_FILE), '{not json');
    expect(readStaticBuildVersion(dir)).toBeUndefined();
    writeFileSync(join(dir, STATIC_BUILD_VERSION_FILE), '{"version":42}');
    expect(readStaticBuildVersion(dir)).toBeUndefined();
    rmSync(dir, { recursive: true, force: true });
  });
});

describe('restampBuildVersion', () => {
  it('replaces every occurrence of the rendered version with the current one', () => {
    const html = '<footer><span>v0.1.0-old</span><meta content="0.1.0-old"></footer>';
    expect(restampBuildVersion(html, '0.1.0-old', '0.1.0-new')).toBe(
      '<footer><span>v0.1.0-new</span><meta content="0.1.0-new"></footer>',
    );
  });

  it('leaves the HTML unchanged without both versions or when they match', () => {
    const html = '<span>v0.1.0-old</span>';
    expect(restampBuildVersion(html, undefined, '0.1.0-new')).toBe(html);
    expect(restampBuildVersion(html, '0.1.0-old', undefined)).toBe(html);
    expect(restampBuildVersion(html, '0.1.0-old', '0.1.0-old')).toBe(html);
  });
});

describe('restampableVersion', () => {
  it('accepts a version that ends with its commit suffix', () => {
    expect(restampableVersion({ version: '0.1.0-20260927164104-94392288', suffix: '20260927164104-94392288' })).toBe(
      '0.1.0-20260927164104-94392288',
    );
  });

  it('rejects a bare release tag and a version without a suffix', () => {
    expect(restampableVersion({ version: '1.0.0', suffix: '20260927164104-94392288' })).toBeUndefined();
    expect(restampableVersion({ version: '1.0.0' })).toBeUndefined();
    expect(restampableVersion(undefined)).toBeUndefined();
  });

  it('keeps unrelated release numbers in the page when it restamps', () => {
    const rendered = '0.1.0-20260927164104-94392288';
    const html = `<pre>"@putnami/web": "1.0.0"</pre><footer>v${rendered}</footer>`;
    expect(restampBuildVersion(html, rendered, '0.1.0-20260928090000-abcdef012')).toBe(
      '<pre>"@putnami/web": "1.0.0"</pre><footer>v0.1.0-20260928090000-abcdef012</footer>',
    );
  });
});
