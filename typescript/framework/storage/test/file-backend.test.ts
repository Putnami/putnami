import { afterAll, afterEach, beforeAll, describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { rm } from 'node:fs/promises';
import {
  DEFAULT_SIGNED_URL_EXPIRY,
  FileBackend,
  MAX_SIGNED_URL_DURATION_MS,
  parseDuration,
  resolveExpiry,
  safePath,
  validateKey,
  validateToken,
} from '../src/backend/file.backend';
import { win32 } from 'node:path';
import { StorageError } from '../src/errors';

const TEST_DIR = `/tmp/putnami-storage-test-${Date.now()}`;
const TOKEN_SECRET = 'test-secret-key-for-hmac';

describe('FileBackend', () => {
  let backend: FileBackend;

  beforeAll(() => {
    backend = new FileBackend(TEST_DIR, TOKEN_SECRET);
  });

  afterEach(async () => {
    // Clean up test files between tests
    await rm(TEST_DIR, { recursive: true, force: true });
  });

  afterAll(async () => {
    await rm(TEST_DIR, { recursive: true, force: true });
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

    it('should store metadata sidecar', async () => {
      await backend.put('test', 'with-meta.txt', new Blob(['data']), {
        contentType: 'text/plain',
        custom: { author: 'alice' },
        cacheControl: 'public, max-age=3600',
      });

      const retrieved = await backend.get('test', 'with-meta.txt');
      expect(retrieved?.contentType).toBe('text/plain');
      expect(retrieved?.metadata).toEqual({ author: 'alice' });
    });

    it('should handle nested key paths', async () => {
      await backend.put('test', 'a/b/c/deep.txt', new Blob(['deep']));

      const retrieved = await backend.get('test', 'a/b/c/deep.txt');
      expect(retrieved).not.toBeNull();
      const text = await new Response(retrieved?.body).text();
      expect(text).toBe('deep');
    });

    it('should overwrite existing object', async () => {
      await backend.put('test', 'overwrite.txt', new Blob(['v1']));
      await backend.put('test', 'overwrite.txt', new Blob(['v2']));

      const retrieved = await backend.get('test', 'overwrite.txt');
      const text = await new Response(retrieved?.body).text();
      expect(text).toBe('v2');
    });
  });

  describe('delete', () => {
    it('should delete an existing object and its metadata', async () => {
      await backend.put('test', 'to-delete.txt', new Blob(['data']), { contentType: 'text/plain' });
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
  });

  describe('copy', () => {
    it('should copy an object within the same bucket', async () => {
      await backend.put('test', 'original.txt', new Blob(['copy me']), { contentType: 'text/plain' });
      await backend.copy('test', 'original.txt', 'copied.txt');

      expect(await backend.exists('test', 'original.txt')).toBe(true);
      expect(await backend.exists('test', 'copied.txt')).toBe(true);

      const copied = await backend.get('test', 'copied.txt');
      const text = await new Response(copied?.body).text();
      expect(text).toBe('copy me');
      expect(copied?.contentType).toBe('text/plain');
    });

    it('should throw a StorageError with code NOT_FOUND when source does not exist', async () => {
      // Uniform typed error across all backends so callers can branch on a
      // missing copy source without knowing which backend is configured.
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

      const result = await backend.list('test');
      const keys = result.objects.map((o) => o.key);
      expect(keys).toContain('a.txt');
      expect(keys).toContain('b.txt');
    });

    it('should filter by prefix', async () => {
      await backend.put('test', 'images/a.png', new Blob(['a']));
      await backend.put('test', 'docs/b.pdf', new Blob(['b']));

      const result = await backend.list('test', { prefix: 'images/' });
      expect(result.objects).toHaveLength(1);
      expect(result.objects[0].key).toBe('images/a.png');
    });

    it('should group by delimiter', async () => {
      await backend.put('test', 'a/1.txt', new Blob(['1']));
      await backend.put('test', 'b/2.txt', new Blob(['2']));
      await backend.put('test', 'root.txt', new Blob(['root']));

      const result = await backend.list('test', { delimiter: '/' });
      expect(result.objects.map((o) => o.key)).toContain('root.txt');
      expect(result.prefixes.sort()).toEqual(['a/', 'b/']);
    });

    it('should return empty result for nonexistent bucket', async () => {
      const result = await backend.list('no-bucket');
      expect(result.objects).toEqual([]);
    });
  });

  describe('url', () => {
    it('should return the local filesystem path', () => {
      const url = backend.url('test', 'file.txt');
      expect(url).toContain('test');
      expect(url).toContain('file.txt');
    });
  });

  describe('signedUploadUrl / signedDownloadUrl', () => {
    it('should return a signed upload URL with HMAC token', async () => {
      const result = await backend.signedUploadUrl('test', 'file.txt', { expiresIn: '15m' });
      expect(result.url).toContain('/_storage/test/file.txt');
      expect(result.url).toContain('token=');
      expect(result.method).toBe('PUT');
      expect(result.expiresAt.getTime()).toBeGreaterThan(Date.now());

      // Token should contain a dot (payload.signature)
      const tokenMatch = result.url.match(/token=([^&]+)/);
      expect(tokenMatch).not.toBeNull();
      expect(tokenMatch?.[1]).toContain('.');
    });

    it('should return a signed download URL with HMAC token', async () => {
      const result = await backend.signedDownloadUrl('test', 'file.txt');
      expect(result.url).toContain('/_storage/test/file.txt');
      expect(result.method).toBe('GET');
    });

    it('should reject path traversal keys in signed URLs', async () => {
      await expect(backend.signedUploadUrl('test', '../etc/passwd')).rejects.toThrow();
      await expect(backend.signedDownloadUrl('test', '../etc/passwd')).rejects.toThrow();
    });

    specTest(
      'should default to a 15m expiry when expiresIn is omitted',
      {
        feature: 'typescript/object-storage',
        requirement: 'bounded-expiry',
        check: 'the-file-backend-defaults-to-a-15-minute-expiry',
      },
      async () => {
        const before = Date.now();
        const { expiresAt } = await backend.signedDownloadUrl('test', 'file.txt');
        const ttl = expiresAt.getTime() - before;
        // Allow a small scheduling delta around the 15m default.
        expect(ttl).toBeGreaterThan(14 * 60_000);
        expect(ttl).toBeLessThanOrEqual(15 * 60_000 + 1000);
      },
    );

    specTest(
      'should allow an expiry at the 7d maximum',
      {
        feature: 'typescript/object-storage',
        requirement: 'bounded-expiry',
        check: 'the-file-backend-allows-the-7-day-maximum',
      },
      async () => {
        await expect(backend.signedUploadUrl('test', 'file.txt', { expiresIn: '7d' })).resolves.toBeDefined();
        await expect(backend.signedDownloadUrl('test', 'file.txt', { expiresIn: '7d' })).resolves.toBeDefined();
      },
    );

    specTest(
      'should reject signed URLs whose expiry exceeds the 7d maximum',
      {
        feature: 'typescript/object-storage',
        requirement: 'bounded-expiry',
        check: 'the-file-backend-refuses-an-expiry-beyond-7-days',
      },
      async () => {
        await expect(backend.signedUploadUrl('test', 'file.txt', { expiresIn: '8d' })).rejects.toThrow(StorageError);
        await expect(backend.signedDownloadUrl('test', 'file.txt', { expiresIn: '3650d' })).rejects.toThrow(
          /exceeds the maximum of 7d/,
        );
      },
    );
  });

  describe('validateToken', () => {
    it('should validate a correctly signed token', async () => {
      const { url } = await backend.signedUploadUrl('test', 'file.txt', { expiresIn: '15m' });
      const token = url.match(/token=([^&]+)/)?.[1];
      expect(backend.validateToken('PUT', token, 'test', 'file.txt')).toBe(true);
    });

    it('should reject a token signed with a different secret', () => {
      // Forge a token with a wrong secret
      const forgedPayload = Buffer.from(`PUT:test:file.txt:${Date.now() + 60_000}`).toString('base64url');
      const forgedToken = `${forgedPayload}.forged-signature`;
      expect(backend.validateToken('PUT', forgedToken, 'test', 'file.txt')).toBe(false);
    });

    it('should reject a plain base64 token (no signature)', () => {
      const payload = `PUT:test:file.txt:${Date.now() + 60_000}`;
      const plainToken = Buffer.from(payload).toString('base64url');
      // No dot separator — should fail
      expect(backend.validateToken('PUT', plainToken, 'test', 'file.txt')).toBe(false);
    });

    specTest(
      'should reject expired tokens',
      {
        feature: 'typescript/object-storage',
        requirement: 'signed-url-binding',
        check: 'an-expired-token-is-rejected',
      },
      async () => {
        const originalDateNow = Date.now;
        const issuedAt = 1_700_000_000_000;
        Date.now = () => issuedAt;

        try {
          const { url } = await backend.signedUploadUrl('test', 'file.txt', { expiresIn: '1s' });
          const token = url.match(/token=([^&]+)/)?.[1];

          Date.now = () => issuedAt + 1100;
          expect(backend.validateToken('PUT', token, 'test', 'file.txt')).toBe(false);
        } finally {
          Date.now = originalDateNow;
        }
      },
    );

    it('should reject tokens for different bucket/key', async () => {
      const { url } = await backend.signedUploadUrl('test', 'file.txt', { expiresIn: '15m' });
      const token = url.match(/token=([^&]+)/)?.[1];

      expect(backend.validateToken('PUT', token, 'other-bucket', 'file.txt')).toBe(false);
      expect(backend.validateToken('PUT', token, 'test', 'other-file.txt')).toBe(false);
    });

    it('should reject tokens minted for a different HTTP method', async () => {
      // The method is part of the signed payload: a download (GET) token must
      // never authorize an upload (PUT), and vice-versa.
      const downloadUrl = await backend.signedDownloadUrl('test', 'file.txt', { expiresIn: '15m' });
      const downloadToken = downloadUrl.url.match(/token=([^&]+)/)?.[1];
      expect(backend.validateToken('GET', downloadToken, 'test', 'file.txt')).toBe(true);
      expect(backend.validateToken('PUT', downloadToken, 'test', 'file.txt')).toBe(false);

      const uploadUrl = await backend.signedUploadUrl('test', 'file.txt', { expiresIn: '15m' });
      const uploadToken = uploadUrl.url.match(/token=([^&]+)/)?.[1];
      expect(backend.validateToken('PUT', uploadToken, 'test', 'file.txt')).toBe(true);
      expect(backend.validateToken('GET', uploadToken, 'test', 'file.txt')).toBe(false);
    });

    it('should handle keys containing colons', async () => {
      const { url } = await backend.signedUploadUrl('test', 'path:with:colons.txt', { expiresIn: '15m' });
      const token = url.match(/token=([^&]+)/)?.[1];
      expect(backend.validateToken('PUT', token, 'test', 'path:with:colons.txt')).toBe(true);
    });
  });

  describe('concurrency', () => {
    // Atomic writes (temp file + rename) must guarantee that a reader never
    // observes a half-written object or a data/metadata pair from two different
    // writes interleaved on disk, even under parallel put/get/copy on one key.

    it('should never expose a torn object during concurrent puts to the same key', async () => {
      // Each writer puts a fixed-size payload of a single repeated character and
      // a matching content-type. A torn read would surface a body shorter than
      // the payload or a body/metadata pairing that does not match.
      const payloadSize = 64 * 1024;
      const writers = ['a', 'b', 'c', 'd'].map((ch) => {
        const body = ch.repeat(payloadSize);
        return () => backend.put('test', 'hot.bin', new Blob([body]), { contentType: `text/${ch}`, custom: { ch } });
      });

      // Interleave many concurrent puts with reads of the same key.
      const ops: Promise<unknown>[] = [];
      for (let i = 0; i < 40; i++) {
        ops.push(writers[i % writers.length]());
        ops.push(
          backend.get('test', 'hot.bin').then(async (res) => {
            if (!res) return; // first reads may land before any write commits
            const text = await new Response(res.body).text();
            // Body must be a complete payload of one repeated character.
            expect(text.length).toBe(payloadSize);
            const ch = text[0];
            expect(text).toBe(ch.repeat(payloadSize));
            // Size metadata must match the committed body exactly (no torn meta).
            expect(res.size).toBe(payloadSize);
          }),
        );
      }
      await Promise.all(ops);

      // Final state is internally consistent: body matches its own content-type
      // and the recorded size.
      const final = await backend.get('test', 'hot.bin');
      expect(final).not.toBeNull();
      const finalText = await new Response(final?.body).text();
      const finalCh = finalText[0];
      expect(finalText).toBe(finalCh.repeat(payloadSize));
      expect(final?.size).toBe(payloadSize);
      expect(final?.contentType).toBe(`text/${finalCh}`);
      expect(final?.metadata).toEqual({ ch: finalCh });
    });

    it('should wait for an in-flight write before reading the same key', async () => {
      await backend.put('test', 'locked.bin', new Blob(['old']), {
        contentType: 'text/old',
        custom: { version: 'old' },
      });

      let releaseWrite!: () => void;
      const writeGate = new Promise<void>((resolve) => {
        releaseWrite = resolve;
      });
      const stream = new ReadableStream({
        async start(controller) {
          controller.enqueue(new TextEncoder().encode('new'));
          await writeGate;
          controller.close();
        },
      });

      const putPromise = backend.put('test', 'locked.bin', stream, {
        contentType: 'text/new',
        custom: { version: 'new' },
      });
      const readPromise = backend.get('test', 'locked.bin');

      let readSettled = false;
      const trackRead = readPromise.then(() => {
        readSettled = true;
      });
      await new Promise((resolve) => setTimeout(resolve, 25));
      expect(readSettled).toBe(false);

      releaseWrite();
      await putPromise;

      const result = await readPromise;
      await trackRead;
      expect(result).not.toBeNull();
      expect(await new Response(result?.body).text()).toBe('new');
      expect(result?.contentType).toBe('text/new');
      expect(result?.metadata).toEqual({ version: 'new' });
    });

    it('should not leak in-flight temp files into list()', async () => {
      const body = 'x'.repeat(32 * 1024);
      const puts = Array.from({ length: 20 }, () => backend.put('test', 'listed.bin', new Blob([body])));
      // Hammer list() while writes are committing.
      const lists = Array.from({ length: 20 }, () => backend.list('test'));
      const [, ...listResults] = await Promise.all([Promise.all(puts), ...lists]);

      for (const result of listResults as Awaited<ReturnType<typeof backend.list>>[]) {
        for (const obj of result.objects) {
          expect(obj.key.endsWith('.tmp')).toBe(false);
          expect(obj.key.endsWith('.meta.json')).toBe(false);
        }
      }
    });

    it('should produce a consistent copy under concurrent copy and read', async () => {
      const payloadSize = 48 * 1024;
      await backend.put('test', 'src.bin', new Blob(['s'.repeat(payloadSize)]), {
        contentType: 'text/s',
        custom: { origin: 'src' },
      });

      const ops: Promise<unknown>[] = [];
      for (let i = 0; i < 30; i++) {
        ops.push(backend.copy('test', 'src.bin', 'dst.bin'));
        ops.push(
          backend.get('test', 'dst.bin').then(async (res) => {
            if (!res) return; // dst may not exist before the first copy commits
            const text = await new Response(res.body).text();
            expect(text.length).toBe(payloadSize);
            expect(text).toBe('s'.repeat(payloadSize));
            expect(res.size).toBe(payloadSize);
          }),
        );
      }
      await Promise.all(ops);

      const dst = await backend.get('test', 'dst.bin');
      expect(dst).not.toBeNull();
      const dstText = await new Response(dst?.body).text();
      expect(dstText).toBe('s'.repeat(payloadSize));
      expect(dst?.contentType).toBe('text/s');
      expect(dst?.metadata).toEqual({ origin: 'src' });
    });
  });

  describe('close', () => {
    it('should not throw on close', async () => {
      await expect(backend.close()).resolves.toBeUndefined();
    });
  });
});

describe('validateKey', () => {
  it('should accept valid keys', () => {
    expect(() => validateKey('file.txt')).not.toThrow();
    expect(() => validateKey('path/to/file.txt')).not.toThrow();
    expect(() => validateKey('a/b/c/d.png')).not.toThrow();
    expect(() => validateKey('user-123/photo.png')).not.toThrow();
  });

  it('should reject keys with .. path traversal', () => {
    expect(() => validateKey('../etc/passwd')).toThrow(StorageError);
    expect(() => validateKey('foo/../../etc/passwd')).toThrow(StorageError);
    expect(() => validateKey('foo/..')).toThrow(StorageError);
  });

  it('should reject absolute paths', () => {
    expect(() => validateKey('/etc/passwd')).toThrow(StorageError);
  });

  it('should reject empty keys', () => {
    expect(() => validateKey('')).toThrow(StorageError);
  });

  it('should reject keys with null bytes', () => {
    expect(() => validateKey('file\0.txt')).toThrow(StorageError);
  });

  it('should allow single dots in path segments', () => {
    expect(() => validateKey('.hidden')).not.toThrow();
    expect(() => validateKey('path/.hidden/file.txt')).not.toThrow();
  });
});

describe('parseDuration', () => {
  it('should parse seconds', () => {
    expect(parseDuration('30s')).toBe(30_000);
  });

  it('should parse minutes', () => {
    expect(parseDuration('15m')).toBe(15 * 60_000);
  });

  it('should parse hours', () => {
    expect(parseDuration('1h')).toBe(3_600_000);
  });

  it('should parse days', () => {
    expect(parseDuration('7d')).toBe(7 * 24 * 3_600_000);
  });

  it('should throw on invalid format', () => {
    expect(() => parseDuration('abc')).toThrow('Invalid duration format');
    expect(() => parseDuration('15')).toThrow('Invalid duration format');
    expect(() => parseDuration('15x')).toThrow('Invalid duration format');
  });
});

describe('resolveExpiry', () => {
  it('should default to 15m when omitted', () => {
    expect(resolveExpiry()).toBe(parseDuration(DEFAULT_SIGNED_URL_EXPIRY));
    expect(resolveExpiry()).toBe(15 * 60_000);
  });

  it('should return the parsed duration for values within the cap', () => {
    expect(resolveExpiry('30s')).toBe(30_000);
    expect(resolveExpiry('1h')).toBe(3_600_000);
    expect(resolveExpiry('7d')).toBe(MAX_SIGNED_URL_DURATION_MS);
  });

  it('should reject durations above the 7d maximum', () => {
    expect(() => resolveExpiry('8d')).toThrow(StorageError);
    expect(() => resolveExpiry('3650d')).toThrow(/exceeds the maximum of 7d/);
  });

  it('should propagate invalid duration formats', () => {
    expect(() => resolveExpiry('forever')).toThrow('Invalid duration format');
  });

  it('should set the EXPIRY_TOO_LONG error code', () => {
    try {
      resolveExpiry('30d');
      throw new Error('expected resolveExpiry to throw');
    } catch (e) {
      expect(e).toBeInstanceOf(StorageError);
      expect((e as StorageError).code).toBe('EXPIRY_TOO_LONG');
    }
  });
});

describe('validateToken (standalone)', () => {
  it('should validate a correct token', () => {
    const { createHmac } = require('node:crypto');
    const secret = 'my-secret';
    const expiresAt = Date.now() + 60_000;
    const payload = `GET:bucket:key:${expiresAt}`;
    const signature = createHmac('sha256', secret).update(payload).digest('base64url');
    const token = `${Buffer.from(payload).toString('base64url')}.${signature}`;

    expect(validateToken(secret, 'GET', token, 'bucket', 'key')).toBe(true);
  });

  it('should reject tampered payload', () => {
    const { createHmac } = require('node:crypto');
    const secret = 'my-secret';
    const expiresAt = Date.now() + 60_000;
    const payload = `GET:bucket:key:${expiresAt}`;
    const signature = createHmac('sha256', secret).update(payload).digest('base64url');

    // Tamper with the payload
    const tamperedPayload = `GET:bucket:evil-key:${expiresAt}`;
    const token = `${Buffer.from(tamperedPayload).toString('base64url')}.${signature}`;
    expect(validateToken(secret, 'GET', token, 'bucket', 'evil-key')).toBe(false);
  });

  it('should reject a token minted for another method', () => {
    const { createHmac } = require('node:crypto');
    const secret = 'my-secret';
    const expiresAt = Date.now() + 60_000;
    const payload = `GET:bucket:key:${expiresAt}`;
    const signature = createHmac('sha256', secret).update(payload).digest('base64url');
    const token = `${Buffer.from(payload).toString('base64url')}.${signature}`;

    expect(validateToken(secret, 'PUT', token, 'bucket', 'key')).toBe(false);
  });

  it('should normalize the method casing', () => {
    const { createHmac } = require('node:crypto');
    const secret = 'my-secret';
    const expiresAt = Date.now() + 60_000;
    const payload = `GET:bucket:key:${expiresAt}`;
    const signature = createHmac('sha256', secret).update(payload).digest('base64url');
    const token = `${Buffer.from(payload).toString('base64url')}.${signature}`;

    expect(validateToken(secret, 'get', token, 'bucket', 'key')).toBe(true);
  });

  it('should reject wrong secret', () => {
    const { createHmac } = require('node:crypto');
    const expiresAt = Date.now() + 60_000;
    const payload = `GET:bucket:key:${expiresAt}`;
    const signature = createHmac('sha256', 'wrong-secret').update(payload).digest('base64url');
    const token = `${Buffer.from(payload).toString('base64url')}.${signature}`;

    expect(validateToken('correct-secret', 'GET', token, 'bucket', 'key')).toBe(false);
  });

  it('should reject expired token', () => {
    const { createHmac } = require('node:crypto');
    const secret = 'my-secret';
    const expiresAt = Date.now() - 1000; // already expired
    const payload = `GET:bucket:key:${expiresAt}`;
    const signature = createHmac('sha256', secret).update(payload).digest('base64url');
    const token = `${Buffer.from(payload).toString('base64url')}.${signature}`;

    expect(validateToken(secret, 'GET', token, 'bucket', 'key')).toBe(false);
  });

  it('should reject token without signature separator', () => {
    expect(validateToken('secret', 'GET', 'noseparator', 'bucket', 'key')).toBe(false);
  });

  it('should reject empty string', () => {
    expect(validateToken('secret', 'GET', '', 'bucket', 'key')).toBe(false);
  });
});

describe('safePath with Windows paths', () => {
  const dataDir = 'C:\\data';

  it('accepts a key inside the bucket', () => {
    expect(safePath(dataDir, 'bucket', 'a/b.txt', win32)).toBe('C:\\data\\bucket\\a\\b.txt');
  });
});
