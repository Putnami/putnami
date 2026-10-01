/**
 * Site-content bundle pipeline tests.
 *
 * Covers the TS reimplementation of the `site-content-bundle/v1` contract
 * (ported from the `protocols/sitecontent` Go fixture corpus so both sides
 * enforce identical rules), the lock format, the registry seams, `content
 * bump`, and the generate-time materialization flow — all against local
 * file/function doubles: no network, no real registry.
 */
import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/spectest';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { bumpContentLock } from '../src/lib/content/bump';
import { formatContentLock, parseContentLock } from '../src/lib/content/lock';
import {
  deriveSiteLocalPrefixes,
  materializeContentBundles,
  type MaterializeOptions,
} from '../src/lib/content/materialize';
import {
  registryRef,
  resolveChannelFromRegistry,
  resolveRegistryToken,
  validateRegistryUrl,
} from '../src/lib/content/registry';
import {
  compatibleFormatVersion,
  ErrorCodes,
  type Manifest,
  parseManifest,
  prefixesConflict,
  urlInPrefix,
  validateManifest,
  validDigest,
  validMountPrefix,
  validRelPath,
} from '../src/lib/content/sitecontent';
import { readTarEntries, type TarWriteEntry, writeTarEntries } from '../src/lib/content/tar';
import { sha256Hex, verifyBundleArchive } from '../src/lib/content/verify';

const PROJECT_ROOT = join(import.meta.dir, '..');
const WORKSPACE_ROOT = join(PROJECT_ROOT, '..', '..');

// ---------------------------------------------------------------------------
// Fixture helpers — a local producer double (tar.gz with bundle.json at root)
// ---------------------------------------------------------------------------

const bytes = (text: string) => new TextEncoder().encode(text);

function manifestFor(files: Record<string, string>, overrides: Partial<Manifest> = {}): Manifest {
  return {
    formatVersion: '1.0',
    name: 'cloud-docs',
    mounts: [{ urlPrefix: '/docs/99-cloud' }],
    source: { repo: 'acme-platform', commit: 'abc123' },
    files: Object.entries(files).map(([path, data]) => ({ path, digest: sha256Hex(bytes(data)) })),
    ...overrides,
  };
}

function buildBlob(manifest: unknown, entries: TarWriteEntry[]): Uint8Array {
  const tar = writeTarEntries([{ name: 'bundle.json', data: JSON.stringify(manifest) }, ...entries]);
  return Bun.gzipSync(tar);
}

function blobFor(files: Record<string, string>, overrides: Partial<Manifest> = {}): Uint8Array {
  return buildBlob(
    manifestFor(files, overrides),
    Object.entries(files).map(([name, data]) => ({ name, data })),
  );
}

const codes = (result: { diagnostics: { code: string }[] }) => result.diagnostics.map((d) => d.code);

// ---------------------------------------------------------------------------
// Contract rules — ported from the protocols/sitecontent fixture corpus
// ---------------------------------------------------------------------------

