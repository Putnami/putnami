/**
 * Runtime content-overlay tests.
 *
 * Exercises the self-update overlay end to end against local doubles (no
 * network, no real registry, no app context): the overlay manager's active
 * pointer, the ingest pipeline (fetch → verify → materialize → reindex →
 * atomic finalize → swap), the TTL-gated single-flight newness check, the
 * cheap channel-pointer client, and the refresh-endpoint authorization matrix.
 *
 * Bundle fixtures reuse the `content-bundle.test.ts` producer double: a tar.gz
 * with `bundle.json` at the root, addressed by one sha256 digest.
 */
import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { existsSync, mkdirSync, mkdtempSync, readdirSync, unlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileExists } from '@putnami/utils';
import { authorizeRefresh, type RefreshTokenVerifier } from '../src/plugins/content-overlay.plugin';
import { ingestBundle } from '../src/lib/content/ingest';
import type { LockBundle } from '../src/lib/content/lock';
import { forceCheckAndConverge, maybeCheckAndConverge, resetConvergeStateForTest } from '../src/lib/content/newness';
import { activeContentDigest, getActiveDocsRoot, resetOverlayForTest, setActive } from '../src/lib/content/overlay';
import { getBakedDocsRoot, invalidateNavCache, resolveContentPath } from '../src/lib/docs/navigation.server';
import { type ChannelPointer, fetchChannelPointerFromRegistry } from '../src/lib/content/registry';
import type { Manifest } from '../src/lib/content/sitecontent';
import { type TarWriteEntry, writeTarEntries } from '../src/lib/content/tar';
import { sha256Hex } from '../src/lib/content/verify';

// ---------------------------------------------------------------------------
// Fixture helpers — a local producer double (tar.gz with bundle.json at root),
// mirroring content-bundle.test.ts so both suites build byte-identical bundles.
// ---------------------------------------------------------------------------

const REGISTRY = 'https://put.putnami.dev';
const IDENTITY: Pick<LockBundle, 'name' | 'package' | 'registry'> = {
  name: 'cloud-docs',
  package: 'site-content/cloud',
  registry: REGISTRY,
};
const SIZE_CAP = 64 * 1024 * 1024;

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

/** A default cloud bundle: one baked-only file plus one bundle-owned file. */
const CLOUD_FILES = {
  'docs/99-cloud/index.md': '# Cloud\nhello cloud\n',
  'docs/99-cloud/guides/start.md': '# Start\n',
};

function mkTmp(prefix: string): string {
  return mkdtempSync(join(tmpdir(), `overlay-${prefix}-`));
}

/**
 * Stand in for an OS temp reaper: delete every FILE under `dir`, keep every
 * directory. That asymmetry is what makes a reaped version look finalized.
 */
function reapFiles(dir: string): void {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const target = join(dir, entry.name);
    if (entry.isDirectory()) reapFiles(target);
    else unlinkSync(target);
  }
}

/** A baked docs root carrying a build-time doc the overlay must preserve. */
function bakedRootWithIntro(): string {
  const root = mkTmp('baked');
  writeFileSync(join(root, '01-intro.md'), '# Intro\nbaked intro\n');
  return root;
}

beforeEach(() => {
  resetOverlayForTest();
  resetConvergeStateForTest();
});

// ---------------------------------------------------------------------------
// Ingest pipeline
// ---------------------------------------------------------------------------

