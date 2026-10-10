// biome-ignore-all lint/suspicious/noConsole: Captures JsonSink output written through console.log.
import { afterEach, beforeEach, describe, expect, it, mock } from 'bun:test';
import { resetDefaultLogger, resetLoggerConfig } from '@putnami/runtime';
import {
  RemoteConfigError,
  RemoteConfigSource,
  resetRemoteConfigSourceCacheForTest,
} from '../src/runtime/remote-config-source';
import { loadConfigSnapshot, parseGsUri, resetSnapshotAccessTokenCacheForTest } from '../src/runtime/remote-snapshot';
import {
  resetSyncFetchForTest,
  setSyncFetchForTest,
  type SyncFetchRequest,
  type SyncFetchResponse,
} from '../src/runtime/sync-fetch';

/**
 * Stubs the three HTTP hops the snapshot loader makes — the metadata
 * access-token fetch, the GCS object download, and the KMS Decrypt — plus the
 * config-server resolve call the RemoteConfigSource makes first.
 *
 * The stub models the snapshot encoding contract shared with the Go loader:
 * the GCS object holds the base64 ciphertext string,
 * and KMS Decrypt echoes it back as the base64 plaintext. The fake "decrypt" is
 * the identity function — the object IS base64(plaintext) — so the test
 * exercises the base64/JSON/REST wiring end to end without real crypto.
 */
function snapshotStub(
  objectText: string,
  opts: { onResolve?: () => SyncFetchResponse } = {},
): (request: SyncFetchRequest) => SyncFetchResponse {
  return (request) => {
    const url = request.url;
    if (url.includes('metadata.google.internal')) {
      return {
        status: 200,
        body: JSON.stringify({ access_token: 'ya29.access-token', expires_in: 3600, token_type: 'Bearer' }),
      };
    }
    if (url.includes('storage.googleapis.com')) {
      return { status: 200, body: objectText };
    }
    if (url.includes('cloudkms.googleapis.com') && url.endsWith(':decrypt')) {
      const body = JSON.parse(request.body ?? '{}') as { ciphertext?: string };
      return { status: 200, body: JSON.stringify({ plaintext: body.ciphertext ?? '' }) };
    }
    // config-server resolve endpoint: connection refused by default.
    if (opts.onResolve) {
      return opts.onResolve();
    }
    throw new Error('connection refused');
  };
}

function encodeSnapshotObject(tree: Record<string, unknown>): string {
  // The object holds the base64 KMS ciphertext string; with identity "crypto"
  // that is just base64(JSON(tree)).
  return Buffer.from(JSON.stringify(tree)).toString('base64');
}

describe('parseGsUri', () => {
  const okCases: Array<{ name: string; uri: string; bucket: string; object: string }> = [
    {
      name: 'nested object',
      uri: 'gs://my-bucket/config-snapshots/ws1/registry/rev.json.enc',
      bucket: 'my-bucket',
      object: 'config-snapshots/ws1/registry/rev.json.enc',
    },
    { name: 'flat object', uri: 'gs://b/o', bucket: 'b', object: 'o' },
    { name: 'trimmed', uri: '  gs://b/o  ', bucket: 'b', object: 'o' },
  ];
  for (const tc of okCases) {
    it(`parses ${tc.name}`, () => {
      expect(parseGsUri(tc.uri)).toEqual({ bucket: tc.bucket, object: tc.object });
    });
  }

  const errCases: Array<{ name: string; uri: string }> = [
    { name: 'empty', uri: '' },
    { name: 'whitespace', uri: '   ' },
    { name: 'wrong scheme', uri: 'https://b/o' },
    { name: 'no object', uri: 'gs://b' },
    { name: 'no object trailing slash', uri: 'gs://b/' },
  ];
  for (const tc of errCases) {
    it(`rejects ${tc.name}`, () => {
      expect(() => parseGsUri(tc.uri)).toThrow();
    });
  }
});

