import { afterEach, describe, expect, it } from 'bun:test';
import { MemoryBackend } from '../src/backend/memory.backend';
import { StorageError } from '../src/errors';

describe('MemoryBackend key validation', () => {
  // MemoryBackend touches no filesystem and builds no URL, so unsafe keys are
  // harmless *here* — and that is exactly the problem: a key it accepted and
  // the file/S3/remote backends rejected made a test pass against a
  // substitution that fails in production. The shared contract now asserts
  // traversal rejection for this backend too; these cover the rest of the rule.
  it('rejects empty, null-byte, absolute, and traversal keys on every key-taking method', async () => {
    const backend = new MemoryBackend();

    await expect(backend.put('b', '../escape.txt', new Blob(['x']))).rejects.toThrow(StorageError);
    await expect(backend.get('b', 'a/../../b')).rejects.toThrow(StorageError);
    await expect(backend.delete('b', '../x')).rejects.toThrow(StorageError);
    await expect(backend.exists('b', '../x')).rejects.toThrow(StorageError);
    await expect(backend.copy('b', '../src.txt', 'dst.txt')).rejects.toThrow(StorageError);
    await expect(backend.copy('b', 'src.txt', '../dst.txt')).rejects.toThrow(StorageError);
    await expect(backend.signedUploadUrl('b', '../x')).rejects.toThrow(StorageError);
    await expect(backend.signedDownloadUrl('b', '../x')).rejects.toThrow(StorageError);
    expect(() => backend.url('b', '../x')).toThrow(StorageError);

    await expect(backend.put('b', '', new Blob(['x']))).rejects.toThrow(/empty/i);
    await expect(backend.put('b', 'bad\0key', new Blob(['x']))).rejects.toThrow(/null bytes/i);
    await expect(backend.get('b', '/absolute')).rejects.toThrow(/absolute/i);
  });

  it('still accepts ordinary nested keys and single dots', async () => {
    const backend = new MemoryBackend();
    await backend.put('b', 'a/b/c.txt', new Blob(['ok']));
    await backend.put('b', './relative.txt', new Blob(['ok']));
    expect(await backend.exists('b', 'a/b/c.txt')).toBe(true);
    expect(await backend.exists('b', './relative.txt')).toBe(true);
  });
});

