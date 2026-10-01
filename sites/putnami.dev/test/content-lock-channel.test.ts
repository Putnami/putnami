/**
 * The committed content lock decides which channel the site follows: both the
 * runtime newness check and `content bump` resolve `package@channelHint`.
 *
 * The Cloud workspace's native CI publishes `cloud/doc-contents-platform` as a release-set
 * member and moves that package's `canary` channel. The site follows `canary`
 * so it picks up what CI publishes; `latest` is not moved by CI.
 */
import { afterEach, describe, expect, it } from 'bun:test';
import { copyFileSync, mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { readContentLock } from '../src/lib/content/lock';
import { forceCheckAndConverge, resetConvergeStateForTest } from '../src/lib/content/newness';
import { resetOverlayForTest } from '../src/lib/content/overlay';
import { fetchChannelPointerFromRegistry } from '../src/lib/content/registry';

const COMMITTED_LOCK = join(import.meta.dir, '..', 'content.lock.json');

describe('committed content lock channel', () => {
  const realFetch = globalThis.fetch;
  afterEach(() => {
    globalThis.fetch = realFetch;
    resetConvergeStateForTest();
    resetOverlayForTest();
  });

  it('pins cloud/doc-contents-platform to the canary channel', () => {
    const lock = readContentLock(COMMITTED_LOCK);
    const platform = lock.bundles.find((bundle) => bundle.package === 'cloud/doc-contents-platform');
    expect(platform).toBeDefined();
    expect(platform?.channelHint).toBe('canary');
  });

  it('polls the canary channel pointer of the committed bundle', async () => {
    const lockPath = join(mkdtempSync(join(tmpdir(), 'content-lock-channel-')), 'content.lock.json');
    copyFileSync(COMMITTED_LOCK, lockPath);
    const pinned = readContentLock(lockPath).bundles[0];

    const requested: string[] = [];
    globalThis.fetch = (async (input: RequestInfo | URL) => {
      requested.push(String(input));
      // Report the pinned digest so the check ends as "unchanged" without a blob fetch.
      return new Response(JSON.stringify({ version: pinned.version, digest: pinned.digest }), { status: 200 });
    }) as typeof fetch;

    const anonymous = () => ({ exitCode: 1, stdout: '' });
    const result = await forceCheckAndConverge({
      lockPath,
      bakedDocsRoot: mkdtempSync(join(tmpdir(), 'content-lock-baked-')),
      checkTtlMs: 60_000,
      sizeCapBytes: 1024 * 1024,
      fetchPointer: (bundle) =>
        fetchChannelPointerFromRegistry(bundle.package, bundle.channelHint, bundle.registry, anonymous),
      log: () => {},
    });

    expect(result?.outcome).toBe('unchanged');
    expect(requested).toHaveLength(1);
    expect(requested[0]).toEndWith('/cloud/doc-contents-platform/channels/canary');
  });
});