describe('sitecontent contract (Go parity)', () => {
  it('pins the shared fixture digest so TS and Go hash identically', () => {
    // protocols/sitecontent/fixtures/payload/valid-bundle.json
    expect(sha256Hex(bytes('hello cloud\n'))).toBe('1a31f208b4e0b7759528b7af8580d7beb300ba849259987113d8b8d7718ff4a5');
  });

  it('verifies a valid bundle (fixtures/payload/valid-bundle)', () => {
    const files = { 'docs/99-cloud/index.md': 'hello cloud\n', 'docs/99-cloud/guides/start.md': '# Start\n' };
    const result = verifyBundleArchive(blobFor(files));
    expect(result.diagnostics).toEqual([]);
    expect(result.bundle?.manifest.name).toBe('cloud-docs');
    expect(new TextDecoder().decode(result.bundle?.files.get('docs/99-cloud/index.md'))).toBe('hello cloud\n');
  });

  it('rejects a traversal entry (fixtures/payload/traversal-attempt)', () => {
    const manifest = manifestFor({ 'docs/99-cloud/index.md': 'hello cloud\n' });
    const blob = buildBlob(manifest, [{ name: '../docs/99-cloud/escape.md', data: 'escape\n' }]);
    expect(codes(verifyBundleArchive(blob))).toEqual([ErrorCodes.InvalidPath, ErrorCodes.MissingFile]);
  });

  it('rejects a file outside every mount (fixtures/payload/mount-escape)', () => {
    const manifest = manifestFor({ 'docs/99-cloud/index.md': 'hello cloud\n' });
    const blob = buildBlob(manifest, [{ name: 'blog/index.md', data: 'hello cloud\n' }]);
    expect(codes(verifyBundleArchive(blob))).toEqual([ErrorCodes.PathOutsideMount, ErrorCodes.MissingFile]);
  });

  it('rejects symlinks (fixtures/payload/symlink)', () => {
    const manifest = manifestFor({ 'docs/99-cloud/index.md': 'hello cloud\n' });
    const blob = buildBlob(manifest, [{ name: 'docs/99-cloud/index.md', typeflag: '2', linkname: '../../secret.md' }]);
    expect(codes(verifyBundleArchive(blob))).toEqual([ErrorCodes.InvalidSymlink, ErrorCodes.MissingFile]);
  });

  it('rejects an unlisted payload file (fixtures/payload/unlisted-file)', () => {
    const manifest = manifestFor({ 'docs/99-cloud/index.md': 'hello cloud\n' });
    const blob = buildBlob(manifest, [
      { name: 'docs/99-cloud/index.md', data: 'hello cloud\n' },
      { name: 'docs/99-cloud/extra.md', data: 'extra\n' },
    ]);
    expect(codes(verifyBundleArchive(blob))).toEqual([ErrorCodes.UnlistedFile]);
  });

  it('rejects a digest mismatch (fixtures/payload/digest-mismatch)', () => {
    const manifest = manifestFor({ 'docs/99-cloud/index.md': 'hello cloud\n' });
    const blob = buildBlob(manifest, [{ name: 'docs/99-cloud/index.md', data: 'changed\n' }]);
    expect(codes(verifyBundleArchive(blob))).toEqual([ErrorCodes.DigestMismatch]);
  });

  it('gates on a newer format major (fixtures/payload/newer-format-version)', () => {
    const result = verifyBundleArchive(
      blobFor({ 'docs/99-cloud/index.md': 'hello cloud\n' }, { formatVersion: '2.0' }),
    );
    expect(result.newerFormatMajor).toBe(true);
    expect(result.bundle).toBeNull();
    expect(codes(result)).toEqual([ErrorCodes.InvalidFormatVersion]);
  });

  it('gates a newer major before strict-decoding its unknown manifest shape', () => {
    const result = verifyBundleArchive(
      buildBlob({ formatVersion: '2.0', replacementManifest: { layout: 'not-a-v1-shape' } }, []),
    );
    expect(result.newerFormatMajor).toBe(true);
    expect(result.bundle).toBeNull();
    expect(codes(result)).toEqual([ErrorCodes.InvalidFormatVersion]);
  });

  it('fails closed when the version envelope is not a string', () => {
    const result = verifyBundleArchive(buildBlob({ formatVersion: 2 }, []));
    expect(result.newerFormatMajor).toBe(false);
    expect(result.bundle).toBeNull();
    expect(result.diagnostics.length).toBeGreaterThan(0);
  });

  it('rejects unknown manifest fields (fixtures/invalid/unknown-field)', () => {
    const manifest = { ...manifestFor({ 'docs/99-cloud/index.md': 'x\n' }), extra: true };
    const parsed = parseManifest(JSON.stringify(manifest));
    expect(parsed.manifest).toBeNull();
    expect(parsed.diagnostics.map((d) => d.code)).toEqual([ErrorCodes.UnknownField]);
  });

  it('rejects duplicate manifest paths and intra-bundle mount overlap', () => {
    const dup = manifestFor({ 'docs/99-cloud/index.md': 'x\n' });
    dup.files.push({ ...dup.files[0] });
    expect(validateManifest(dup).map((d) => d.code)).toContain(ErrorCodes.DuplicateFile);

    const overlapping = manifestFor(
      { 'docs/99-cloud/index.md': 'x\n' },
      { mounts: [{ urlPrefix: '/docs/99-cloud' }, { urlPrefix: '/docs/99-cloud/sub' }] },
    );
    expect(validateManifest(overlapping).map((d) => d.code)).toContain(ErrorCodes.MountCollision);
  });

  it('matches the Go prefix-overlap semantics', () => {
    expect(urlInPrefix('/docs/cloud', '/docs')).toBe(true);
    expect(urlInPrefix('/docs-cloud', '/docs')).toBe(false);
    expect(prefixesConflict('/docs/a', '/docs/a')).toBe(true);
    expect(prefixesConflict('/docs/a', '/docs/a/b')).toBe(true);
    expect(prefixesConflict('/docs/a', '/docs/b')).toBe(false);
    expect(prefixesConflict('/docs/a', '/')).toBe(true);
  });

  it('matches the Go path/mount/digest grammar', () => {
    expect(validRelPath('docs/a/b.md')).toBe(true);
    for (const bad of ['', '/abs.md', 'a/../b.md', 'a//b.md', 'a\\b.md', './a.md', '..']) {
      expect(validRelPath(bad)).toBe(false);
    }
    expect(validMountPrefix('/docs/99-cloud')).toBe(true);
    for (const bad of ['/', '', 'docs', '/docs/', '/Docs', '/docs/..', '/docs//x']) {
      expect(validMountPrefix(bad)).toBe(false);
    }
    expect(validDigest(sha256Hex(bytes('x')))).toBe(true);
    expect(validDigest('SHA256:abc')).toBe(false);
    expect(compatibleFormatVersion('1.0')).toBe(true);
    expect(compatibleFormatVersion('1')).toBe(true);
    expect(compatibleFormatVersion('2.0')).toBe(false);
    expect(compatibleFormatVersion('nope')).toBe(false);
  });
});