describe('ingest pipeline', () => {
  it('happy path: verifies, unions baked + bundle, reindexes, swaps the active pointer', async () => {
    const blob = blobFor(CLOUD_FILES);
    const digest = sha256Hex(blob);
    const baked = bakedRootWithIntro();
    const versionsDir = mkTmp('versions');
    let swaps = 0;

    const outcome = await ingestBundle({
      bundle: IDENTITY,
      digest,
      bakedDocsRoot: baked,
      sizeCapBytes: SIZE_CAP,
      versionsDir,
      fetchBlob: async () => blob,
      onSwap: async () => {
        swaps += 1;
      },
      log: () => {},
    });

    expect(outcome.outcome).toBe('updated');
    expect(outcome.activeDigest).toBe(digest);
    expect(swaps).toBe(1); // post-swap invalidation was awaited

    const active = getActiveDocsRoot(baked);
    expect(active).toBe(join(versionsDir, digest, 'docs'));
    // Baked-only doc is preserved (union), bundle-owned docs are present.
    expect(existsSync(join(active, '01-intro.md'))).toBe(true);
    expect(existsSync(join(active, '99-cloud/index.md'))).toBe(true);
    expect(existsSync(join(active, '99-cloud/guides/start.md'))).toBe(true);
    // The search index was rebuilt over the unioned tree, beside docs/.
    expect(existsSync(join(versionsDir, digest, 'search', 'index.json'))).toBe(true);
  });

  it('runs the default post-swap invalidation without throwing (nav cache + revalidateTag)', async () => {
    const blob = blobFor(CLOUD_FILES);
    const digest = sha256Hex(blob);
    const outcome = await ingestBundle({
      bundle: IDENTITY,
      digest,
      bakedDocsRoot: bakedRootWithIntro(),
      sizeCapBytes: SIZE_CAP,
      versionsDir: mkTmp('versions'),
      fetchBlob: async () => blob,
      log: () => {},
    });
    expect(outcome.outcome).toBe('updated');
  });

  it('rejects a blob whose sha256 does not match the target digest, keeping baked content', async () => {
    const blob = blobFor(CLOUD_FILES);
    const wrongDigest = sha256Hex(bytes('a different bundle')); // valid hex, wrong content
    const baked = bakedRootWithIntro();

    const outcome = await ingestBundle({
      bundle: IDENTITY,
      digest: wrongDigest,
      bakedDocsRoot: baked,
      sizeCapBytes: SIZE_CAP,
      versionsDir: mkTmp('versions'),
      fetchBlob: async () => blob,
      log: () => {},
    });

    expect(outcome.outcome).toBe('rejected-digest');
    // No swap happened: readers still resolve baked content.
    expect(activeContentDigest()).toBeNull();
    expect(getActiveDocsRoot(baked)).toBe(baked);
  });

  it('gates a newer format MAJOR (rejected-format, baked kept) but accepts a newer MINOR', async () => {
    const baked = bakedRootWithIntro();

    const majorBlob = blobFor(CLOUD_FILES, { formatVersion: '2.0' });
    const major = await ingestBundle({
      bundle: IDENTITY,
      digest: sha256Hex(majorBlob),
      bakedDocsRoot: baked,
      sizeCapBytes: SIZE_CAP,
      versionsDir: mkTmp('versions'),
      fetchBlob: async () => majorBlob,
      log: () => {},
    });
    expect(major.outcome).toBe('rejected-format');
    expect(activeContentDigest()).toBeNull(); // still baked — the "force CI/CD" signal

    const minorBlob = blobFor(CLOUD_FILES, { formatVersion: '1.9' });
    const minorDigest = sha256Hex(minorBlob);
    const minor = await ingestBundle({
      bundle: IDENTITY,
      digest: minorDigest,
      bakedDocsRoot: baked,
      sizeCapBytes: SIZE_CAP,
      versionsDir: mkTmp('versions'),
      fetchBlob: async () => minorBlob,
      log: () => {},
    });
    expect(minor.outcome).toBe('updated');
    expect(activeContentDigest()).toBe(minorDigest);
  });

  it('rejects a blob larger than the size cap before materializing', async () => {
    const blob = blobFor(CLOUD_FILES);
    const outcome = await ingestBundle({
      bundle: IDENTITY,
      digest: sha256Hex(blob),
      bakedDocsRoot: bakedRootWithIntro(),
      sizeCapBytes: 8, // smaller than any real bundle
      versionsDir: mkTmp('versions'),
      fetchBlob: async () => blob,
      log: () => {},
    });
    expect(outcome.outcome).toBe('rejected-size');
    expect(activeContentDigest()).toBeNull();
  });

  it('maps a fetch failure to fetch-failed and keeps serving baked content', async () => {
    const blob = blobFor(CLOUD_FILES);
    const outcome = await ingestBundle({
      bundle: IDENTITY,
      digest: sha256Hex(blob),
      bakedDocsRoot: bakedRootWithIntro(),
      sizeCapBytes: SIZE_CAP,
      versionsDir: mkTmp('versions'),
      fetchBlob: async () => {
        throw new Error('registry unreachable');
      },
      log: () => {},
    });
    expect(outcome.outcome).toBe('fetch-failed');
    expect(activeContentDigest()).toBeNull();
  });

  it('prunes a dropped baked bundle mount so no stale baked docs survive activation', async () => {
    const baked = bakedRootWithIntro();
    // Simulate a previously-baked bundle that owned /docs/99-cloud.
    mkdirSync(join(baked, '99-cloud'), { recursive: true });
    writeFileSync(join(baked, '99-cloud', 'old.md'), '# Old cloud\n');

    // The new channel version drops /docs/99-cloud and owns /docs/88-cloud.
    const blob = blobFor({ 'docs/88-cloud/index.md': '# New cloud\n' }, { mounts: [{ urlPrefix: '/docs/88-cloud' }] });
    const digest = sha256Hex(blob);

    const outcome = await ingestBundle({
      bundle: IDENTITY,
      digest,
      bakedDocsRoot: baked,
      bakedMounts: ['/docs/99-cloud'],
      sizeCapBytes: SIZE_CAP,
      versionsDir: mkTmp('versions'),
      fetchBlob: async () => blob,
      onSwap: async () => {},
      log: () => {},
    });

    expect(outcome.outcome).toBe('updated');
    const active = getActiveDocsRoot(baked);
    expect(existsSync(join(active, '88-cloud/index.md'))).toBe(true); // new mount materialized
    expect(existsSync(join(active, '99-cloud'))).toBe(false); // dropped baked mount pruned
    expect(existsSync(join(active, '01-intro.md'))).toBe(true); // site-local doc preserved
  });

  it('finalizes atomically: no .staging-* residue and a complete version dir after a swap', async () => {
    const blob = blobFor(CLOUD_FILES);
    const digest = sha256Hex(blob);
    const versionsDir = mkTmp('versions');
    await ingestBundle({
      bundle: IDENTITY,
      digest,
      bakedDocsRoot: bakedRootWithIntro(),
      sizeCapBytes: SIZE_CAP,
      versionsDir,
      fetchBlob: async () => blob,
      log: () => {},
    });
    const entries = readdirSync(versionsDir);
    expect(entries).toContain(digest);
    expect(entries.some((e) => e.startsWith('.staging-'))).toBe(false);
    // The finalized dir is complete (docs + search) — the pointer only flips
    // after the rename, so a reader never observes a partial version.
    expect(existsSync(join(versionsDir, digest, 'docs'))).toBe(true);
    expect(existsSync(join(versionsDir, digest, 'search', 'index.json'))).toBe(true);
  });

  it('re-materializes a version an OS temp reaper emptied instead of adopting the skeleton', async () => {
    const blob = blobFor(CLOUD_FILES);
    const digest = sha256Hex(blob);
    const baked = bakedRootWithIntro();
    const versionsDir = mkTmp('versions');
    const converge = () =>
      ingestBundle({
        bundle: IDENTITY,
        digest,
        bakedDocsRoot: baked,
        sizeCapBytes: SIZE_CAP,
        versionsDir,
        fetchBlob: async () => blob,
        onSwap: async () => {},
        log: () => {},
      });

    expect((await converge()).outcome).toBe('updated');
    const versionDir = join(versionsDir, digest);

    // Versions live under the OS temp dir, whose reaper deletes FILES past its
    // retention and leaves the directory tree standing.
    reapFiles(versionDir);
    expect(existsSync(versionDir)).toBe(true);
    expect(existsSync(join(versionDir, 'docs', '99-cloud/index.md'))).toBe(false);

    // A fresh process re-checks the same digest: the surviving skeleton must not
    // pass for a finalized version, or every docs page 404s against empty content.
    resetOverlayForTest();
    expect((await converge()).outcome).toBe('updated');

    const active = getActiveDocsRoot(baked);
    expect(active).toBe(join(versionDir, 'docs'));
    expect(existsSync(join(active, '99-cloud/index.md'))).toBe(true);
    expect(existsSync(join(active, '01-intro.md'))).toBe(true);
    expect(existsSync(join(versionDir, 'search', 'index.json'))).toBe(true);
  });

  it('replaces an unmarked version directory left by a build that predates the marker', async () => {
    const blob = blobFor(CLOUD_FILES);
    const digest = sha256Hex(blob);
    const baked = bakedRootWithIntro();
    const versionsDir = mkTmp('versions');
    // Finalized by an older build: populated, but carrying no completeness proof.
    // The rename also has to survive a NON-EMPTY target (rename(2) refuses one).
    mkdirSync(join(versionsDir, digest, 'docs'), { recursive: true });
    writeFileSync(join(versionsDir, digest, 'docs', 'stale.md'), '# stale\n');

    const outcome = await ingestBundle({
      bundle: IDENTITY,
      digest,
      bakedDocsRoot: baked,
      sizeCapBytes: SIZE_CAP,
      versionsDir,
      fetchBlob: async () => blob,
      onSwap: async () => {},
      log: () => {},
    });

    expect(outcome.outcome).toBe('updated');
    const active = getActiveDocsRoot(baked);
    expect(existsSync(join(active, '99-cloud/index.md'))).toBe(true);
    expect(existsSync(join(active, 'stale.md'))).toBe(false); // replaced wholesale, never merged
  });
});

