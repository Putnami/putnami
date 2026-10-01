import { afterAll, beforeEach, describe, expect, it, mock } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { RemoteBackend, StorageRemoteError } from '../src/backend/remote.backend';
import { StorageError } from '../src/errors';

const originalFetch = globalThis.fetch;
const fetchMock = mock(async (_input: string | URL | Request, _init?: RequestInit) => new Response(null));

function jsonResponse(body: unknown, init?: ResponseInit): Response {
  return new Response(JSON.stringify(body), {
    ...init,
    headers: {
      'content-type': 'application/json',
      ...(init?.headers ?? {}),
    },
  });
}

describe('RemoteBackend', () => {
  beforeEach(() => {
    fetchMock.mockReset();
    globalThis.fetch = fetchMock as typeof fetch;
  });

  afterAll(() => {
    globalThis.fetch = originalFetch;
  });

  describe('constructor', () => {
    it('should strip trailing slash from endpoint', () => {
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com/' });
      expect(backend.url('test', 'file.txt')).toBe('https://storage.example.com/test/file.txt');
    });

    it('should preserve endpoint without trailing slash', () => {
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com' });
      expect(backend.url('test', 'file.txt')).toBe('https://storage.example.com/test/file.txt');
    });
  });

  describe('authentication', () => {
    it('should send only a Bearer accessKey and no secret-derived header', async () => {
      fetchMock.mockResolvedValueOnce(new Response(null, { status: 200 }));
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com', accessKey: 'access-token' });

      await backend.exists('avatars', 'file.txt');

      const [, init] = fetchMock.mock.calls[0]!;
      const headers = (init?.headers ?? {}) as Record<string, string>;
      expect(headers.authorization).toBe('Bearer access-token');
      // No client-side secret-key signing: the only credential header is the bearer token.
      const headerKeys = Object.keys(headers).map((k) => k.toLowerCase());
      expect(headerKeys).toEqual(['authorization']);
      expect(headerKeys.some((k) => k.includes('signature') || k.includes('secret') || k.startsWith('x-amz'))).toBe(
        false,
      );
    });

    it('should send no Authorization header when accessKey is absent', async () => {
      fetchMock.mockResolvedValueOnce(new Response(null, { status: 200 }));
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com' });

      await backend.exists('avatars', 'file.txt');

      const [, init] = fetchMock.mock.calls[0]!;
      const headers = (init?.headers ?? {}) as Record<string, string>;
      expect('authorization' in headers).toBe(false);
    });
  });

  describe('put', () => {
    it('should upload data with auth and metadata headers', async () => {
      fetchMock.mockResolvedValueOnce(jsonResponse({ key: 'file.txt', size: 5, etag: 'etag-1' }));
      const backend = new RemoteBackend({
        endpoint: 'https://storage.example.com/',
        accessKey: 'access-token',
        timeoutMs: 1234,
      });

      const result = await backend.put('avatars', 'file.txt', Buffer.from('hello'), {
        contentType: 'text/plain',
        cacheControl: 'public, max-age=60',
        contentDisposition: 'inline',
        custom: { owner: 'alice' },
      });

      expect(result).toEqual({ key: 'file.txt', size: 5, etag: 'etag-1' });

      const [url, init] = fetchMock.mock.calls[0]!;
      expect(url).toBe('https://storage.example.com/avatars/file.txt');
      expect(init?.method).toBe('PUT');
      expect(init?.body).toBeInstanceOf(Uint8Array);
      expect(init?.signal).toBeInstanceOf(AbortSignal);
      expect(init?.headers).toMatchObject({
        authorization: 'Bearer access-token',
        'content-type': 'text/plain',
        'cache-control': 'public, max-age=60',
        'content-disposition': 'inline',
        'x-meta-owner': 'alice',
      });
    });

    it('should throw a StorageRemoteError on failed upload', async () => {
      fetchMock.mockResolvedValueOnce(new Response('bad request', { status: 400 }));
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com' });

      await expect(backend.put('avatars', 'file.txt', new Blob(['hello']))).rejects.toThrow(StorageRemoteError);
    });
  });

  describe('get', () => {
    it('should parse object metadata from response headers', async () => {
      fetchMock.mockResolvedValueOnce(
        new Response('hello', {
          status: 200,
          headers: {
            'content-length': '5',
            'content-type': 'text/plain',
            etag: 'etag-2',
            'last-modified': 'Wed, 21 Oct 2015 07:28:00 GMT',
            'x-meta-owner': 'alice',
            'x-meta-purpose': 'test',
          },
        }),
      );
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com', accessKey: 'access-token' });

      const result = await backend.get('avatars', 'file.txt');

      expect(result?.key).toBe('file.txt');
      expect(result?.size).toBe(5);
      expect(result?.contentType).toBe('text/plain');
      expect(result?.etag).toBe('etag-2');
      expect(result?.lastModified?.toISOString()).toBe('2015-10-21T07:28:00.000Z');
      expect(result?.metadata).toEqual({ owner: 'alice', purpose: 'test' });

      const [url, init] = fetchMock.mock.calls[0]!;
      expect(url).toBe('https://storage.example.com/avatars/file.txt');
      expect(init?.method).toBe('GET');
      expect(init?.headers).toMatchObject({ authorization: 'Bearer access-token' });
    });

    it('should return null when the object is missing', async () => {
      fetchMock.mockResolvedValueOnce(new Response(null, { status: 404 }));
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com' });

      await expect(backend.get('avatars', 'missing.txt')).resolves.toBeNull();
    });

    it('should throw a StorageRemoteError on failed download', async () => {
      fetchMock.mockResolvedValueOnce(new Response('boom', { status: 500 }));
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com' });

      await expect(backend.get('avatars', 'file.txt')).rejects.toThrow(StorageRemoteError);
    });
  });

  describe('delete', () => {
    it('should delete an object', async () => {
      fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }));
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com', accessKey: 'access-token' });

      await expect(backend.delete('avatars', 'file.txt')).resolves.toBeUndefined();

      const [url, init] = fetchMock.mock.calls[0]!;
      expect(url).toBe('https://storage.example.com/avatars/file.txt');
      expect(init?.method).toBe('DELETE');
      expect(init?.headers).toMatchObject({ authorization: 'Bearer access-token' });
    });

    it('should ignore missing objects on delete', async () => {
      fetchMock.mockResolvedValueOnce(new Response(null, { status: 404 }));
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com' });

      await expect(backend.delete('avatars', 'missing.txt')).resolves.toBeUndefined();
    });

    it('should throw a StorageRemoteError on failed delete', async () => {
      fetchMock.mockResolvedValueOnce(new Response('boom', { status: 500 }));
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com' });

      await expect(backend.delete('avatars', 'file.txt')).rejects.toThrow(StorageRemoteError);
    });
  });

  describe('list', () => {
    it('should send list filters as query parameters', async () => {
      fetchMock.mockResolvedValueOnce(
        jsonResponse({
          objects: [{ key: 'images/a.png', size: 1 }],
          prefixes: ['images/'],
          isTruncated: true,
          continuationToken: 'next-page',
        }),
      );
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com', accessKey: 'access-token' });

      const result = await backend.list('avatars', {
        prefix: 'images/',
        delimiter: '/',
        maxKeys: 10,
        continuationToken: 'cursor-1',
      });

      expect(result).toEqual({
        objects: [{ key: 'images/a.png', size: 1 }],
        prefixes: ['images/'],
        isTruncated: true,
        continuationToken: 'next-page',
      });

      const [url, init] = fetchMock.mock.calls[0]!;
      expect(url).toBe(
        'https://storage.example.com/avatars?prefix=images%2F&delimiter=%2F&max-keys=10&continuation-token=cursor-1',
      );
      expect(init?.method).toBe('GET');
      expect(init?.headers).toMatchObject({ authorization: 'Bearer access-token' });
    });

    it('should throw a StorageRemoteError on failed listing', async () => {
      fetchMock.mockResolvedValueOnce(new Response('boom', { status: 500 }));
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com' });

      await expect(backend.list('avatars')).rejects.toThrow(StorageRemoteError);
    });

    specTest(
      'should return lastModified as a Date, like every in-process backend',
      {
        feature: 'typescript/object-storage',
        requirement: 'typed-shapes',
        check: 'the-remote-backend-returns-last-modified-as-a-date',
      },
      async () => {
        fetchMock.mockResolvedValueOnce(
          jsonResponse({
            objects: [
              { key: 'a.png', size: 1, lastModified: '2026-08-17T10:00:00.000Z' },
              { key: 'b.png', size: 2 },
            ],
            prefixes: [],
            isTruncated: false,
          }),
        );
        const backend = new RemoteBackend({ endpoint: 'https://storage.example.com' });

        const result = await backend.list('avatars');

        // JSON has no date type. Handing the parsed payload back untouched made
        // `lastModified` a string at runtime while ObjectInfo declares a Date —
        // and only on the remote backend, so `.getTime()` worked in tests against
        // memory/file and threw in production.
        expect(result.objects[0]?.lastModified).toBeInstanceOf(Date);
        expect(result.objects[0]?.lastModified?.toISOString()).toBe('2026-08-17T10:00:00.000Z');
        // An object the server reports without a timestamp keeps it absent
        // rather than gaining an Invalid Date.
        expect(result.objects[1]).toBeDefined();
        expect('lastModified' in (result.objects[1] ?? {})).toBe(false);
      },
    );

    specTest(
      'should drop an unparseable lastModified rather than surface an Invalid Date',
      {
        feature: 'typescript/object-storage',
        requirement: 'typed-shapes',
        check: 'an-unparseable-last-modified-is-dropped-rather-than-surfaced-invalid',
      },
      async () => {
        fetchMock.mockResolvedValueOnce(
          jsonResponse({
            objects: [{ key: 'a.png', size: 1, lastModified: 'not-a-date' }],
            prefixes: [],
            isTruncated: false,
          }),
        );
        const backend = new RemoteBackend({ endpoint: 'https://storage.example.com' });

        const result = await backend.list('avatars');

        expect('lastModified' in (result.objects[0] ?? {})).toBe(false);
      },
    );
  });

  describe('exists', () => {
    it('should return true when the object exists', async () => {
      fetchMock.mockResolvedValueOnce(new Response(null, { status: 200 }));
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com' });

      await expect(backend.exists('avatars', 'file.txt')).resolves.toBe(true);

      const [url, init] = fetchMock.mock.calls[0]!;
      expect(url).toBe('https://storage.example.com/avatars/file.txt');
      expect(init?.method).toBe('HEAD');
    });

    it('should return false when the object does not exist', async () => {
      fetchMock.mockResolvedValueOnce(new Response(null, { status: 404 }));
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com' });

      await expect(backend.exists('avatars', 'missing.txt')).resolves.toBe(false);
    });
  });

  describe('copy', () => {
    it('should send the source object in the x-copy-source header', async () => {
      fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }));
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com', accessKey: 'access-token' });

      await expect(backend.copy('avatars', 'from.txt', 'to.txt')).resolves.toBeUndefined();

      const [url, init] = fetchMock.mock.calls[0]!;
      expect(url).toBe('https://storage.example.com/avatars/to.txt');
      expect(init?.method).toBe('PUT');
      expect(init?.headers).toMatchObject({
        authorization: 'Bearer access-token',
        'x-copy-source': 'avatars/from.txt',
      });
    });

    it('should throw a StorageRemoteError on failed copy', async () => {
      fetchMock.mockResolvedValueOnce(new Response('boom', { status: 500 }));
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com' });

      await expect(backend.copy('avatars', 'from.txt', 'to.txt')).rejects.toThrow(StorageRemoteError);
    });

    it('should map a 404 copy source to a StorageError with code NOT_FOUND', async () => {
      // A copy writes the destination, so a 404 can only mean the source is
      // missing. Surface it as the same typed NOT_FOUND error the in-process
      // backends throw so callers branch uniformly across backends.
      fetchMock.mockResolvedValueOnce(new Response('not found', { status: 404 }));
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com' });

      const error = await backend.copy('avatars', 'missing.txt', 'to.txt').then(
        () => null,
        (e) => e,
      );
      expect(error).toBeInstanceOf(StorageError);
      expect(error).not.toBeInstanceOf(StorageRemoteError);
      expect((error as StorageError).code).toBe('NOT_FOUND');
      expect((error as StorageError).message).toContain('Source object not found');
    });
  });

  describe('url', () => {
    it('should return the full URL for a bucket/key', () => {
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com' });
      expect(backend.url('avatars', 'user-123/photo.png')).toBe(
        'https://storage.example.com/avatars/user-123/photo.png',
      );
    });
  });

  describe('signedUploadUrl / signedDownloadUrl', () => {
    it('should request and normalize a signed upload URL', async () => {
      fetchMock.mockResolvedValueOnce(
        jsonResponse({
          url: 'https://upload.example.com/file.txt',
          expiresAt: '2026-03-10T12:00:00.000Z',
          headers: { 'x-upload-token': 'abc' },
        }),
      );
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com', accessKey: 'access-token' });

      const result = await backend.signedUploadUrl('avatars', 'file.txt', {
        expiresIn: '15m',
        maxFileSize: '10mb',
        allowedMimeTypes: ['text/plain'],
        metadata: { owner: 'alice' },
        contentType: 'text/plain',
      });

      expect(result.method).toBe('PUT');
      expect(result.url).toBe('https://upload.example.com/file.txt');
      expect(result.expiresAt).toBeInstanceOf(Date);
      expect(result.expiresAt.toISOString()).toBe('2026-03-10T12:00:00.000Z');
      expect(result.headers).toEqual({ 'x-upload-token': 'abc' });

      const [url, init] = fetchMock.mock.calls[0]!;
      expect(url).toBe('https://storage.example.com/avatars/_sign/upload');
      expect(init?.method).toBe('POST');
      expect(init?.headers).toMatchObject({
        authorization: 'Bearer access-token',
        'content-type': 'application/json',
      });
      expect(JSON.parse(String(init?.body))).toEqual({
        key: 'file.txt',
        expiresIn: '15m',
        maxFileSize: '10mb',
        allowedMimeTypes: ['text/plain'],
        metadata: { owner: 'alice' },
        contentType: 'text/plain',
      });
    });

    it('should request and normalize a signed download URL', async () => {
      fetchMock.mockResolvedValueOnce(
        jsonResponse({
          url: 'https://download.example.com/file.txt',
          expiresAt: '2026-03-10T12:00:00.000Z',
          headers: { 'x-download-token': 'abc' },
        }),
      );
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com' });

      const result = await backend.signedDownloadUrl('avatars', 'file.txt', {
        expiresIn: '15m',
        contentDisposition: 'attachment; filename="file.txt"',
      });

      expect(result.method).toBe('GET');
      expect(result.expiresAt).toBeInstanceOf(Date);

      const [url, init] = fetchMock.mock.calls[0]!;
      expect(url).toBe('https://storage.example.com/avatars/_sign/download');
      expect(init?.method).toBe('POST');
      expect(JSON.parse(String(init?.body))).toEqual({
        key: 'file.txt',
        expiresIn: '15m',
        contentDisposition: 'attachment; filename="file.txt"',
      });
    });

    it('should throw a StorageRemoteError when signing fails', async () => {
      fetchMock.mockResolvedValueOnce(new Response('boom', { status: 500 }));
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com' });

      await expect(backend.signedUploadUrl('avatars', 'file.txt')).rejects.toThrow(StorageRemoteError);
      fetchMock.mockResolvedValueOnce(new Response('boom', { status: 500 }));
      await expect(backend.signedDownloadUrl('avatars', 'file.txt')).rejects.toThrow(StorageRemoteError);
    });
  });

  describe('close', () => {
    it('should not throw on close', async () => {
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com' });
      await expect(backend.close()).resolves.toBeUndefined();
    });
  });

  describe('key encoding and validation', () => {
    it('percent-encodes special characters in bucket and key', async () => {
      fetchMock.mockResolvedValueOnce(new Response(null, { status: 200 }));
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com' });
      await backend.exists('my bucket', 'reports/q1 2026?draft#v2.pdf');
      const [url] = fetchMock.mock.calls[0]!;
      expect(url).toBe('https://storage.example.com/my%20bucket/reports/q1%202026%3Fdraft%23v2.pdf');
    });

    it('preserves "/" folder separators in keys', () => {
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com' });
      expect(backend.url('avatars', 'user-123/photo.png')).toBe(
        'https://storage.example.com/avatars/user-123/photo.png',
      );
    });

    it('rejects path-traversal keys before issuing a request', async () => {
      const backend = new RemoteBackend({ endpoint: 'https://storage.example.com' });
      await expect(backend.get('avatars', '../secrets/key.pem')).rejects.toThrow(/path traversal/);
      expect(fetchMock.mock.calls.length).toBe(0);
    });
  });
});