describe('tar reader', () => {
  it('round-trips entries and applies PAX path overrides', () => {
    const pax = `29 path=docs/renamed/file.md\n`;
    const raw = writeTarEntries([
      { name: 'PaxHeader', typeflag: 'x', data: pax },
      { name: 'short-name.md', data: 'content\n' },
    ]);
    const entries = readTarEntries(raw);
    expect(entries).toHaveLength(1);
    expect(entries[0].name).toBe('docs/renamed/file.md');
    expect(new TextDecoder().decode(entries[0].data)).toBe('content\n');
  });

  it('throws on a truncated archive and maps non-gzip blobs to a parse error', () => {
    const raw = writeTarEntries([{ name: 'a.md', data: 'x'.repeat(600) }]);
    expect(() => readTarEntries(raw.subarray(0, 700))).toThrow(/truncated/);
    expect(codes(verifyBundleArchive(bytes('not gzip')))).toEqual([ErrorCodes.ParseError]);
  });
});

// ---------------------------------------------------------------------------
// Lock format
// ---------------------------------------------------------------------------

describe('content.lock.json', () => {
  const entry = {
    name: 'cloud-docs',
    package: 'site-content/cloud',
    channelHint: 'stable',
    version: '1.2.3',
    digest: sha256Hex(bytes('blob')),
    registry: 'https://put.putnami.dev',
  };

  it('parses the committed lock (empty lock is a valid clean no-op)', () => {
    const lock = parseContentLock(readFileSync(join(PROJECT_ROOT, 'content.lock.json'), 'utf8'));
    expect(Array.isArray(lock.bundles)).toBe(true);
  });

  it('accepts a full entry and rejects defects', () => {
    expect(parseContentLock(JSON.stringify({ bundles: [entry] })).bundles).toHaveLength(1);
    expect(() => parseContentLock(JSON.stringify({ bundles: [{ ...entry, digest: 'short' }] }))).toThrow(/digest/);
    expect(() => parseContentLock(JSON.stringify({ bundles: [{ ...entry, registry: '' }] }))).toThrow(/registry/);
    expect(() => parseContentLock(JSON.stringify({ bundles: [entry, entry] }))).toThrow(/duplicate/);
    expect(() => parseContentLock('{}')).toThrow(/bundles/);
    expect(() => parseContentLock('nope')).toThrow(/JSON/);
  });

  it('formats with a stable field order for reviewable bump diffs', () => {
    const text = formatContentLock({ bundles: [entry] });
    expect(text.endsWith('\n')).toBe(true);
    const keys = Object.keys((JSON.parse(text) as { bundles: Record<string, string>[] }).bundles[0]);
    expect(keys).toEqual(['name', 'package', 'channelHint', 'version', 'digest', 'registry']);
  });

  it('is pinned into the generate cache key via putnami.json filePatterns', () => {
    // Cache-key completeness invariant: same lock → same key,
    // lock bump → new key. The lock MUST stay a declared hash input.
    const config = JSON.parse(readFileSync(join(PROJECT_ROOT, 'putnami.json'), 'utf8')) as {
      options: Record<string, { filePatterns?: string[] }>;
    };
    expect(config.options['/typescript/extension'].filePatterns).toContain('content.lock.json');
  });
});