// ---------------------------------------------------------------------------
// Empty-overlay degradation
// ---------------------------------------------------------------------------

describe('empty-overlay degradation', () => {
  afterEach(() => {
    resetOverlayForTest();
    invalidateNavCache();
  });

  it('has a generated docs tree to degrade to', () => {
    // The generate phase populates .gen/public/docs before tests run; without it
    // the degradation assertion below would pass vacuously.
    expect(fileExists(getBakedDocsRoot())).toBe(true);
  });

  it('drops an active overlay that holds no markdown and resolves baked content', async () => {
    const baked = getBakedDocsRoot();
    invalidateNavCache();
    const bakedContentPath = await resolveContentPath('why');
    expect(bakedContentPath).toBeDefined();

    // An activated version whose files were reaped after the pointer flipped —
    // the case the ingest-side marker cannot catch.
    const versionsDir = mkTmp('versions');
    const digest = 'a'.repeat(64);
    mkdirSync(join(versionsDir, digest, 'docs'), { recursive: true });
    setActive(join(versionsDir, digest, 'docs'), digest);
    invalidateNavCache();
    expect(getActiveDocsRoot(baked)).not.toBe(baked);

    // Readers degrade rather than 404, and the pointer is dropped so the loader
    // resolves file paths against the same root the path map was built from.
    expect(await resolveContentPath('why')).toBe(bakedContentPath!);
    expect(activeContentDigest()).toBeNull();
    expect(getActiveDocsRoot(baked)).toBe(baked);
  });
});