describe('loadConfigSnapshot', () => {
  beforeEach(() => {
    resetSyncFetchForTest();
    resetSnapshotAccessTokenCacheForTest();
  });
  afterEach(() => {
    resetSyncFetchForTest();
    resetSnapshotAccessTokenCacheForTest();
  });

  it('round-trips the resolved config tree end to end', () => {
    const tree = { service: { port: 8080 }, flag: true };
    setSyncFetchForTest(snapshotStub(encodeSnapshotObject(tree)));

    const got = loadConfigSnapshot(
      'gs://snapshots/config-snapshots/ws1/registry/rev.json.enc',
      'projects/p/locations/l/keyRings/putnami/cryptoKeys/ws1',
      5000,
    );
    expect(got).toEqual(tree);
  });

  it('sends the object text verbatim as the KMS ciphertext and access-token bearer', () => {
    const tree = { db: { host: 'snap' } };
    const objectText = encodeSnapshotObject(tree);
    const seen: SyncFetchRequest[] = [];
    setSyncFetchForTest((request) => {
      seen.push(request);
      return snapshotStub(objectText)(request);
    });

    loadConfigSnapshot('gs://snapshots/o.enc', 'projects/p/locations/l/keyRings/r/cryptoKeys/k', 5000);

    const gcs = seen.find((r) => r.url.includes('storage.googleapis.com'));
    const kms = seen.find((r) => r.url.includes('cloudkms.googleapis.com'));
    expect(gcs?.headers?.Authorization).toBe('Bearer ya29.access-token');
    expect(gcs?.url).toContain('/b/snapshots/o/o.enc?alt=media');
    expect(kms?.headers?.Authorization).toBe('Bearer ya29.access-token');
    expect(kms?.url).toBe('https://cloudkms.googleapis.com/v1/projects/p/locations/l/keyRings/r/cryptoKeys/k:decrypt');
    const kmsBody = JSON.parse(kms?.body ?? '{}') as { ciphertext?: string };
    expect(kmsBody.ciphertext).toBe(objectText);
  });

  it('throws for an empty KMS key before any fetch', () => {
    let calls = 0;
    setSyncFetchForTest((request) => {
      calls += 1;
      return snapshotStub('x')(request);
    });
    expect(() => loadConfigSnapshot('gs://b/o', '', 5000)).toThrow(/KMS key is empty/);
    expect(calls).toBe(0);
  });

  it('throws for a non-gs URI', () => {
    setSyncFetchForTest(snapshotStub('x'));
    expect(() => loadConfigSnapshot('https://b/o', 'projects/p/l/k', 5000)).toThrow(/gs:\/\//);
  });

  it('throws when the metadata access-token endpoint is unreachable (off-GCP)', () => {
    setSyncFetchForTest((request) => {
      if (request.url.includes('metadata.google.internal')) {
        throw new Error('metadata unavailable');
      }
      return { status: 200, body: '' };
    });
    expect(() => loadConfigSnapshot('gs://b/o', 'projects/p/locations/l/keyRings/r/cryptoKeys/k', 5000)).toThrow(
      /metadata unavailable/,
    );
  });
});

describe('RemoteConfigSource snapshot fallback', () => {
  const originalConsoleLog = console.log;
  const snapshotUri = 'gs://snapshots/config-snapshots/ws1/registry/rev.json.enc';
  const snapshotKmsKey = 'projects/p/locations/l/keyRings/putnami/cryptoKeys/ws1';

  beforeEach(() => {
    resetSyncFetchForTest();
    resetSnapshotAccessTokenCacheForTest();
    resetRemoteConfigSourceCacheForTest();
    resetLoggerConfig();
    resetDefaultLogger();
  });

  afterEach(() => {
    console.log = originalConsoleLog;
    resetSyncFetchForTest();
    resetSnapshotAccessTokenCacheForTest();
    resetRemoteConfigSourceCacheForTest();
    resetLoggerConfig();
    resetDefaultLogger();
  });

  it('boots on the durable snapshot when a required config server is unreachable', () => {
    const tree = { db: { host: 'snap' } };
    const lines: string[] = [];
    console.log = mock((line?: unknown) => {
      lines.push(String(line ?? ''));
    }) as unknown as typeof console.log;
    setSyncFetchForTest(snapshotStub(encodeSnapshotObject(tree)));

    const source = new RemoteConfigSource({
      serverUrl: 'https://config.putnami.test',
      appName: 'registry',
      environment: 'prod',
      required: true,
      retryBudget: -1, // fail fast to the fallback
      timeout: 200,
      snapshotUri,
      snapshotKmsKey,
    });

    expect(source.load()).toEqual(tree);

    const staleWarn = lines
      .map((line) => JSON.parse(line) as Record<string, unknown>)
      .find(
        (entry) =>
          entry.message === 'config server unreachable; booting on durable config snapshot (stale-config mode)',
      );
    expect(staleWarn).toBeDefined();
    expect(staleWarn).toMatchObject({
      severity: 'WARNING',
      logger: 'config-server',
      appName: 'registry',
      environment: 'prod',
      snapshotUri,
    });
  });

  it('caches load() so a second call does not re-fetch the snapshot', () => {
    const tree = { db: { host: 'snap' } };
    let metadataCalls = 0;
    setSyncFetchForTest((request) => {
      if (request.url.includes('metadata.google.internal')) {
        metadataCalls += 1;
      }
      return snapshotStub(encodeSnapshotObject(tree))(request);
    });

    const source = new RemoteConfigSource({
      serverUrl: 'https://config.putnami.test',
      appName: 'registry',
      environment: 'prod',
      required: true,
      retryBudget: -1,
      timeout: 200,
      snapshotUri,
      snapshotKmsKey,
    });

    expect(source.load()).toEqual(tree);
    expect(source.load()).toEqual(tree);
    expect(metadataCalls).toBe(1);
  });

  it('still throws when the config server is unreachable and no snapshot is configured', () => {
    setSyncFetchForTest(snapshotStub(encodeSnapshotObject({ db: { host: 'snap' } })));

    const source = new RemoteConfigSource({
      serverUrl: 'https://config.putnami.test',
      appName: 'registry',
      environment: 'prod',
      required: true,
      retryBudget: -1,
      timeout: 200,
    });

    expect(() => source.load()).toThrow(RemoteConfigError);
  });

  it('applies the snapshot on a cache-poisoned "failed" entry from a peer instance', () => {
    // First instance has no snapshot: it poisons the module cache with
    // status:'failed'. A second instance with the same request but a snapshot
    // configured must still fall back through resolveCached rather than fail.
    const tree = { db: { host: 'snap-cached' } };
    setSyncFetchForTest(snapshotStub(encodeSnapshotObject(tree)));

    const options = {
      serverUrl: 'https://config.putnami.test',
      appName: 'registry',
      environment: 'prod',
      required: true,
      retryBudget: -1,
      timeout: 200,
    } as const;

    const first = new RemoteConfigSource({ ...options });
    expect(() => first.load()).toThrow(RemoteConfigError);

    const second = new RemoteConfigSource({ ...options, snapshotUri, snapshotKmsKey });
    expect(second.load()).toEqual(tree);
  });

  it('does not serve a stale snapshot once the config server resolves (no snapshot poisoning)', () => {
    const remote = { server: { port: 3000 } };
    setSyncFetchForTest(
      snapshotStub(encodeSnapshotObject({ db: { host: 'snap' } }), {
        onResolve: () => ({
          status: 200,
          body: JSON.stringify({ config: remote, resolved: true, schemaMatch: true }),
        }),
      }),
    );

    const source = new RemoteConfigSource({
      serverUrl: 'https://config.putnami.test',
      appName: 'registry',
      environment: 'prod',
      required: true,
      retryBudget: -1,
      timeout: 200,
      snapshotUri,
      snapshotKmsKey,
    });

    expect(source.load()).toEqual(remote);
  });

  it('does not fall back to the snapshot on a terminal 403 (access revoked)', () => {
    // 403 is terminal, not a retryable outage: even with a snapshot configured the
    // boot must fail rather than serve stale, secret-bearing config after access
    // was revoked.
    setSyncFetchForTest(
      snapshotStub(encodeSnapshotObject({ db: { host: 'snap' } }), {
        onResolve: () => ({ status: 403, body: 'Forbidden' }),
      }),
    );

    const source = new RemoteConfigSource({
      serverUrl: 'https://config.putnami.test',
      appName: 'registry',
      environment: 'prod',
      required: true,
      retryBudget: -1,
      timeout: 200,
      snapshotUri,
      snapshotKmsKey,
    });

    expect(() => source.load()).toThrow(RemoteConfigError);
  });

  it('does not fall back to the snapshot when the server reports resolved:false', () => {
    // Deleted / unresolved config is authoritative and terminal — a stale snapshot
    // must not resurrect config the control plane no longer resolves.
    setSyncFetchForTest(
      snapshotStub(encodeSnapshotObject({ db: { host: 'snap' } }), {
        onResolve: () => ({ status: 200, body: JSON.stringify({ config: {}, resolved: false, schemaMatch: false }) }),
      }),
    );

    const source = new RemoteConfigSource({
      serverUrl: 'https://config.putnami.test',
      appName: 'registry',
      environment: 'prod',
      required: true,
      retryBudget: -1,
      timeout: 200,
      snapshotUri,
      snapshotKmsKey,
    });

    expect(() => source.load()).toThrow(RemoteConfigError);
  });

  it('emits a quieter warn and rethrows when the snapshot itself cannot be loaded', () => {
    const lines: string[] = [];
    console.log = mock((line?: unknown) => {
      lines.push(String(line ?? ''));
    }) as unknown as typeof console.log;
    // GCS returns 404 for the snapshot object; resolve throws.
    setSyncFetchForTest((request) => {
      if (request.url.includes('metadata.google.internal')) {
        return { status: 200, body: JSON.stringify({ access_token: 'ya29.access-token', expires_in: 3600 }) };
      }
      if (request.url.includes('storage.googleapis.com')) {
        return { status: 404, body: 'Not Found' };
      }
      throw new Error('connection refused');
    });

    const source = new RemoteConfigSource({
      serverUrl: 'https://config.putnami.test',
      appName: 'registry',
      environment: 'prod',
      required: true,
      retryBudget: -1,
      timeout: 200,
      snapshotUri,
      snapshotKmsKey,
    });

    expect(() => source.load()).toThrow(RemoteConfigError);
    const quietWarn = lines
      .map((line) => JSON.parse(line) as Record<string, unknown>)
      .find((entry) => entry.message === 'config snapshot fallback failed; no durable config available');
    expect(quietWarn).toBeDefined();
    expect(quietWarn).toMatchObject({ severity: 'WARNING', logger: 'config-server', snapshotUri });
  });
});