// ---------------------------------------------------------------------------
// Registry seams
// ---------------------------------------------------------------------------

describe('registry seams', () => {
  it('maps package names to registry refs (RegistryRef parity)', () => {
    expect(registryRef('site-content/cloud')).toEqual({ namespace: 'site-content', pkg: 'cloud' });
    expect(registryRef('@putnami/go')).toEqual({ namespace: 'putnami', pkg: 'go' });
    expect(registryRef('cli')).toEqual({ namespace: 'putnami', pkg: 'cli' });
  });

  it('accepts https and loopback http, rejects plaintext http', () => {
    expect(validateRegistryUrl('https://put.putnami.dev').host).toBe('put.putnami.dev');
    expect(validateRegistryUrl('http://localhost:8080').port).toBe('8080');
    expect(validateRegistryUrl('http://127.0.0.1:9999').hostname).toBe('127.0.0.1');
    expect(() => validateRegistryUrl('http://evil.example.com')).toThrow(/plaintext/);
    expect(() => validateRegistryUrl('ftp://put.putnami.dev')).toThrow(/scheme/);
    expect(() => validateRegistryUrl('not a url')).toThrow(/invalid registry URL/);
  });

  it('treats every registry-token failure mode as anonymous (fail-open to anonymous, never to a bad header)', () => {
    expect(resolveRegistryToken('put.putnami.dev', () => ({ exitCode: 0, stdout: 'tok-123\n' }))).toBe('tok-123');
    expect(resolveRegistryToken('put.putnami.dev', () => ({ exitCode: 1, stdout: 'tok-123' }))).toBeUndefined();
    expect(resolveRegistryToken('put.putnami.dev', () => ({ exitCode: 0, stdout: '' }))).toBeUndefined();
    // A human status line on stdout is not a bearer (protocols/registry ValidBearer).
    expect(resolveRegistryToken('put.putnami.dev', () => ({ exitCode: 0, stdout: 'not signed in\n' }))).toBeUndefined();
    expect(
      resolveRegistryToken('put.putnami.dev', () => {
        throw new Error('no putnami on PATH');
      }),
    ).toBeUndefined();
  });

  it('fails during resolution when the registry advertises no version (never writes an unparseable lock)', async () => {
    // parseContentLock rejects an empty version, so a pointer without one must
    // fail HERE rather than get written into content.lock.json.
    const realFetch = globalThis.fetch;
    globalThis.fetch = (() =>
      Promise.resolve(
        new Response(JSON.stringify({ version: '', digest: sha256Hex(bytes('blob')) }), { status: 200 }),
      )) as typeof fetch;
    try {
      await expect(
        resolveChannelFromRegistry('site-content/cloud', 'stable', 'https://put.putnami.dev', () => ({
          exitCode: 1,
          stdout: '',
        })),
      ).rejects.toThrow(/no version/i);
    } finally {
      globalThis.fetch = realFetch;
    }
  });

  it('resolves through the channel pointer then the blob by digest, never the binary download route', async () => {
    const blob = bytes('bundle-bytes');
    const digest = sha256Hex(blob);
    const requested: string[] = [];
    const realFetch = globalThis.fetch;
    globalThis.fetch = ((input: string | URL | Request) => {
      const url = String(input instanceof Request ? input.url : input);
      requested.push(url);
      if (url.endsWith('/channels/canary')) {
        return Promise.resolve(new Response(JSON.stringify({ version: '0.0.0-1-abc', digest }), { status: 200 }));
      }
      return Promise.resolve(new Response(blob, { status: 200 }));
    }) as typeof fetch;
    try {
      const resolved = await resolveChannelFromRegistry(
        'cloud/doc-contents-platform',
        'canary',
        'https://put.putnami.dev',
        () => ({ exitCode: 1, stdout: '' }),
      );
      expect(resolved.version).toBe('0.0.0-1-abc');
      expect(sha256Hex(resolved.blob)).toBe(digest);
      expect(requested).toEqual([
        'https://put.putnami.dev/cloud/doc-contents-platform/channels/canary',
        `https://put.putnami.dev/cloud/doc-contents-platform/blobs/${digest}`,
      ]);
    } finally {
      globalThis.fetch = realFetch;
    }
  });
});