describe('MemoryBackend', () => {
  const backend = new MemoryBackend();

  afterEach(() => {
    backend.clear();
  });

  describe('put / get', () => {
    it('should store and retrieve a Blob', async () => {
      const data = new Blob(['hello world'], { type: 'text/plain' });
      const result = await backend.put('test', 'greeting.txt', data, { contentType: 'text/plain' });

      expect(result.key).toBe('greeting.txt');
      expect(result.size).toBe(11);
      expect(result.etag).toBeDefined();

      const retrieved = await backend.get('test', 'greeting.txt');
      expect(retrieved).not.toBeNull();
      expect(retrieved?.key).toBe('greeting.txt');
      expect(retrieved?.size).toBe(11);
      expect(retrieved?.contentType).toBe('text/plain');

      const text = await new Response(retrieved?.body).text();
      expect(text).toBe('hello world');
    });

    it('should store and retrieve a Buffer', async () => {
      const data = Buffer.from('buffer content');
      await backend.put('test', 'buf.txt', data);

      const retrieved = await backend.get('test', 'buf.txt');
      expect(retrieved).not.toBeNull();
      const text = await new Response(retrieved?.body).text();
      expect(text).toBe('buffer content');
    });

    it('should store and retrieve a ReadableStream', async () => {
      const stream = new ReadableStream({
        start(controller) {
          controller.enqueue(new TextEncoder().encode('stream data'));
          controller.close();
        },
      });
      await backend.put('test', 'stream.txt', stream);

      const retrieved = await backend.get('test', 'stream.txt');
      expect(retrieved).not.toBeNull();
      const text = await new Response(retrieved?.body).text();
      expect(text).toBe('stream data');
    });

    it('should return null for nonexistent key', async () => {
      const result = await backend.get('test', 'missing.txt');
      expect(result).toBeNull();
    });

    it('should return null for nonexistent bucket', async () => {
      const result = await backend.get('no-bucket', 'file.txt');
      expect(result).toBeNull();
    });

    it('should store custom metadata', async () => {
      await backend.put('test', 'meta.txt', new Blob(['data']), {
        contentType: 'text/plain',
        custom: { author: 'alice' },
        cacheControl: 'public, max-age=3600',
      });

      const retrieved = await backend.get('test', 'meta.txt');
      expect(retrieved?.metadata).toEqual({ author: 'alice' });
    });

    it('should overwrite existing object', async () => {
      await backend.put('test', 'file.txt', new Blob(['v1']));
      await backend.put('test', 'file.txt', new Blob(['v2']));

      const retrieved = await backend.get('test', 'file.txt');
      const text = await new Response(retrieved?.body).text();
      expect(text).toBe('v2');
    });
  });

  describe('delete', () => {
    it('should delete an existing object', async () => {
      await backend.put('test', 'to-delete.txt', new Blob(['data']));
      expect(await backend.exists('test', 'to-delete.txt')).toBe(true);

      await backend.delete('test', 'to-delete.txt');
      expect(await backend.exists('test', 'to-delete.txt')).toBe(false);
    });

    it('should not throw when deleting nonexistent key', async () => {
      await expect(backend.delete('test', 'nonexistent.txt')).resolves.toBeUndefined();
    });
  });

  describe('exists', () => {
    it('should return true for existing objects', async () => {
      await backend.put('test', 'exists.txt', new Blob(['data']));
      expect(await backend.exists('test', 'exists.txt')).toBe(true);
    });

    it('should return false for nonexistent objects', async () => {
      expect(await backend.exists('test', 'missing.txt')).toBe(false);
    });

    it('should return false for nonexistent bucket', async () => {
      expect(await backend.exists('no-bucket', 'file.txt')).toBe(false);
    });
  });

  describe('copy', () => {
    it('should copy an object within the same bucket', async () => {
      await backend.put('test', 'original.txt', new Blob(['copy me']));
      await backend.copy('test', 'original.txt', 'copied.txt');

      const original = await backend.get('test', 'original.txt');
      const copied = await backend.get('test', 'copied.txt');
      expect(original).not.toBeNull();
      expect(copied).not.toBeNull();

      const origText = await new Response(original?.body).text();
      const copyText = await new Response(copied?.body).text();
      expect(origText).toBe('copy me');
      expect(copyText).toBe('copy me');
    });

    it('should throw a StorageError with code NOT_FOUND when source does not exist', async () => {
      // Callers must be able to branch on a uniform typed error across all
      // backends, so a missing copy source is a StorageError('NOT_FOUND'),
      // not a plain Error.
      const error = await backend.copy('test', 'missing.txt', 'dest.txt').then(
        () => null,
        (e) => e,
      );
      expect(error).toBeInstanceOf(StorageError);
      expect((error as StorageError).code).toBe('NOT_FOUND');
      expect((error as StorageError).message).toContain('Source object not found');
    });
  });

  describe('list', () => {
    it('should list all objects in a bucket', async () => {
      await backend.put('test', 'a.txt', new Blob(['a']));
      await backend.put('test', 'b.txt', new Blob(['b']));
      await backend.put('test', 'c.txt', new Blob(['c']));

      const result = await backend.list('test');
      expect(result.objects).toHaveLength(3);
      expect(result.objects.map((o) => o.key)).toEqual(['a.txt', 'b.txt', 'c.txt']);
      expect(result.isTruncated).toBe(false);
    });

    it('should filter by prefix', async () => {
      await backend.put('test', 'images/a.png', new Blob(['a']));
      await backend.put('test', 'images/b.png', new Blob(['b']));
      await backend.put('test', 'docs/c.pdf', new Blob(['c']));

      const result = await backend.list('test', { prefix: 'images/' });
      expect(result.objects).toHaveLength(2);
      expect(result.objects.map((o) => o.key)).toEqual(['images/a.png', 'images/b.png']);
    });

    it('should group by delimiter', async () => {
      await backend.put('test', 'a/1.txt', new Blob(['1']));
      await backend.put('test', 'a/2.txt', new Blob(['2']));
      await backend.put('test', 'b/3.txt', new Blob(['3']));
      await backend.put('test', 'root.txt', new Blob(['root']));

      const result = await backend.list('test', { delimiter: '/' });
      expect(result.objects).toHaveLength(1);
      expect(result.objects[0].key).toBe('root.txt');
      expect(result.prefixes).toEqual(['a/', 'b/']);
    });

    it('should respect maxKeys', async () => {
      await backend.put('test', 'a.txt', new Blob(['a']));
      await backend.put('test', 'b.txt', new Blob(['b']));
      await backend.put('test', 'c.txt', new Blob(['c']));

      const result = await backend.list('test', { maxKeys: 2 });
      expect(result.objects).toHaveLength(2);
      expect(result.isTruncated).toBe(true);
    });

    it('should return empty result for nonexistent bucket', async () => {
      const result = await backend.list('no-bucket');
      expect(result.objects).toEqual([]);
      expect(result.prefixes).toEqual([]);
    });
  });

  describe('url', () => {
    it('should return memory:// URL', () => {
      expect(backend.url('test', 'file.txt')).toBe('memory://test/file.txt');
    });
  });

  describe('signedUploadUrl / signedDownloadUrl', () => {
    it('should return a signed upload URL', async () => {
      const result = await backend.signedUploadUrl('test', 'file.txt');
      expect(result.url).toContain('memory://test/file.txt');
      expect(result.method).toBe('PUT');
      expect(result.expiresAt).toBeInstanceOf(Date);
      expect(result.expiresAt.getTime()).toBeGreaterThan(Date.now());
    });

    it('should return a signed download URL', async () => {
      const result = await backend.signedDownloadUrl('test', 'file.txt');
      expect(result.url).toContain('memory://test/file.txt');
      expect(result.method).toBe('GET');
      expect(result.expiresAt).toBeInstanceOf(Date);
    });
  });

  describe('test utilities', () => {
    it('should clear all data', async () => {
      await backend.put('a', 'file.txt', new Blob(['data']));
      await backend.put('b', 'file.txt', new Blob(['data']));
      expect(backend.bucketNames()).toHaveLength(2);

      backend.clear();
      expect(backend.bucketNames()).toHaveLength(0);
    });

    it('should report object count per bucket', async () => {
      await backend.put('test', 'a.txt', new Blob(['a']));
      await backend.put('test', 'b.txt', new Blob(['b']));
      expect(backend.objectCount('test')).toBe(2);
      expect(backend.objectCount('empty')).toBe(0);
    });

    it('should report bucket names', async () => {
      await backend.put('alpha', 'f.txt', new Blob(['a']));
      await backend.put('beta', 'f.txt', new Blob(['b']));
      expect(backend.bucketNames().sort()).toEqual(['alpha', 'beta']);
    });
  });

  describe('close', () => {
    it('should clear all data on close', async () => {
      await backend.put('test', 'file.txt', new Blob(['data']));
      await backend.close();
      expect(backend.objectCount('test')).toBe(0);
    });
  });
});