// ---------------------------------------------------------------------------
// Cold-start fallback
// ---------------------------------------------------------------------------

describe('cold-start fallback', () => {
  it('resolves baked content when no overlay has been ingested', () => {
    const baked = '/some/baked/.gen/public/docs';
    expect(activeContentDigest()).toBeNull();
    expect(getActiveDocsRoot(baked)).toBe(baked);
  });
});

// ---------------------------------------------------------------------------
// Newness check (TTL-gated, single-flight, stale-while-revalidate)
// ---------------------------------------------------------------------------

describe('newness check', () => {
  function writeLock(dir: string, bundleDigest: string): string {
    const lock = {
      bundles: [
        {
          name: IDENTITY.name,
          package: IDENTITY.package,
          channelHint: 'stable',
          version: '2026.1.0',
          digest: bundleDigest,
          registry: REGISTRY,
        },
      ],
    };
    const lockPath = join(dir, 'content.lock.json');
    writeFileSync(lockPath, JSON.stringify(lock));
    return lockPath;
  }

  const BAKED_PIN = sha256Hex(bytes('baked-pin-blob'));

  it('ingests when the channel pointer advertises a digest different from the baked pin', async () => {
    const blob = blobFor(CLOUD_FILES);
    const newDigest = sha256Hex(blob);
    const dir = mkTmp('lock');
    const baked = bakedRootWithIntro();

    const result = await maybeCheckAndConverge({
      lockPath: writeLock(dir, BAKED_PIN),
      bakedDocsRoot: baked,
      checkTtlMs: 300_000,
      sizeCapBytes: SIZE_CAP,
      versionsDir: mkTmp('versions'),
      fetchPointer: async () => ({ version: '2026.2.0', digest: newDigest }),
      fetchBlob: async () => blob,
      onSwap: async () => {},
      log: () => {},
      now: () => 1000,
    });

    expect(result?.outcome).toBe('updated');
    expect(activeContentDigest()).toBe(newDigest);
  });

  it('does not ingest when the pointer digest equals the current (baked) digest', async () => {
    let blobFetches = 0;
    const result = await maybeCheckAndConverge({
      lockPath: writeLock(mkTmp('lock'), BAKED_PIN),
      bakedDocsRoot: bakedRootWithIntro(),
      checkTtlMs: 300_000,
      sizeCapBytes: SIZE_CAP,
      versionsDir: mkTmp('versions'),
      fetchPointer: async () => ({ version: '2026.1.0', digest: BAKED_PIN }),
      fetchBlob: async () => {
        blobFetches += 1;
        return new Uint8Array();
      },
      onSwap: async () => {},
      log: () => {},
      now: () => 1000,
    });

    expect(result?.outcome).toBe('unchanged');
    expect(blobFetches).toBe(0);
    expect(activeContentDigest()).toBeNull();
  });

  it('skips the poll entirely inside the TTL window (no pointer fetch)', async () => {
    let pointerFetches = 0;
    const options = {
      lockPath: writeLock(mkTmp('lock'), BAKED_PIN),
      bakedDocsRoot: bakedRootWithIntro(),
      checkTtlMs: 300_000,
      sizeCapBytes: SIZE_CAP,
      versionsDir: mkTmp('versions'),
      fetchPointer: async () => {
        pointerFetches += 1;
        return { version: '2026.1.0', digest: BAKED_PIN } as ChannelPointer;
      },
      fetchBlob: async () => new Uint8Array(),
      onSwap: async () => {},
      log: () => {},
    };

    await maybeCheckAndConverge({ ...options, now: () => 1000 }); // first poll runs
    expect(pointerFetches).toBe(1);
    const skipped = maybeCheckAndConverge({ ...options, now: () => 1000 + 60_000 }); // within TTL
    expect(skipped).toBeUndefined();
    expect(pointerFetches).toBe(1);
  });

  it('is single-flight: concurrent triggers share one in-flight poll', async () => {
    let pointerFetches = 0;
    const options = {
      lockPath: writeLock(mkTmp('lock'), BAKED_PIN),
      bakedDocsRoot: bakedRootWithIntro(),
      checkTtlMs: 300_000,
      sizeCapBytes: SIZE_CAP,
      versionsDir: mkTmp('versions'),
      fetchPointer: async () => {
        pointerFetches += 1;
        return { version: '2026.1.0', digest: BAKED_PIN } as ChannelPointer;
      },
      fetchBlob: async () => new Uint8Array(),
      onSwap: async () => {},
      log: () => {},
      now: () => 1000,
    };

    const first = maybeCheckAndConverge(options);
    const second = maybeCheckAndConverge(options);
    expect(second).toBe(first); // same in-flight promise, not a second poll
    await first;
    expect(pointerFetches).toBe(1);
  });

  it('force path bypasses the TTL to converge immediately', async () => {
    const blob = blobFor(CLOUD_FILES);
    const newDigest = sha256Hex(blob);
    const options = {
      lockPath: writeLock(mkTmp('lock'), BAKED_PIN),
      bakedDocsRoot: bakedRootWithIntro(),
      checkTtlMs: 300_000,
      sizeCapBytes: SIZE_CAP,
      versionsDir: mkTmp('versions'),
      fetchPointer: async () => ({ version: '2026.2.0', digest: newDigest }) as ChannelPointer,
      fetchBlob: async () => blob,
      onSwap: async () => {},
      log: () => {},
      now: () => 1000,
    };

    await maybeCheckAndConverge({ ...options, fetchPointer: async () => ({ version: 'x', digest: BAKED_PIN }) });
    expect(activeContentDigest()).toBeNull(); // first poll saw no change

    const forced = await forceCheckAndConverge(options); // bypasses TTL, sees the new head
    expect(forced?.outcome).toBe('updated');
    expect(activeContentDigest()).toBe(newDigest);
  });

  it('degrades to fetch-failed (keeps serving) when the pointer fetch throws', async () => {
    const result = await maybeCheckAndConverge({
      lockPath: writeLock(mkTmp('lock'), BAKED_PIN),
      bakedDocsRoot: bakedRootWithIntro(),
      checkTtlMs: 300_000,
      sizeCapBytes: SIZE_CAP,
      versionsDir: mkTmp('versions'),
      fetchPointer: async () => {
        throw new Error('pointer 503');
      },
      fetchBlob: async () => new Uint8Array(),
      onSwap: async () => {},
      log: () => {},
      now: () => 1000,
    });
    expect(result?.outcome).toBe('fetch-failed');
    expect(activeContentDigest()).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// Channel-pointer client (cheap TTL poll — never downloads the blob)
// ---------------------------------------------------------------------------

describe('channel-pointer client', () => {
  const anonymous = () => ({ exitCode: 1, stdout: '' }); // no CLI → anonymous fetch
  const realFetch = globalThis.fetch;
  afterEach(() => {
    globalThis.fetch = realFetch;
  });

  it('parses a {version,digest} pointer document', async () => {
    const digest = sha256Hex(bytes('head'));
    globalThis.fetch = (async () =>
      new Response(JSON.stringify({ version: '2026.2.0', digest }), { status: 200 })) as typeof fetch;
    const pointer = await fetchChannelPointerFromRegistry('site-content/cloud', 'stable', REGISTRY, anonymous);
    expect(pointer).toEqual({ version: '2026.2.0', digest });
  });

  it('rejects a pointer whose digest is not sha256 bare-hex', async () => {
    globalThis.fetch = (async () =>
      new Response(JSON.stringify({ version: '2026.2.0', digest: 'not-a-digest' }), { status: 200 })) as typeof fetch;
    await expect(fetchChannelPointerFromRegistry('site-content/cloud', 'stable', REGISTRY, anonymous)).rejects.toThrow(
      /sha256/,
    );
  });

  it('refuses a plaintext http registry (URL-trust) before fetching', async () => {
    let fetched = false;
    globalThis.fetch = (async () => {
      fetched = true;
      return new Response('{}', { status: 200 });
    }) as typeof fetch;
    await expect(
      fetchChannelPointerFromRegistry('site-content/cloud', 'stable', 'http://evil.example', anonymous),
    ).rejects.toThrow(/plaintext http/);
    expect(fetched).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// Refresh-endpoint authorization matrix
// ---------------------------------------------------------------------------

describe('refresh authorization', () => {
  const GUARD = { issuer: 'https://accounts.example/', audience: 'putnami.dev', allowedSubjects: ['ci@putnami.dev'] };
  const withBearer = (token: string) => new Headers({ authorization: `Bearer ${token}` });
  const verifierReturning =
    (claims: Record<string, unknown> | undefined): RefreshTokenVerifier =>
    async () =>
      claims;

  it('rejects a request with no bearer token', async () => {
    const auth = await authorizeRefresh(new Headers(), GUARD, verifierReturning({ email: 'ci@putnami.dev' }));
    expect(auth).toEqual({ ok: false, reason: 'unconfigured' });
  });

  it('rejects when the guard is not configured (no allowlist)', async () => {
    const auth = await authorizeRefresh(
      withBearer('t'),
      { issuer: GUARD.issuer, audience: GUARD.audience, allowedSubjects: [] },
      verifierReturning({ email: 'ci@putnami.dev' }),
    );
    expect(auth).toEqual({ ok: false, reason: 'unconfigured' });
  });

  it('rejects a token that fails verification', async () => {
    const auth = await authorizeRefresh(withBearer('bad'), GUARD, verifierReturning(undefined));
    expect(auth).toEqual({ ok: false, reason: 'bad-token' });
  });

  it('rejects a verified but non-allowlisted subject', async () => {
    const auth = await authorizeRefresh(withBearer('t'), GUARD, verifierReturning({ email: 'stranger@example.com' }));
    expect(auth).toEqual({ ok: false, reason: 'not-allowlisted' });
  });

  it('rejects an allowlisted email that is not verified', async () => {
    const auth = await authorizeRefresh(
      withBearer('t'),
      GUARD,
      verifierReturning({ email: 'ci@putnami.dev', email_verified: false }),
    );
    expect(auth).toEqual({ ok: false, reason: 'not-allowlisted' });
  });

  it('accepts an allowlisted, verified email', async () => {
    const auth = await authorizeRefresh(
      withBearer('t'),
      GUARD,
      verifierReturning({ email: 'ci@putnami.dev', email_verified: true }),
    );
    expect(auth).toEqual({ ok: true, subject: 'ci@putnami.dev' });
  });

  it('accepts an allowlisted subject (sub) when no email claim is present', async () => {
    const auth = await authorizeRefresh(
      withBearer('t'),
      { issuer: GUARD.issuer, audience: GUARD.audience, allowedSubjects: ['service-account-123'] },
      verifierReturning({ sub: 'service-account-123' }),
    );
    expect(auth).toEqual({ ok: true, subject: 'service-account-123' });
  });
});