// ---------------------------------------------------------------------------
// content bump — the sole channel-resolution point
// ---------------------------------------------------------------------------

describe('content bump', () => {
  const lockEntry = {
    name: 'cloud-docs',
    package: 'site-content/cloud',
    channelHint: 'stable',
    version: '1.0.0',
    digest: sha256Hex(bytes('old-blob')),
    registry: 'https://put.putnami.dev',
  };

  it('rewrites version + digest from the resolved bytes (digest computed, never trusted)', async () => {
    const blob = bytes('new-blob');
    const result = await bumpContentLock({ bundles: [lockEntry] }, () => Promise.resolve({ version: '1.1.0', blob }));
    expect(result.changes).toHaveLength(1);
    expect(result.changes[0].toDigest).toBe(sha256Hex(blob));
    expect(result.lock.bundles[0]).toEqual({ ...lockEntry, version: '1.1.0', digest: sha256Hex(blob) });
    expect(result.lock.bundles[0].channelHint).toBe('stable');
  });

  it('reports no change when the channel head matches the pin', async () => {
    const result = await bumpContentLock({ bundles: [lockEntry] }, () =>
      Promise.resolve({ version: '1.0.0', blob: bytes('old-blob') }),
    );
    expect(result.changes).toEqual([]);
    expect(result.lock.bundles).toEqual([lockEntry]);
  });

  it('honors the --bundle filter', async () => {
    const result = await bumpContentLock(
      { bundles: [lockEntry] },
      () => Promise.resolve({ version: '9.9.9', blob: bytes('other') }),
      ['some-other-bundle'],
    );
    expect(result.changes).toEqual([]);
  });

  it('refuses to write a lock entry with no resolved version (parseContentLock would reject it)', async () => {
    await expect(
      bumpContentLock({ bundles: [lockEntry] }, () => Promise.resolve({ version: '', blob: bytes('x') })),
    ).rejects.toThrow(/empty version/);
  });
});

// ---------------------------------------------------------------------------
// Materialization — generate-time flow against local doubles
// ---------------------------------------------------------------------------

interface Site {
  projectRoot: string;
  cacheDir: string;
  warnings: string[];
  options: (overrides?: Partial<MaterializeOptions>) => MaterializeOptions;
}

function makeSite(lockBundles: unknown[]): Site {
  const root = mkdtempSync(join(tmpdir(), 'putnami-content-'));
  const projectRoot = join(root, 'site');
  const cacheDir = join(root, 'blob-cache');
  mkdirSync(projectRoot, { recursive: true });
  writeFileSync(join(projectRoot, 'content.lock.json'), JSON.stringify({ bundles: lockBundles }));
  const warnings: string[] = [];
  return {
    projectRoot,
    cacheDir,
    warnings,
    options: (overrides = {}) => ({
      projectRoot,
      workspaceRoot: root,
      cacheDir,
      ci: true,
      siteLocalPrefixes: ['/docs/01-getting-started', '/install.sh'],
      log: (message) => warnings.push(message),
      fetchBlob: () => Promise.reject(new Error('unexpected fetch')),
      ...overrides,
    }),
  };
}

function lockEntryFor(blob: Uint8Array, overrides: Record<string, string> = {}) {
  return {
    name: 'cloud-docs',
    package: 'site-content/cloud',
    channelHint: 'stable',
    version: '1.0.0',
    digest: sha256Hex(blob),
    registry: 'https://put.putnami.dev',
    ...overrides,
  };
}

const FILES = { 'docs/99-cloud/index.md': '# Cloud\n', 'docs/99-cloud/guides/start.md': 'start\n' };

