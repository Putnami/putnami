import { afterEach, describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { MemoryBackend } from '../src/backend/memory.backend';
import { Bucket } from '../src/bucket/bucket.builders';
import { bucketRegistry } from '../src/bucket/bucket.registry';
import { StorageClient, StorageValidationError } from '../src/client/storage.client';

afterEach(() => {
  bucketRegistry.clear();
});

describe('StorageClient', () => {
  function createClient(
    bucketName: string,
    options: { maxFileSize?: string; allowedMimeTypes?: string[]; public?: boolean } = {},
  ) {
    const bucket = Bucket(bucketName, options);
    const backend = new MemoryBackend();
    return { client: new StorageClient(bucket, backend), backend };
  }

  describe('properties', () => {
    it('should expose bucketName', () => {
      const { client } = createClient('props-test');
      expect(client.bucketName).toBe('props-test');
    });

    it('should expose isPublic', () => {
      const { client: pub } = createClient('pub-client', { public: true });
      const { client: priv } = createClient('priv-client', { public: false });
      expect(pub.isPublic).toBe(true);
      expect(priv.isPublic).toBe(false);
    });
  });

  describe('put', () => {
    it('should upload a Blob', async () => {
      const { client } = createClient('put-blob');
      const result = await client.put('file.txt', new Blob(['hello']), { contentType: 'text/plain' });
      expect(result.key).toBe('file.txt');
      expect(result.size).toBe(5);
    });

    it('should upload a Buffer', async () => {
      const { client } = createClient('put-buffer');
      const result = await client.put('file.txt', Buffer.from('hello'));
      expect(result.key).toBe('file.txt');
      expect(result.size).toBe(5);
    });

    specTest(
      'should validate file size for Blob',
      {
        feature: 'typescript/object-storage',
        requirement: 'upload-constraints',
        check: 'a-blob-over-the-declared-size-limit-is-rejected',
      },
      async () => {
        const { client } = createClient('size-blob', { maxFileSize: '10b' });
        await expect(client.put('large.txt', new Blob(['this is too large for 10 bytes']))).rejects.toThrow(
          StorageValidationError,
        );
      },
    );

    specTest(
      'should validate file size for Buffer',
      {
        feature: 'typescript/object-storage',
        requirement: 'upload-constraints',
        check: 'a-buffer-over-the-declared-size-limit-is-rejected',
      },
      async () => {
        const { client } = createClient('size-buf', { maxFileSize: '5b' });
        await expect(client.put('large.txt', Buffer.from('too large'))).rejects.toThrow(StorageValidationError);
      },
    );

    specTest(
      'should validate MIME type for Blob',
      {
        feature: 'typescript/object-storage',
        requirement: 'upload-constraints',
        check: 'a-blob-with-a-disallowed-mime-type-is-rejected',
      },
      async () => {
        const { client } = createClient('mime-blob', { allowedMimeTypes: ['image/png'] });
        await expect(
          client.put('file.gif', new Blob(['data'], { type: 'image/gif' }), { contentType: 'image/gif' }),
        ).rejects.toThrow(StorageValidationError);
      },
    );

    it('should pass when Blob meets constraints', async () => {
      const { client } = createClient('valid-blob', {
        maxFileSize: '1mb',
        allowedMimeTypes: ['image/png'],
      });
      const result = await client.put('img.png', new Blob(['data']), { contentType: 'image/png' });
      expect(result.key).toBe('img.png');
    });

    specTest(
      'should reject a ReadableStream that exceeds the bucket size limit while streaming',
      {
        feature: 'typescript/object-storage',
        requirement: 'upload-constraints',
        check: 'a-stream-exceeding-the-limit-is-rejected-while-streaming',
      },
      async () => {
        const { client } = createClient('stream-toobig', { maxFileSize: '8b' });
        const stream = new ReadableStream({
          start(controller) {
            controller.enqueue(new TextEncoder().encode('this is well over eight bytes'));
            controller.close();
          },
        });
        // Size is unknown up front, so the cap is enforced as the bytes flow.
        await expect(client.put('file.txt', stream)).rejects.toThrow(StorageValidationError);
      },
    );

    specTest(
      'should accept a ReadableStream within the bucket size limit',
      {
        feature: 'typescript/object-storage',
        requirement: 'upload-constraints',
        check: 'a-stream-within-the-limit-is-accepted',
      },
      async () => {
        const { client } = createClient('stream-ok', { maxFileSize: '1mb' });
        const stream = new ReadableStream({
          start(controller) {
            controller.enqueue(new TextEncoder().encode('small'));
            controller.close();
          },
        });
        const result = await client.put('file.txt', stream);
        expect(result.key).toBe('file.txt');
        expect(result.size).toBe(5);
      },
    );

    specTest(
      'should reject a ReadableStream whose declared MIME type is not allowed',
      {
        feature: 'typescript/object-storage',
        requirement: 'upload-constraints',
        check: 'a-stream-with-a-disallowed-mime-type-is-rejected',
      },
      async () => {
        const { client } = createClient('stream-mime', { allowedMimeTypes: ['image/png'] });
        const stream = new ReadableStream({
          start(controller) {
            controller.enqueue(new TextEncoder().encode('gif data'));
            controller.close();
          },
        });
        await expect(client.put('file.gif', stream, { contentType: 'image/gif' })).rejects.toThrow(
          StorageValidationError,
        );
      },
    );
  });

  describe('get', () => {
    it('should retrieve an uploaded object', async () => {
      const { client } = createClient('get-test');
      await client.put('file.txt', new Blob(['hello']));

      const result = await client.get('file.txt');
      expect(result).not.toBeNull();
      expect(result?.key).toBe('file.txt');
    });

    it('should return null for nonexistent key', async () => {
      const { client } = createClient('get-missing');
      const result = await client.get('missing.txt');
      expect(result).toBeNull();
    });
  });

  describe('delete', () => {
    it('should delete an object', async () => {
      const { client } = createClient('delete-test');
      await client.put('file.txt', new Blob(['data']));

      await client.delete('file.txt');
      expect(await client.exists('file.txt')).toBe(false);
    });
  });

  describe('exists', () => {
    it('should return true for existing objects', async () => {
      const { client } = createClient('exists-test');
      await client.put('file.txt', new Blob(['data']));
      expect(await client.exists('file.txt')).toBe(true);
    });

    it('should return false for nonexistent objects', async () => {
      const { client } = createClient('exists-missing');
      expect(await client.exists('missing.txt')).toBe(false);
    });
  });

  describe('copy', () => {
    it('should copy an object', async () => {
      const { client } = createClient('copy-test');
      await client.put('original.txt', new Blob(['data']));

      await client.copy('original.txt', 'copied.txt');
      expect(await client.exists('original.txt')).toBe(true);
      expect(await client.exists('copied.txt')).toBe(true);
    });
  });

  describe('list', () => {
    it('should list objects', async () => {
      const { client } = createClient('list-test');
      await client.put('a.txt', new Blob(['a']));
      await client.put('b.txt', new Blob(['b']));

      const result = await client.list();
      expect(result.objects).toHaveLength(2);
    });

    it('should filter by prefix', async () => {
      const { client } = createClient('list-prefix');
      await client.put('images/a.png', new Blob(['a']));
      await client.put('docs/b.pdf', new Blob(['b']));

      const result = await client.list({ prefix: 'images/' });
      expect(result.objects).toHaveLength(1);
      expect(result.objects[0].key).toBe('images/a.png');
    });
  });

  describe('url', () => {
    it('should delegate to backend', () => {
      const { client } = createClient('url-test');
      expect(client.url('file.txt')).toBe('memory://url-test/file.txt');
    });
  });

  describe('signedUploadUrl / signedDownloadUrl', () => {
    it('should return a signed upload URL', async () => {
      const { client } = createClient('sign-upload');
      const result = await client.signedUploadUrl('file.txt', { expiresIn: '15m' });
      expect(result.method).toBe('PUT');
      expect(result.expiresAt).toBeInstanceOf(Date);
    });

    specTest(
      'should refuse a signed upload URL whose content type the bucket does not allow',
      {
        feature: 'typescript/object-storage',
        requirement: 'upload-constraints',
        check: 'a-signed-upload-url-with-a-disallowed-or-missing-mime-type-is-refused',
      },
      async () => {
        const { client } = createClient('sign-mime', { allowedMimeTypes: ['image/png'] });
        await expect(client.signedUploadUrl('a.gif', { contentType: 'image/gif' })).rejects.toThrow(
          StorageValidationError,
        );
        await expect(client.signedUploadUrl('a.png')).rejects.toThrow(StorageValidationError);
        const result = await client.signedUploadUrl('a.png', { contentType: 'image/png' });
        expect(result.method).toBe('PUT');
      },
    );

    it('should return a signed download URL', async () => {
      const { client } = createClient('sign-download');
      const result = await client.signedDownloadUrl('file.txt');
      expect(result.method).toBe('GET');
      expect(result.expiresAt).toBeInstanceOf(Date);
    });
  });
});

describe('key validation (enforced for every backend)', () => {
  // MemoryBackend performs no key validation of its own — these assert that the
  // StorageClient boundary rejects unsafe keys regardless of backend.
  function createClient(bucketName: string) {
    return { client: new StorageClient(Bucket(bucketName), new MemoryBackend()) };
  }

  specTest(
    'rejects path-traversal keys across key-taking methods',
    {
      feature: 'typescript/object-storage',
      requirement: 'key-safety',
      check: 'every-key-taking-client-method-rejects-traversal',
    },
    async () => {
      const { client } = createClient('key-traversal');
      await expect(client.put('../escape.txt', new Blob(['x']))).rejects.toThrow(/traversal/i);
      await expect(client.get('a/../../b')).rejects.toThrow(/traversal/i);
      await expect(client.delete('../x')).rejects.toThrow(/traversal/i);
      await expect(client.exists('../x')).rejects.toThrow(/traversal/i);
      expect(() => client.url('../x')).toThrow(/traversal/i);
    },
  );

  it('rejects empty, null-byte, and absolute keys', async () => {
    const { client } = createClient('key-bad');
    await expect(client.put('bad\0key', new Blob(['x']))).rejects.toThrow(/null bytes/i);
    await expect(client.get('')).rejects.toThrow(/empty/i);
    await expect(client.get('/absolute')).rejects.toThrow(/absolute/i);
  });

  specTest(
    'validates both source and destination of copy',
    { feature: 'typescript/object-storage', requirement: 'key-safety', check: 'both-ends-of-a-copy-are-validated' },
    async () => {
      const { client } = createClient('key-copy');
      await client.put('ok.txt', new Blob(['x']));
      await expect(client.copy('ok.txt', '../escape.txt')).rejects.toThrow(/traversal/i);
      await expect(client.copy('../escape.txt', 'ok2.txt')).rejects.toThrow(/traversal/i);
    },
  );

  it('rejects invalid keys on signed URL methods', async () => {
    const { client } = createClient('key-signed');
    await expect(client.signedUploadUrl('../x')).rejects.toThrow(/traversal/i);
    await expect(client.signedDownloadUrl('../x')).rejects.toThrow(/traversal/i);
  });
});

describe('StorageValidationError', () => {
  it('should contain error messages', () => {
    const error = new StorageValidationError(['error 1', 'error 2']);
    expect(error.errors).toEqual(['error 1', 'error 2']);
    expect(error.message).toContain('error 1');
    expect(error.message).toContain('error 2');
    expect(error.name).toBe('StorageValidationError');
  });
});