describe('StorageRemoteError', () => {
  specTest(
    'should contain operation and status but not body in message',
    {
      feature: 'typescript/object-storage',
      requirement: 'error-body-not-serialized',
      check: 'the-error-message-carries-operation-and-status-but-not-the-body',
    },
    () => {
      const error = new StorageRemoteError('PUT test/file.txt', 500, 'Internal Server Error');
      expect(error.operation).toBe('PUT test/file.txt');
      expect(error.statusCode).toBe(500);
      expect(error.name).toBe('StorageRemoteError');
      expect(error.message).toContain('500');
      expect(error.message).not.toContain('Internal Server Error');
    },
  );

  specTest(
    'should store response body as non-enumerable property',
    {
      feature: 'typescript/object-storage',
      requirement: 'error-body-not-serialized',
      check: 'the-response-body-is-stored-non-enumerably',
    },
    () => {
      const error = new StorageRemoteError('PUT test/file.txt', 500, 'Internal Server Error');
      expect(error.responseBody).toBe('Internal Server Error');
      expect(JSON.stringify(error)).not.toContain('Internal Server Error');
      expect(Object.keys(error)).not.toContain('responseBody');
    },
  );

  it('should truncate large response bodies', () => {
    const largeBody = 'x'.repeat(10_000);
    const error = new StorageRemoteError('GET test/file.txt', 500, largeBody);
    expect(error.responseBody.length).toBeLessThan(largeBody.length);
    expect(error.responseBody).toContain('… [truncated]');
  });

  it('should be an instance of Error', () => {
    const error = new StorageRemoteError('GET test/file.txt', 404, 'Not Found');
    expect(error).toBeInstanceOf(Error);
    expect(error).toBeInstanceOf(StorageRemoteError);
  });
});
