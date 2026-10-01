import { afterAll, afterEach, beforeAll, describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { rm } from 'node:fs/promises';
import { FileBackend } from '../src/backend/file.backend';
import { MemoryBackend } from '../src/backend/memory.backend';
import { RemoteBackend } from '../src/backend/remote.backend';
import type { StorageBackend } from '../src/backend/storage.backend';
import { StorageError } from '../src/errors';

/**
 * Shared cross-backend contract suite.
 *
 * `StorageBackend` has four implementations (memory, file, S3, remote). Each was
 * previously tested in isolation, so behavioral divergences in the common
 * interface — the not-found return shape, the typed missing-copy-source error,
 * overwrite/zero-byte semantics — could drift apart without CI noticing. This
 * function runs one set of assertions against any backend factory so the shared
 * guarantees are asserted to match.
 *
 * Three of the four are wired up below: memory, file-with-temp-dir, and remote
 * over a loopback service that serves the HTTP shape `RemoteBackend` documents.
 * None of them requires a network the suite does not start itself.
 *
 * S3 is NOT wired up, and cannot be as this harness stands: every case here
 * hard-codes the bucket names `bucket` and `no-such-bucket`, and `reset()`
 * deletes everything between cases. Against a real S3 target that means
 * `list('no-such-bucket')` errors instead of returning empty, `put` needs a
 * bucket that exists, and the reset is destructive on somebody's bucket.
 * Wiring S3 in means parameterising the bucket name and the reset first; until
 * then it keeps its own `PUTNAMI_S3_TEST_ENDPOINT`-gated suite in
 * `s3-backend.test.ts`, which re-states a subset of these rules by hand.
 */
export interface StorageContractOptions {
  /**
   * Whether the backend rejects path-traversal keys (`../…`). Every backend
   * wired below now does, so this stays a parameter only so a future backend
   * can be added to the suite before its key validation lands, rather than
   * being left out of the suite entirely.
   */
  rejectsTraversalKeys?: boolean;
}

/** A backend under test plus a hook to reset its state between cases. */
export interface ContractHarness {
  backend: StorageBackend;
  /** Remove every object so each `it` starts from a clean slate. */
  reset: () => Promise<void> | void;
}

export function runStorageContract(
  name: string,
  makeHarness: () => ContractHarness | Promise<ContractHarness>,
  options: StorageContractOptions = {},
): void {
  describe(`StorageBackend contract: ${name}`, () => {
    let backend: StorageBackend;
    let reset: ContractHarness['reset'];

    beforeAll(async () => {
      const harness = await makeHarness();
      backend = harness.backend;
      reset = harness.reset;
    });

    afterEach(async () => {
      await reset();
    });

    afterAll(async () => {
      await backend.close();
    });

    const bodyText = (stream: ReadableStream | undefined) => new Response(stream).text();

    specTest(
      'round-trips put → get with size and etag',
      {
        feature: 'typescript/object-storage',
        requirement: 'one-contract',
        check: 'put-then-get-round-trips-with-size-and-etag',
      },
      async () => {
        const put = await backend.put('bucket', 'greeting.txt', new Blob(['hello world']), {
          contentType: 'text/plain',
        });
        expect(put.key).toBe('greeting.txt');
        expect(put.size).toBe(11);
        expect(put.etag).toBeDefined();

        const got = await backend.get('bucket', 'greeting.txt');
        expect(got).not.toBeNull();
        expect(got?.key).toBe('greeting.txt');
        expect(got?.size).toBe(11);
        expect(got?.contentType).toBe('text/plain');
        expect(await bodyText(got?.body)).toBe('hello world');
      },
    );

    it('overwrites an existing object', async () => {
      await backend.put('bucket', 'file.txt', new Blob(['v1']));
      await backend.put('bucket', 'file.txt', new Blob(['v2']));

      const got = await backend.get('bucket', 'file.txt');
      expect(await bodyText(got?.body)).toBe('v2');
    });

    specTest(
      'stores and reads a zero-byte object',
      { feature: 'typescript/object-storage', requirement: 'one-contract', check: 'a-zero-byte-object-round-trips' },
      async () => {
        const put = await backend.put('bucket', 'empty.bin', new Blob([]));
        expect(put.size).toBe(0);

        expect(await backend.exists('bucket', 'empty.bin')).toBe(true);
        const got = await backend.get('bucket', 'empty.bin');
        expect(got).not.toBeNull();
        expect(got?.size).toBe(0);
        expect(await bodyText(got?.body)).toBe('');
      },
    );

    specTest(
      'returns null from get for a missing key',
      { feature: 'typescript/object-storage', requirement: 'one-contract', check: 'get-of-a-missing-key-returns-null' },
      async () => {
        expect(await backend.get('bucket', 'missing.txt')).toBeNull();
      },
    );

    specTest(
      'returns null from get for a missing bucket',
      {
        feature: 'typescript/object-storage',
        requirement: 'one-contract',
        check: 'get-of-a-missing-bucket-returns-null',
      },
      async () => {
        expect(await backend.get('no-such-bucket', 'file.txt')).toBeNull();
      },
    );

    specTest(
      'reports existence and clears it after delete',
      {
        feature: 'typescript/object-storage',
        requirement: 'one-contract',
        check: 'existence-is-reported-and-cleared-by-delete',
      },
      async () => {
        await backend.put('bucket', 'doomed.txt', new Blob(['x']));
        expect(await backend.exists('bucket', 'doomed.txt')).toBe(true);

        await backend.delete('bucket', 'doomed.txt');
        expect(await backend.exists('bucket', 'doomed.txt')).toBe(false);
      },
    );

    specTest(
      'treats delete of a missing key as a no-op',
      {
        feature: 'typescript/object-storage',
        requirement: 'one-contract',
        check: 'delete-of-a-missing-key-is-a-no-op',
      },
      async () => {
        await expect(backend.delete('bucket', 'never-existed.txt')).resolves.toBeUndefined();
      },
    );

    it('copies an object within the bucket', async () => {
      await backend.put('bucket', 'src.txt', new Blob(['copy me']));
      await backend.copy('bucket', 'src.txt', 'dst.txt');

      expect(await backend.exists('bucket', 'src.txt')).toBe(true);
      const copied = await backend.get('bucket', 'dst.txt');
      expect(copied).not.toBeNull();
      expect(await bodyText(copied?.body)).toBe('copy me');
    });

    specTest(
      'throws StorageError(NOT_FOUND) when copying a missing source',
      {
        feature: 'typescript/object-storage',
        requirement: 'one-contract',
        check: 'copy-of-a-missing-source-raises-not-found',
      },
      async () => {
        // Every backend must surface the same typed error so callers can branch on
        // a missing copy source without knowing which backend is configured.
        const error = await backend.copy('bucket', 'missing.txt', 'dst.txt').then(
          () => null,
          (e) => e,
        );
        expect(error).toBeInstanceOf(StorageError);
        expect((error as StorageError).code).toBe('NOT_FOUND');
        expect((error as StorageError).message).toContain('Source object not found');
      },
    );

    specTest(
      'returns an empty list for a missing bucket',
      { feature: 'typescript/object-storage', requirement: 'one-contract', check: 'list-of-a-missing-bucket-is-empty' },
      async () => {
        const result = await backend.list('no-such-bucket');
        expect(result.objects).toEqual([]);
        expect(result.prefixes).toEqual([]);
      },
    );

    specTest(
      'lists stored objects under a prefix',
      { feature: 'typescript/object-storage', requirement: 'one-contract', check: 'list-is-scoped-by-prefix' },
      async () => {
        await backend.put('bucket', 'images/a.png', new Blob(['a']));
        await backend.put('bucket', 'images/b.png', new Blob(['b']));
        await backend.put('bucket', 'docs/c.pdf', new Blob(['c']));

        const result = await backend.list('bucket', { prefix: 'images/' });
        expect(result.objects.map((o) => o.key).sort()).toEqual(['images/a.png', 'images/b.png']);
      },
    );

    const traversalIt = options.rejectsTraversalKeys ? it : it.skip;
    traversalIt('rejects path-traversal keys', async () => {
      await expect(backend.put('bucket', '../escape.txt', new Blob(['x']))).rejects.toThrow(StorageError);
      await expect(backend.get('bucket', '../escape.txt')).rejects.toThrow(StorageError);
    });
  });
}

// ---------------------------------------------------------------------------
// In-process backends. No network is required, so these run in every CI pass.
// ---------------------------------------------------------------------------

runStorageContract(
  'MemoryBackend',
  () => {
    const backend = new MemoryBackend();
    return { backend, reset: () => backend.clear() };
  },
  { rejectsTraversalKeys: true },
);

const FILE_CONTRACT_DIR = `/tmp/putnami-storage-contract-${Date.now()}`;
runStorageContract(
  'FileBackend',
  () => {
    const backend = new FileBackend(FILE_CONTRACT_DIR, 'contract-test-secret');
    return {
      backend,
      reset: () => rm(FILE_CONTRACT_DIR, { recursive: true, force: true }),
    };
  },
  { rejectsTraversalKeys: true },
);

// ---------------------------------------------------------------------------
// RemoteBackend. It is the backend production runs on, and it was the only
// implementation the shared contract never ran against — every rule below was
// asserted for memory and file only, so a translation bug in the HTTP layer
// (a not-found mapped to an error instead of null, a JSON date handed back as
// a string) could not fail CI. The loopback service below implements exactly
// the HTTP shape RemoteBackend documents and stores through a MemoryBackend,
// so the contract measures the remote backend's translation, not a second
// storage implementation.
// ---------------------------------------------------------------------------

/**
 * Serve the remote storage HTTP shape (`PUT|GET|HEAD|DELETE /{bucket}/{key}`,
 * `GET /{bucket}?prefix=…`, and `x-copy-source` on PUT) over a `MemoryBackend`.
 */
function serveRemoteStorage(store: MemoryBackend): ReturnType<typeof Bun.serve> {
  const split = (url: URL): { bucket: string; key: string } => {
    const [bucket = '', ...rest] = url.pathname.replace(/^\//, '').split('/');
    return { bucket: decodeURIComponent(bucket), key: rest.map(decodeURIComponent).join('/') };
  };

  return Bun.serve({
    port: 0,
    async fetch(request) {
      const url = new URL(request.url);
      const { bucket, key } = split(url);

      if (request.method === 'GET' && key === '') {
        const result = await store.list(bucket, {
          prefix: url.searchParams.get('prefix') ?? undefined,
          delimiter: url.searchParams.get('delimiter') ?? undefined,
        });
        return Response.json(result);
      }

      if (request.method === 'PUT') {
        const copySource = request.headers.get('x-copy-source');
        if (copySource) {
          const source = copySource.split('/').slice(1).map(decodeURIComponent).join('/');
          try {
            await store.copy(bucket, source, key);
          } catch (error) {
            if (error instanceof StorageError && error.code === 'NOT_FOUND') {
              return new Response('source not found', { status: 404 });
            }
            throw error;
          }
          return Response.json({ key, size: 0 });
        }
        const body = await request.arrayBuffer();
        const contentType = request.headers.get('content-type') ?? undefined;
        const result = await store.put(bucket, key, new Blob([body]), contentType ? { contentType } : undefined);
        return Response.json(result);
      }

      if (request.method === 'DELETE') {
        await store.delete(bucket, key);
        return new Response(null, { status: 204 });
      }

      const found = await store.get(bucket, key);
      if (!found) {
        return new Response(null, { status: 404 });
      }
      const headers = new Headers({ 'content-length': String(found.size) });
      if (found.contentType) headers.set('content-type', found.contentType);
      if (found.etag) headers.set('etag', found.etag);
      if (found.lastModified) headers.set('last-modified', found.lastModified.toUTCString());
      if (request.method === 'HEAD') {
        return new Response(null, { headers });
      }
      return new Response(found.body, { headers });
    },
  });
}

let remoteStore: MemoryBackend | undefined;
let remoteServer: ReturnType<typeof Bun.serve> | undefined;

runStorageContract(
  'RemoteBackend (over the documented HTTP shape)',
  () => {
    remoteStore = new MemoryBackend();
    remoteServer = serveRemoteStorage(remoteStore);
    return {
      backend: new RemoteBackend({ endpoint: `http://localhost:${remoteServer.port}`, accessKey: 'contract-token' }),
      reset: () => remoteStore?.clear(),
    };
  },
  { rejectsTraversalKeys: true },
);

afterAll(() => {
  remoteServer?.stop(true);
});