describe('materializeContentBundles', () => {
  it('is a clean no-op on an empty lock', async () => {
    const site = makeSite([]);
    const result = await materializeContentBundles(
      site.options({ fetchBlob: () => Promise.reject(new Error('must not fetch')) }),
    );
    expect(result).toEqual({ assets: {}, mounted: [], skipped: [] });
    expect(existsSync(join(site.projectRoot, '.gen'))).toBe(false);
  });

  it('mounts a bundle into .gen/public and public, registers assets, and caches the blob', async () => {
    const blob = blobFor(FILES);
    const site = makeSite([lockEntryFor(blob)]);
    const result = await materializeContentBundles(site.options({ fetchBlob: () => Promise.resolve(blob) }));

    expect(result.mounted).toEqual(['cloud-docs']);
    const genPath = join(site.projectRoot, '.gen', 'public', 'docs', '99-cloud', 'index.md');
    const publicPath = join(site.projectRoot, 'public', 'docs', '99-cloud', 'index.md');
    expect(readFileSync(genPath, 'utf8')).toBe('# Cloud\n');
    expect(readFileSync(publicPath, 'utf8')).toBe('# Cloud\n');
    expect(result.assets[join('public', 'docs', '99-cloud', 'index.md')]).toBe(genPath);
    expect(Object.keys(result.assets)).toHaveLength(2);
    // Blob cached by digest → the next build is offline-safe.
    expect(existsSync(join(site.cacheDir, sha256Hex(blob)))).toBe(true);
  });

  it('produces byte-identical output for the same lock (determinism invariant)', async () => {
    const blob = blobFor(FILES);
    const read = async () => {
      const site = makeSite([lockEntryFor(blob)]);
      const result = await materializeContentBundles(site.options({ fetchBlob: () => Promise.resolve(blob) }));
      return Object.keys(result.assets)
        .sort()
        .map((key) => [key, sha256Hex(new Uint8Array(readFileSync(result.assets[key])))]);
    };
    expect(await read()).toEqual(await read());
  });

  it('clears stale files under a bundle mount before unpacking', async () => {
    const blob = blobFor(FILES);
    const site = makeSite([lockEntryFor(blob)]);
    const stale = join(site.projectRoot, '.gen', 'public', 'docs', '99-cloud', 'stale.md');
    mkdirSync(join(site.projectRoot, '.gen', 'public', 'docs', '99-cloud'), { recursive: true });
    writeFileSync(stale, 'stale');
    await materializeContentBundles(site.options({ fetchBlob: () => Promise.resolve(blob) }));
    expect(existsSync(stale)).toBe(false);
  });

  it('prunes a mount dropped from the lock across runs (no stale bundle output ships)', async () => {
    const blob = blobFor(FILES);
    const site = makeSite([lockEntryFor(blob)]);
    const genMount = join(site.projectRoot, '.gen', 'public', 'docs', '99-cloud');
    const publicMount = join(site.projectRoot, 'public', 'docs', '99-cloud');

    await materializeContentBundles(site.options({ fetchBlob: () => Promise.resolve(blob) }));
    expect(existsSync(genMount)).toBe(true);
    expect(existsSync(publicMount)).toBe(true);

    // The lock empties (bundle removed) in the SAME working tree — a cache miss
    // would otherwise re-capture the stale mount into the new key.
    writeFileSync(join(site.projectRoot, 'content.lock.json'), JSON.stringify({ bundles: [] }));
    const result = await materializeContentBundles(
      site.options({ fetchBlob: () => Promise.reject(new Error('must not fetch')) }),
    );
    expect(result.mounted).toEqual([]);
    expect(existsSync(genMount)).toBe(false);
    expect(existsSync(publicMount)).toBe(false);
  });

  it('prunes a mount replaced by a differently-mounted bundle across runs', async () => {
    const first = blobFor(FILES); // mounts /docs/99-cloud
    const second = blobFor({ 'docs/98-cloud/index.md': '# Moved\n' }, { mounts: [{ urlPrefix: '/docs/98-cloud' }] });
    const site = makeSite([lockEntryFor(first)]);
    await materializeContentBundles(site.options({ fetchBlob: () => Promise.resolve(first) }));
    expect(existsSync(join(site.projectRoot, '.gen', 'public', 'docs', '99-cloud'))).toBe(true);

    writeFileSync(join(site.projectRoot, 'content.lock.json'), JSON.stringify({ bundles: [lockEntryFor(second)] }));
    await materializeContentBundles(site.options({ fetchBlob: () => Promise.resolve(second) }));
    expect(existsSync(join(site.projectRoot, '.gen', 'public', 'docs', '99-cloud'))).toBe(false);
    expect(existsSync(join(site.projectRoot, '.gen', 'public', 'docs', '98-cloud'))).toBe(true);
  });

  it('serves from the digest-keyed cache when offline (offline-with-cache)', async () => {
    const blob = blobFor(FILES);
    const site = makeSite([lockEntryFor(blob)]);
    mkdirSync(site.cacheDir, { recursive: true });
    writeFileSync(join(site.cacheDir, sha256Hex(blob)), blob);
    const result = await materializeContentBundles(
      site.options({ ci: true, fetchBlob: () => Promise.reject(new Error('offline')) }),
    );
    expect(result.mounted).toEqual(['cloud-docs']);
  });

  it('refetches when a cached blob is corrupted (content address is the authority)', async () => {
    const blob = blobFor(FILES);
    const site = makeSite([lockEntryFor(blob)]);
    mkdirSync(site.cacheDir, { recursive: true });
    writeFileSync(join(site.cacheDir, sha256Hex(blob)), 'corrupted');
    const result = await materializeContentBundles(site.options({ fetchBlob: () => Promise.resolve(blob) }));
    expect(result.mounted).toEqual(['cloud-docs']);
    expect(sha256Hex(new Uint8Array(readFileSync(join(site.cacheDir, sha256Hex(blob)))))).toBe(sha256Hex(blob));
  });

  it('hard-fails in CI when a pinned bundle is unfetchable (offline-without-cache)', async () => {
    const site = makeSite([lockEntryFor(bytes('missing'))]);
    await expect(
      materializeContentBundles(site.options({ ci: true, fetchBlob: () => Promise.reject(new Error('offline')) })),
    ).rejects.toThrow(/unfetchable/);
  });

  it('warns and skips in local dev when a pinned bundle is unfetchable', async () => {
    const site = makeSite([lockEntryFor(bytes('missing'))]);
    const result = await materializeContentBundles(
      site.options({ ci: false, fetchBlob: () => Promise.reject(new Error('offline')) }),
    );
    expect(result.mounted).toEqual([]);
    expect(result.skipped).toHaveLength(1);
    expect(site.warnings.join('\n')).toContain('skipping in local dev');
  });

  it('rejects a fetched blob whose digest does not match the pin (CI fails, dev skips)', async () => {
    const good = blobFor(FILES);
    const tampered = blobFor({ 'docs/99-cloud/index.md': 'tampered\n' });
    const ciSite = makeSite([lockEntryFor(good)]);
    await expect(
      materializeContentBundles(ciSite.options({ ci: true, fetchBlob: () => Promise.resolve(tampered) })),
    ).rejects.toThrow(/does not match the pinned digest/);

    const devSite = makeSite([lockEntryFor(good)]);
    const result = await materializeContentBundles(
      devSite.options({ ci: false, fetchBlob: () => Promise.resolve(tampered) }),
    );
    expect(result.mounted).toEqual([]);
    expect(devSite.warnings.join('\n')).toContain('does not match the pinned digest');
    // A tampered blob must never enter the digest-keyed cache.
    expect(existsSync(join(devSite.cacheDir, sha256Hex(good)))).toBe(false);
  });

  it('hard-fails in CI on an invalid payload (traversal entry)', async () => {
    const manifest = manifestFor({ 'docs/99-cloud/index.md': 'x\n' });
    const blob = buildBlob(manifest, [{ name: '../escape.md', data: 'escape\n' }]);
    const site = makeSite([lockEntryFor(blob)]);
    await expect(
      materializeContentBundles(site.options({ ci: true, fetchBlob: () => Promise.resolve(blob) })),
    ).rejects.toThrow(/sitecontent\.invalid_path/);
  });

  it('always falls back to baked content on a newer format major (CI included)', async () => {
    const blob = blobFor(FILES, { formatVersion: '2.0' });
    for (const ci of [true, false]) {
      const site = makeSite([lockEntryFor(blob)]);
      const result = await materializeContentBundles(site.options({ ci, fetchBlob: () => Promise.resolve(blob) }));
      expect(result.mounted).toEqual([]);
      expect(result.skipped).toEqual([{ name: 'cloud-docs', reason: 'newer format major' }]);
      expect(site.warnings.join('\n')).toContain('falling back to baked content');
    }
  });

  it('rejects a bundle whose manifest name differs from the lock entry', async () => {
    const blob = blobFor(FILES, { name: 'other-name' });
    const site = makeSite([lockEntryFor(blob)]);
    await expect(
      materializeContentBundles(site.options({ ci: true, fetchBlob: () => Promise.resolve(blob) })),
    ).rejects.toThrow(/does not match the lock entry name/);
  });

  specTest(
    'hard-errors on a bundle mount colliding with site-local content — in dev too',
    {
      feature: 'putnami-dev/public-documentation',
      requirement: 'one-source-per-section',
      check: 'bundle-mounts-cannot-overlap-owned-sections',
    },
    async () => {
      const files = { 'docs/01-getting-started/hijack.md': 'x\n' };
      const blob = blobFor(files, { mounts: [{ urlPrefix: '/docs/01-getting-started' }] });
      const site = makeSite([lockEntryFor(blob)]);
      await expect(
        materializeContentBundles(site.options({ ci: false, fetchBlob: () => Promise.resolve(blob) })),
      ).rejects.toThrow(/overlaps/);
    },
  );

  it('hard-errors on a parent-prefix collision (bundle mounting above local content)', async () => {
    const blob = blobFor({ 'docs/index.md': 'x\n' }, { mounts: [{ urlPrefix: '/docs' }] });
    const site = makeSite([lockEntryFor(blob)]);
    await expect(materializeContentBundles(site.options({ fetchBlob: () => Promise.resolve(blob) }))).rejects.toThrow(
      /overlaps/,
    );
  });

  it('hard-errors when two bundles claim overlapping mounts', async () => {
    const blobA = blobFor(FILES);
    const blobB = blobFor(
      { 'docs/99-cloud/sub/index.md': 'x\n' },
      { name: 'other-docs', mounts: [{ urlPrefix: '/docs/99-cloud/sub' }] },
    );
    const site = makeSite([lockEntryFor(blobA), lockEntryFor(blobB, { name: 'other-docs' })]);
    const byDigest = new Map([
      [sha256Hex(blobA), blobA],
      [sha256Hex(blobB), blobB],
    ]);
    await expect(
      materializeContentBundles(
        site.options({ fetchBlob: (bundle) => Promise.resolve(byDigest.get(bundle.digest) as Uint8Array) }),
      ),
    ).rejects.toThrow(/overlaps/);
  });

  it('throws on a defective lock in every environment', async () => {
    const site = makeSite([{ name: 'cloud-docs' }]);
    await expect(materializeContentBundles(site.options({ ci: false }))).rejects.toThrow(/content\.lock\.json/);
  });
});

// ---------------------------------------------------------------------------
// Site-local prefix derivation — against the real putnami.json
// ---------------------------------------------------------------------------

describe('deriveSiteLocalPrefixes', () => {
  it('expands the docs root to its ordered sections and keeps other targets as declared', () => {
    const prefixes = deriveSiteLocalPrefixes(PROJECT_ROOT, WORKSPACE_ROOT);
    // The root public/docs copy is expanded to sections (the ownership unit),
    // so a bundle can add a NEW /docs/<nn-section> without colliding.
    expect(prefixes).not.toContain('/docs');
    expect(prefixes).toContain('/docs/01-getting-started');
    expect(prefixes).toContain('/docs/02-concepts');
    expect(prefixes).toContain('/docs/08-tooling-&-workspace');
    expect(prefixes).toContain('/docs/09-frameworks/01-typescript');
    expect(prefixes).toContain('/install.sh');
    expect(prefixes).toContain('/install.ps1');
    expect(prefixes).toContain('/LICENSE.md');
    // Every declared section conflicts with itself but not with a free slot.
    expect(prefixes.some((p) => prefixesConflict(p, '/docs/99-cloud'))).toBe(false);
  });
});
