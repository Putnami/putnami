import { afterEach, describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { type S3BackendOptions, S3Backend, mapS3ListResponse } from '../src/backend/s3.backend';
import { StorageError } from '../src/errors';

// Dummy credentials are enough for everything that does not touch the network:
// presigning and url() are pure, local SigV4 computations.
const CREDS: S3BackendOptions = {
  accessKeyId: 'AKIAEXAMPLE',
  secretAccessKey: 'secret-key-example',
  region: 'us-east-1',
  endpoint: 'http://localhost:9000',
};

describe('S3Backend (unit, no network)', () => {
  describe('mapS3ListResponse', () => {
    it('maps contents, common prefixes, truncation and continuation token', () => {
      const result = mapS3ListResponse({
        contents: [
          { key: 'a.txt', size: 3, eTag: '"abc123"', lastModified: '2026-01-01T00:00:00.000Z' },
          { key: 'b.txt', size: 5 },
        ],
        commonPrefixes: [{ prefix: 'images/' }, { prefix: 'docs/' }],
        isTruncated: true,
        nextContinuationToken: 'next-token',
      });

      expect(result.objects).toEqual([
        { key: 'a.txt', size: 3, etag: 'abc123', lastModified: new Date('2026-01-01T00:00:00.000Z') },
        { key: 'b.txt', size: 5, etag: undefined, lastModified: undefined },
      ]);
      expect(result.prefixes).toEqual(['images/', 'docs/']);
      expect(result.isTruncated).toBe(true);
      expect(result.continuationToken).toBe('next-token');
    });

    it('defaults an empty response to empty objects/prefixes and not-truncated', () => {
      const result = mapS3ListResponse({});
      expect(result.objects).toEqual([]);
      expect(result.prefixes).toEqual([]);
      expect(result.isTruncated).toBe(false);
      expect(result.continuationToken).toBeUndefined();
    });
  });

  describe('signed URLs (client-side SigV4)', () => {
    const backend = new S3Backend(CREDS);

    it('signs an upload URL locally without a server round-trip', async () => {
      const signed = await backend.signedUploadUrl('avatars', 'u1/photo.png', {
        expiresIn: '15m',
        contentType: 'image/png',
      });

      const url = new URL(signed.url);
      expect(url.host).toBe('localhost:9000');
      expect(url.pathname).toBe('/avatars/u1/photo.png');
      expect(url.searchParams.get('X-Amz-Algorithm')).toBe('AWS4-HMAC-SHA256');
      expect(url.searchParams.has('X-Amz-Signature')).toBe(true);
      expect(url.searchParams.get('X-Amz-Expires')).toBe('900');
      expect(signed.method).toBe('PUT');
      expect(signed.expiresAt).toBeInstanceOf(Date);
      expect(signed.expiresAt.getTime()).toBeGreaterThan(Date.now());
    });

    it('signs a download URL with the requested expiry', async () => {
      const signed = await backend.signedDownloadUrl('avatars', 'u1/photo.png', { expiresIn: '1h' });
      const url = new URL(signed.url);
      expect(url.searchParams.get('X-Amz-Expires')).toBe('3600');
      expect(url.searchParams.has('X-Amz-Signature')).toBe(true);
      expect(signed.method).toBe('GET');
    });

    specTest(
      'defaults to a 15m expiry when none is given',
      {
        feature: 'typescript/object-storage',
        requirement: 'bounded-expiry',
        check: 'the-s3-backend-defaults-to-a-15-minute-expiry',
      },
      async () => {
        const signed = await backend.signedUploadUrl('avatars', 'u1/photo.png');
        expect(new URL(signed.url).searchParams.get('X-Amz-Expires')).toBe('900');
      },
    );

    specTest(
      'rejects an expiry beyond the 7d cap',
      {
        feature: 'typescript/object-storage',
        requirement: 'bounded-expiry',
        check: 'the-s3-backend-refuses-an-expiry-beyond-7-days',
      },
      async () => {
        await expect(backend.signedUploadUrl('avatars', 'k', { expiresIn: '30d' })).rejects.toThrow(StorageError);
      },
    );

    it('rejects path-traversal keys before signing', async () => {
      await expect(backend.signedUploadUrl('avatars', '../secret')).rejects.toThrow(StorageError);
      await expect(backend.signedDownloadUrl('avatars', '../secret')).rejects.toThrow(StorageError);
    });
  });

  describe('url()', () => {
    it('builds a path-style endpoint URL by default', () => {
      const backend = new S3Backend(CREDS);
      expect(backend.url('avatars', 'u1/photo.png')).toBe('http://localhost:9000/avatars/u1/photo.png');
    });

    it('uses publicBaseUrl when configured', () => {
      const backend = new S3Backend({ ...CREDS, publicBaseUrl: 'https://cdn.example.com/' });
      expect(backend.url('avatars', 'u1/photo.png')).toBe('https://cdn.example.com/u1/photo.png');
    });

    it('builds a virtual-hosted-style URL', () => {
      const backend = new S3Backend({ ...CREDS, virtualHostedStyle: true });
      expect(backend.url('avatars', 'u1/photo.png')).toBe('http://avatars.localhost:9000/u1/photo.png');
    });

    it('falls back to the AWS region host when no endpoint is set', () => {
      const backend = new S3Backend({ accessKeyId: 'x', secretAccessKey: 'y', region: 'eu-west-1' });
      expect(backend.url('avatars', 'k.txt')).toBe('https://s3.eu-west-1.amazonaws.com/avatars/k.txt');
    });

    it('rejects path-traversal keys', () => {
      const backend = new S3Backend(CREDS);
      expect(() => backend.url('avatars', '../secret')).toThrow(StorageError);
    });
  });

  describe('copy', () => {
    const RealS3Client = Bun.S3Client;

    afterEach(() => {
      (Bun as { S3Client: typeof Bun.S3Client }).S3Client = RealS3Client;
    });

    function installStubClient(exists: boolean): {
      sourceFile: { exists: () => Promise<boolean> };
      fileCalls: [string, unknown][];
      writeCalls: [string, unknown, unknown][];
    } {
      const fileCalls: [string, unknown][] = [];
      const writeCalls: [string, unknown, unknown][] = [];
      const sourceFile = { exists: () => Promise.resolve(exists) };
      class StubS3Client {
        file = (path: string, options: unknown) => {
          fileCalls.push([path, options]);
          return sourceFile;
        };
        write = (path: string, body: unknown, options: unknown) => {
          writeCalls.push([path, body, options]);
          return Promise.resolve(undefined);
        };
      }
      (Bun as { S3Client: unknown }).S3Client = StubS3Client;
      return { sourceFile, fileCalls, writeCalls };
    }

    it('writes the destination from the source file handle', async () => {
      const { sourceFile, fileCalls, writeCalls } = installStubClient(true);
      const backend = new S3Backend(CREDS);
      await backend.copy('avatars', 'src.txt', 'dest.txt');

      expect(fileCalls).toEqual([['src.txt', { bucket: 'avatars' }]]);
      expect(writeCalls).toEqual([['dest.txt', sourceFile, { bucket: 'avatars', acl: undefined }]]);
    });

    it('forwards the configured ACL to the copied object', async () => {
      const { writeCalls } = installStubClient(true);
      const backend = new S3Backend({ ...CREDS, acl: 'public-read' });
      await backend.copy('avatars', 'src.txt', 'dest.txt');

      expect(writeCalls[0]?.[2]).toEqual({ bucket: 'avatars', acl: 'public-read' });
    });

    it('throws NOT_FOUND without writing when the source is missing', async () => {
      const { writeCalls } = installStubClient(false);
      const backend = new S3Backend(CREDS);
      const error = await backend.copy('avatars', 'missing.txt', 'dest.txt').then(
        () => null,
        (e) => e,
      );
      expect(error).toBeInstanceOf(StorageError);
      expect((error as StorageError).code).toBe('NOT_FOUND');
      expect(writeCalls).toHaveLength(0);
    });
  });

  describe('close()', () => {
    it('resolves without error', async () => {
      const backend = new S3Backend(CREDS);
      await expect(backend.close()).resolves.toBeUndefined();
    });
  });

  // A black-holed endpoint must not hang the caller forever: every network op
  // is raced against `timeoutMs` and rejects with a typed TIMEOUT StorageError.
  describe('operation timeout', () => {
    const RealS3Client = Bun.S3Client;
    // Every method returns a promise that never settles, simulating a stalled
    // endpoint. `file()` returns a handle whose stat/stream/exists also stall.
    const stallForever = () => new Promise<never>(() => {});
    const stallingFile = {
      stat: stallForever,
      stream: () => new ReadableStream(),
      exists: stallForever,
    };
    // A constructable stand-in for Bun.S3Client (the backend calls `new`), so it
    // must be a class — an arrow function is not a constructor.
    class StallingS3Client {
      write = stallForever;
      stat = stallForever;
      delete = stallForever;
      list = stallForever;
      exists = stallForever;
      file = () => stallingFile;
    }

    afterEach(() => {
      (Bun as { S3Client: typeof Bun.S3Client }).S3Client = RealS3Client;
    });

    function stallingBackend(): S3Backend {
      (Bun as { S3Client: unknown }).S3Client = StallingS3Client;
      return new S3Backend({ ...CREDS, timeoutMs: 50 });
    }

    it('rejects get with a TIMEOUT StorageError when the endpoint stalls', async () => {
      const backend = stallingBackend();
      const start = Date.now();
      const error = await backend.get('avatars', 'k.txt').then(
        () => null,
        (e) => e,
      );
      expect(error).toBeInstanceOf(StorageError);
      expect((error as StorageError).code).toBe('TIMEOUT');
      expect(Date.now() - start).toBeLessThan(2000);
    });

    it('rejects body reads with a TIMEOUT StorageError when a download stream stalls', async () => {
      const bodyStallingFile = {
        stat: () =>
          Promise.resolve({
            size: 1,
            type: 'text/plain',
            etag: '"etag"',
            lastModified: new Date('2026-01-01T00:00:00.000Z'),
          } as Bun.S3Stats),
        stream: () =>
          new ReadableStream({
            pull: stallForever,
          }),
        exists: () => Promise.resolve(true),
      };
      class BodyStallingS3Client {
        file = () => bodyStallingFile;
      }
      (Bun as { S3Client: unknown }).S3Client = BodyStallingS3Client;

      const backend = new S3Backend({ ...CREDS, timeoutMs: 50 });
      const result = await backend.get('avatars', 'k.txt');
      if (!result) throw new Error('expected S3 object');

      const start = Date.now();
      const error = await new Response(result.body).text().then(
        () => null,
        (e) => e,
      );

      expect(error).toBeInstanceOf(StorageError);
      expect((error as StorageError).code).toBe('TIMEOUT');
      expect(Date.now() - start).toBeLessThan(2000);
    });

    it('rejects put, list, delete, exists and copy within the bound', async () => {
      const backend = stallingBackend();
      const expectTimeout = async (promise: Promise<unknown>) => {
        const error = await promise.then(
          () => null,
          (e) => e,
        );
        expect(error).toBeInstanceOf(StorageError);
        expect((error as StorageError).code).toBe('TIMEOUT');
      };

      const start = Date.now();
      await Promise.all([
        expectTimeout(backend.put('avatars', 'k.txt', new Blob(['x']))),
        expectTimeout(backend.list('avatars')),
        expectTimeout(backend.delete('avatars', 'k.txt')),
        expectTimeout(backend.exists('avatars', 'k.txt')),
        expectTimeout(backend.copy('avatars', 'a.txt', 'b.txt')),
      ]);
      expect(Date.now() - start).toBeLessThan(2000);
    });
  });
});

// ---------------------------------------------------------------------------
// Integration suite — exercises a live S3-compatible target (MinIO, R2, AWS).
// Skipped unless PUTNAMI_S3_TEST_ENDPOINT is set, so CI stays hermetic.
//
//   PUTNAMI_S3_TEST_ENDPOINT=http://localhost:9000 \
//   PUTNAMI_S3_TEST_BUCKET=putnami-test \
//   S3_ACCESS_KEY_ID=minioadmin S3_SECRET_ACCESS_KEY=minioadmin \
//   bun test test/s3-backend.test.ts
// ---------------------------------------------------------------------------
const INTEGRATION_ENDPOINT = process.env.PUTNAMI_S3_TEST_ENDPOINT;
const INTEGRATION_BUCKET = process.env.PUTNAMI_S3_TEST_BUCKET ?? 'putnami-test';
const itIntegration = INTEGRATION_ENDPOINT ? it : it.skip;

describe('S3Backend (integration, requires PUTNAMI_S3_TEST_ENDPOINT)', () => {
  function makeBackend(): S3Backend {
    return new S3Backend({
      endpoint: INTEGRATION_ENDPOINT,
      accessKeyId: process.env.S3_ACCESS_KEY_ID ?? process.env.AWS_ACCESS_KEY_ID,
      secretAccessKey: process.env.S3_SECRET_ACCESS_KEY ?? process.env.AWS_SECRET_ACCESS_KEY,
      region: process.env.S3_REGION ?? process.env.AWS_REGION ?? 'us-east-1',
    });
  }

  // Unique prefix per run so parallel runs and reruns never collide.
  const prefix = `it-${Date.now()}-${Math.random().toString(16).slice(2)}/`;

  itIntegration('round-trips put → get with content type', async () => {
    const backend = makeBackend();
    const key = `${prefix}hello.txt`;
    const put = await backend.put(INTEGRATION_BUCKET, key, new Blob(['hello world']), {
      contentType: 'text/plain',
    });
    expect(put.size).toBe(11);

    const got = await backend.get(INTEGRATION_BUCKET, key);
    expect(got).not.toBeNull();
    expect(got?.size).toBe(11);
    expect(got?.contentType).toBe('text/plain');
    expect(await new Response(got?.body).text()).toBe('hello world');

    await backend.delete(INTEGRATION_BUCKET, key);
  });

  itIntegration('reports existence and returns null after delete', async () => {
    const backend = makeBackend();
    const key = `${prefix}exists.txt`;
    expect(await backend.exists(INTEGRATION_BUCKET, key)).toBe(false);
    expect(await backend.get(INTEGRATION_BUCKET, key)).toBeNull();

    await backend.put(INTEGRATION_BUCKET, key, new Blob(['x']));
    expect(await backend.exists(INTEGRATION_BUCKET, key)).toBe(true);

    await backend.delete(INTEGRATION_BUCKET, key);
    expect(await backend.exists(INTEGRATION_BUCKET, key)).toBe(false);
  });

  itIntegration('copies an object and lists by prefix', async () => {
    const backend = makeBackend();
    const source = `${prefix}list/a.txt`;
    const destination = `${prefix}list/b.txt`;
    await backend.put(INTEGRATION_BUCKET, source, new Blob(['copy me']));

    await backend.copy(INTEGRATION_BUCKET, source, destination);
    const copied = await backend.get(INTEGRATION_BUCKET, destination);
    expect(await new Response(copied?.body).text()).toBe('copy me');

    const listed = await backend.list(INTEGRATION_BUCKET, { prefix: `${prefix}list/` });
    expect(listed.objects.map((o) => o.key).sort()).toEqual([source, destination].sort());

    await backend.delete(INTEGRATION_BUCKET, source);
    await backend.delete(INTEGRATION_BUCKET, destination);
  });

  itIntegration('throws when copying a missing source', async () => {
    const backend = makeBackend();
    await expect(backend.copy(INTEGRATION_BUCKET, `${prefix}missing.txt`, `${prefix}dest.txt`)).rejects.toThrow(
      'Source object not found',
    );
  });

  itIntegration('uploads and downloads via client-side presigned URLs', async () => {
    const backend = makeBackend();
    const key = `${prefix}presigned.txt`;

    const upload = await backend.signedUploadUrl(INTEGRATION_BUCKET, key, { contentType: 'text/plain' });
    const putResponse = await fetch(upload.url, {
      method: 'PUT',
      body: 'presigned body',
      headers: { 'content-type': 'text/plain' },
    });
    expect(putResponse.ok).toBe(true);

    const download = await backend.signedDownloadUrl(INTEGRATION_BUCKET, key);
    const getResponse = await fetch(download.url);
    expect(await getResponse.text()).toBe('presigned body');

    await backend.delete(INTEGRATION_BUCKET, key);
  });
});
