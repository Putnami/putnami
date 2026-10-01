import { describe, expect, test } from 'bun:test';
import { createEnvelope } from '@putnami/application';
import {
  advertisesGzip,
  ConnectFrameDecoder,
  decodeContentEncoding,
  ENVELOPE_FLAG_COMPRESSED,
  ENVELOPE_FLAG_END_STREAM,
  gunzip,
  gzip,
  isGzipped,
  StreamRelay,
} from '../../src/runtime/connect-stream';
import { ClientResponseSizeError } from '../../src/runtime/errors';
import type { StreamObserver } from '../../src/runtime/stream.type';

const encoder = new TextEncoder();
const decoder = new TextDecoder();

function frame(flags: number, text: string): Uint8Array {
  return createEnvelope(flags, encoder.encode(text));
}

function concat(...parts: Uint8Array[]): Uint8Array {
  const total = parts.reduce((sum, part) => sum + part.length, 0);
  const out = new Uint8Array(total);
  let offset = 0;
  for (const part of parts) {
    out.set(part, offset);
    offset += part.length;
  }
  return out;
}

describe('ConnectFrameDecoder', () => {
  test('decodes several frames from a single chunk', () => {
    const decoderUnderTest = new ConnectFrameDecoder(1024);
    const frames = decoderUnderTest.push(concat(frame(0x00, '{"a":1}'), frame(ENVELOPE_FLAG_END_STREAM, '{}')));

    expect(frames).toHaveLength(2);
    expect(decoder.decode(frames[0].payload)).toBe('{"a":1}');
    expect(frames[0].endStream).toBe(false);
    expect(frames[1].endStream).toBe(true);
  });

  test('reassembles a frame split across chunk boundaries', () => {
    const decoderUnderTest = new ConnectFrameDecoder(1024);
    const full = frame(0x00, '{"hello":"world"}');

    // Split mid-header and mid-payload: fetch chunks respect no framing.
    expect(decoderUnderTest.push(full.subarray(0, 3))).toHaveLength(0);
    expect(decoderUnderTest.push(full.subarray(3, 9))).toHaveLength(0);
    const frames = decoderUnderTest.push(full.subarray(9));

    expect(frames).toHaveLength(1);
    expect(decoder.decode(frames[0].payload)).toBe('{"hello":"world"}');
  });

  test('keeps a trailing partial frame buffered until it completes', () => {
    const decoderUnderTest = new ConnectFrameDecoder(1024);
    const first = frame(0x00, 'one');
    const second = frame(0x00, 'two');

    const frames = decoderUnderTest.push(concat(first, second.subarray(0, 6)));
    expect(frames).toHaveLength(1);
    expect(decoder.decode(frames[0].payload)).toBe('one');

    const rest = decoderUnderTest.push(second.subarray(6));
    expect(rest).toHaveLength(1);
    expect(decoder.decode(rest[0].payload)).toBe('two');
  });

  test('surfaces the compressed flag', () => {
    const decoderUnderTest = new ConnectFrameDecoder(1024);
    const frames = decoderUnderTest.push(frame(ENVELOPE_FLAG_COMPRESSED, 'gz'));
    expect(frames[0].flags & ENVELOPE_FLAG_COMPRESSED).toBe(ENVELOPE_FLAG_COMPRESSED);
  });

  test('rejects an oversized frame from its length prefix alone', () => {
    const decoderUnderTest = new ConnectFrameDecoder(8, { service: 'svc', method: '/Rpc' });
    // Header claims 1 MiB; only the 5-byte header is present, so the cap must
    // trip before any of the body is buffered.
    const header = createEnvelope(0x00, new Uint8Array(0));
    new DataView(header.buffer).setUint32(1, 1024 * 1024, false);

    expect(() => decoderUnderTest.push(header)).toThrow(ClientResponseSizeError);
  });

  test('accepts a frame exactly at the cap', () => {
    const decoderUnderTest = new ConnectFrameDecoder(4);
    const frames = decoderUnderTest.push(frame(0x00, 'abcd'));
    expect(frames).toHaveLength(1);
  });

  test('an empty chunk yields no frames', () => {
    const decoderUnderTest = new ConnectFrameDecoder(1024);
    expect(decoderUnderTest.push(new Uint8Array(0))).toHaveLength(0);
  });
});

describe('gzip helpers', () => {
  test('round-trips through gzip/gunzip', async () => {
    const original = encoder.encode(JSON.stringify({ users: Array.from({ length: 40 }, (_, i) => `user-${i}`) }));
    const compressed = await gzip(original);

    expect(isGzipped(compressed)).toBe(true);
    expect(decoder.decode(await gunzip(compressed, 1024 * 1024))).toBe(decoder.decode(original));
  });

  test('decompresses what the server produces with node:zlib gzip', async () => {
    // The server compresses with `gzipSync` (grpc-protocol.ts `compressGzip`).
    const { gzipSync } = await import('node:zlib');
    const payload = JSON.stringify({ id: 'u-1', name: 'Alice' });
    const serverBytes = new Uint8Array(gzipSync(Buffer.from(payload)));

    expect(decoder.decode(await gunzip(serverBytes, 1024 * 1024))).toBe(payload);
  });

  test('bounds the decompressed output (decompression bomb)', async () => {
    const bomb = await gzip(new Uint8Array(64 * 1024));
    await expect(gunzip(bomb, 1024, { service: 'svc', method: '/Rpc' })).rejects.toBeInstanceOf(
      ClientResponseSizeError,
    );
  });

  test('isGzipped only matches the gzip magic header', () => {
    expect(isGzipped(new Uint8Array([0x1f, 0x8b, 0x08]))).toBe(true);
    expect(isGzipped(new Uint8Array([0x7b, 0x7d]))).toBe(false);
    expect(isGzipped(new Uint8Array([0x1f]))).toBe(false);
  });

  test('advertisesGzip parses comma-separated encoding lists', () => {
    expect(advertisesGzip('gzip')).toBe(true);
    expect(advertisesGzip('gzip, identity')).toBe(true);
    expect(advertisesGzip('identity, gzip;q=0.9')).toBe(true);
    expect(advertisesGzip('br, deflate')).toBe(false);
  });
});

describe('decodeContentEncoding', () => {
  const payload = encoder.encode(JSON.stringify({ ok: true }));

  test('gunzips a body the fetch layer left encoded (grpc-encoding)', async () => {
    const compressed = await gzip(payload);
    const headers = new Headers({ 'grpc-encoding': 'gzip' });

    expect(decoder.decode(await decodeContentEncoding(compressed, headers, 1024 * 1024))).toBe(decoder.decode(payload));
  });

  test('gunzips a per-message stream encoding (connect-content-encoding)', async () => {
    const compressed = await gzip(payload);
    const headers = new Headers({ 'connect-content-encoding': 'gzip' });

    expect(decoder.decode(await decodeContentEncoding(compressed, headers, 1024 * 1024))).toBe(decoder.decode(payload));
  });

  test('leaves an already-decoded body alone even when content-encoding says gzip', async () => {
    // Bun/undici decompress `Content-Encoding: gzip` transparently but keep the
    // header — decoding twice would corrupt the body.
    const headers = new Headers({ 'content-encoding': 'gzip' });
    expect(await decodeContentEncoding(payload, headers, 1024 * 1024)).toBe(payload);
  });

  test('leaves a body alone when no gzip is advertised', async () => {
    expect(await decodeContentEncoding(payload, new Headers(), 1024 * 1024)).toBe(payload);
    expect(await decodeContentEncoding(payload, new Headers({ 'content-encoding': 'br' }), 1024)).toBe(payload);
  });
});

describe('StreamRelay', () => {
  test('delivers to handlers registered after construction', () => {
    const relay = new StreamRelay<{ n: number }>();
    const seen: number[] = [];
    let completed = false;

    relay.onMessage((data) => seen.push(data.n));
    relay.onComplete(() => {
      completed = true;
    });

    relay.handlers.message?.({ n: 1 });
    relay.handlers.complete?.();

    expect(seen).toEqual([1]);
    expect(completed).toBe(true);
  });

  test('cancel() reaches the attached source', () => {
    const relay = new StreamRelay<string>();
    let cancelled = false;
    relay.attach(() => {
      cancelled = true;
    });

    relay.cancel();
    expect(cancelled).toBe(true);
    expect(relay.isCancelled).toBe(true);
  });

  test('a source attached after cancel() is torn down immediately', () => {
    const relay = new StreamRelay<string>();
    relay.cancel();

    let cancelled = false;
    relay.attach(() => {
      cancelled = true;
    });

    expect(cancelled).toBe(true);
  });

  test('adopt() re-points already-registered handlers at a fallback source', () => {
    const relay = new StreamRelay<{ id: string }>();
    const seen: string[] = [];
    let failure: Error | undefined;
    let completed = false;
    let cancelled = false;

    relay.onMessage((data) => seen.push(data.id));
    relay.onError((error) => {
      failure = error;
    });
    relay.onComplete(() => {
      completed = true;
    });

    const source: StreamObserver<{ id: string }> & {
      emit: (data: { id: string }) => void;
      fail: (error: Error) => void;
      finish: () => void;
    } = (() => {
      let onMessage: ((data: { id: string }) => void) | undefined;
      let onError: ((error: Error) => void) | undefined;
      let onComplete: (() => void) | undefined;
      return {
        onMessage: (handler) => {
          onMessage = handler;
        },
        onError: (handler) => {
          onError = handler;
        },
        onComplete: (handler) => {
          onComplete = handler;
        },
        cancel: () => {
          cancelled = true;
        },
        emit: (data) => onMessage?.(data),
        fail: (error) => onError?.(error),
        finish: () => onComplete?.(),
      };
    })();

    relay.adopt(source);
    source.emit({ id: 'a' });
    source.fail(new Error('boom'));
    source.finish();
    relay.cancel();

    expect(seen).toEqual(['a']);
    expect(failure?.message).toBe('boom');
    expect(completed).toBe(false);
    expect(cancelled).toBe(true);
  });

  test('fail() is a no-op when no error handler was registered', () => {
    const relay = new StreamRelay<string>();
    expect(() => relay.fail(new Error('unobserved'))).not.toThrow();
  });
});
