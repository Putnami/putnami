import { describe, expect, it } from 'bun:test';
import { BadRequestException, NotFoundException } from '@putnami/runtime';
import { GrpcPlugin, grpc } from '../../src/grpc/grpc.plugin';
import { mapErrorToGrpcStatus } from '../../src/grpc/grpc-protocol';
import { decodeBadRequestDetail, findFrameworkErrorDetail } from '../../src/grpc/connect-protocol';
import { HttpResponse } from '../../src/http/http-response';

describe('GrpcPlugin', () => {
  describe('factory function', () => {
    it('should create a GrpcPlugin with default config', () => {
      const plugin = grpc();
      expect(plugin).toBeInstanceOf(GrpcPlugin);
    });

    it('should accept custom content types', () => {
      const plugin = grpc({ acceptContentTypes: ['application/json'] });
      expect(plugin).toBeInstanceOf(GrpcPlugin);
    });

    it('should accept empty config', () => {
      const plugin = grpc({});
      expect(plugin).toBeInstanceOf(GrpcPlugin);
    });
  });

  describe('lifecycle', () => {
    it('should have warmup method', () => {
      const plugin = grpc();
      expect(typeof plugin.warmup).toBe('function');
    });
  });

  describe('compression config', () => {
    it('should enable compression by default', () => {
      const plugin = grpc();
      // Default config has compression: true
      expect(plugin).toBeInstanceOf(GrpcPlugin);
    });

    it('should accept compression: false', () => {
      const plugin = grpc({ compression: false });
      expect(plugin).toBeInstanceOf(GrpcPlugin);
    });

    it('should accept compression: true', () => {
      const plugin = grpc({ compression: true });
      expect(plugin).toBeInstanceOf(GrpcPlugin);
    });
  });
});

describe('gRPC compression (gzip)', () => {
  /** Helper: compress data using CompressionStream (mirrors grpc.plugin.ts internals). */
  async function gzipCompress(data: Uint8Array): Promise<Uint8Array<ArrayBuffer>> {
    const cs = new CompressionStream('gzip');
    const writer = cs.writable.getWriter();
    writer.write(data as unknown as BufferSource);
    writer.close();
    return collectStream(cs.readable);
  }

  /** Helper: decompress gzipped data using DecompressionStream. */
  async function gzipDecompress(data: Uint8Array): Promise<Uint8Array<ArrayBuffer>> {
    const ds = new DecompressionStream('gzip');
    const writer = ds.writable.getWriter();
    writer.write(data as unknown as BufferSource);
    writer.close();
    return collectStream(ds.readable);
  }

  async function collectStream(readable: ReadableStream): Promise<Uint8Array<ArrayBuffer>> {
    const reader = readable.getReader();
    const chunks: Uint8Array<ArrayBuffer>[] = [];
    let totalLength = 0;
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      const chunk = new Uint8Array(value as ArrayBuffer);
      chunks.push(chunk);
      totalLength += chunk.byteLength;
    }
    if (chunks.length === 1) return chunks[0];
    const result = new Uint8Array(totalLength);
    let offset = 0;
    for (const chunk of chunks) {
      result.set(chunk, offset);
      offset += chunk.byteLength;
    }
    return result;
  }

  it('should round-trip compress/decompress JSON payload', async () => {
    const original = JSON.stringify({
      users: [
        { id: 1, name: 'Alice' },
        { id: 2, name: 'Bob' },
      ],
    });
    const data = new TextEncoder().encode(original);

    const compressed = await gzipCompress(data);
    expect(compressed.byteLength).toBeGreaterThan(0);
    // gzip adds overhead for small payloads, but should not be identical bytes
    expect(compressed).not.toEqual(data);

    const decompressed = await gzipDecompress(compressed);
    expect(new TextDecoder().decode(decompressed)).toBe(original);
  });

  it('should round-trip compress/decompress binary protobuf-like payload', async () => {
    // Simulate a proto binary payload (field 1 = string "hello", field 2 = varint 42)
    const data = new Uint8Array([0x0a, 0x05, 0x68, 0x65, 0x6c, 0x6c, 0x6f, 0x10, 0x2a]);

    const compressed = await gzipCompress(data);
    const decompressed = await gzipDecompress(compressed);
    expect(decompressed).toEqual(data);
  });

  it('should handle empty payload', async () => {
    const data = new Uint8Array(0);
    const compressed = await gzipCompress(data);
    const decompressed = await gzipDecompress(compressed);
    expect(decompressed.byteLength).toBe(0);
  });

  it('should handle large payload with good compression ratio', async () => {
    // Highly compressible data (repeated JSON)
    const item = JSON.stringify({ id: 1, name: 'User Name', email: 'user@example.com', active: true });
    const largePayload = `[${Array(100).fill(item).join(',')}]`;
    const data = new TextEncoder().encode(largePayload);

    const compressed = await gzipCompress(data);
    // Repeated data should compress well
    expect(compressed.byteLength).toBeLessThan(data.byteLength);

    const decompressed = await gzipDecompress(compressed);
    expect(new TextDecoder().decode(decompressed)).toBe(largePayload);
  });

  it('should produce valid gzip magic bytes', async () => {
    const data = new TextEncoder().encode('test data for gzip');
    const compressed = await gzipCompress(data);
    // gzip magic bytes: 0x1f 0x8b
    expect(compressed[0]).toBe(0x1f);
    expect(compressed[1]).toBe(0x8b);
  });
});

describe('HttpResponse.rawData (gRPC fast path)', () => {
  it('should preserve raw data from HttpResponse.json()', () => {
    const data = { id: '123', name: 'Alice', active: true };
    const rh = HttpResponse.json(data);
    expect(rh.rawData()).toBe(data);
  });

  it('should return undefined for non-json HttpResponse', () => {
    const rh = new HttpResponse('plain text');
    expect(rh.rawData()).toBeUndefined();
  });

  it('should preserve raw data through setHeader()', () => {
    const data = { count: 42 };
    const rh = HttpResponse.json(data).setHeader('X-Custom', 'value');
    expect(rh.rawData()).toBe(data);
  });

  it('should preserve raw data through pushHeader()', () => {
    const data = { items: [1, 2, 3] };
    const rh = HttpResponse.json(data).pushHeader('X-Extra', 'value');
    expect(rh.rawData()).toBe(data);
  });

  it('should preserve raw data through withStatus()', () => {
    const data = { ok: true };
    const rh = HttpResponse.json(data).withStatus(201);
    expect(rh.rawData()).toBe(data);
  });

  it('should preserve raw data through copy()', () => {
    const data = { nested: { a: 1 } };
    const rh = HttpResponse.json(data).copy();
    expect(rh.rawData()).toBe(data);
  });

  it('should NOT preserve raw data through withBody()', () => {
    const data = { original: true };
    const rh = HttpResponse.json(data).withBody('new body');
    expect(rh.rawData()).toBeUndefined();
  });

  it('should preserve raw data through chained operations', () => {
    const data = { id: 'abc', tags: ['a', 'b'] };
    const rh = HttpResponse.json(data).setHeader('X-A', '1').pushHeader('X-B', '2').withStatus(200);
    expect(rh.rawData()).toBe(data);
  });
});

describe('HttpResponse.getBodyInit (direct body access)', () => {
  it('should return string body from HttpResponse.json()', () => {
    const rh = HttpResponse.json({ x: 1 });
    const body = rh.getBodyInit();
    expect(typeof body).toBe('string');
    expect(JSON.parse(body as string)).toEqual({ x: 1 });
  });

  it('should return ArrayBuffer body', () => {
    const buf = new ArrayBuffer(4);
    const rh = new HttpResponse(buf);
    expect(rh.getBodyInit()).toBe(buf);
  });

  it('should return undefined for no body', () => {
    const rh = new HttpResponse();
    expect(rh.getBodyInit()).toBeUndefined();
  });
});

describe('Connect error details (mapErrorToGrpcStatus)', () => {
  it('should map BadRequestException with validation errors to BadRequest detail', () => {
    const err = new BadRequestException({
      statusCode: 400,
      message: 'body.name is required; body.email must be a valid email address',
      error: 'Bad Request',
      errors: [
        { field: 'body.name', message: 'body.name is required' },
        { field: 'body.email', message: 'body.email must be a valid email address' },
      ],
    });

    const result = mapErrorToGrpcStatus(err);
    expect(result.grpcCode).toBe(3);
    expect(result.code).toBe('invalid_argument');
    expect(result.httpStatus).toBe(400);
    expect(result.details).toBeDefined();
    expect(result.details?.length).toBe(2); // google.rpc.BadRequest + the framework envelope

    const badRequest = result.details?.find((d) => d.type === 'google.rpc.BadRequest');
    expect(badRequest).toBeDefined();
    // The bytes are what a conforming client reads; `debug` is a readability
    // affordance no client may depend on.
    expect(decodeBadRequestDetail(badRequest!)).toEqual([
      { field: 'body.name', description: 'body.name is required' },
      { field: 'body.email', description: 'body.email must be a valid email address' },
    ]);
  });

  it('carries the stable framework code in the first-party detail', () => {
    const err = new NotFoundException('User not found');
    const result = mapErrorToGrpcStatus(err);
    expect(result.grpcCode).toBe(5);
    expect(result.code).toBe('not_found');

    // The endpoint here declares nothing, so the envelope carries the generic
    // remote code — the declared case is proved in grpc-protocol.test.ts.
    const framework = result.details?.find((d) => d.type === 'putnami.client.v1.FrameworkError');
    expect(framework).toBeDefined();
    expect(findFrameworkErrorDetail({ code: 'not_found', details: [framework!] })).toEqual({
      code: 'client.remote',
      status: 404,
    });
  });

  it('names a plain Error internal and attaches no violation detail', () => {
    const err = new Error('something broke');
    const result = mapErrorToGrpcStatus(err);
    expect(result.grpcCode).toBe(13);
    expect(result.code).toBe('internal');
    expect(result.details).toBeUndefined();
  });

  it('attaches no violation detail to an HttpException with a string response', () => {
    const err = new NotFoundException('not here');
    const result = mapErrorToGrpcStatus(err);
    expect(result.details?.find((d) => d.type === 'google.rpc.BadRequest')).toBeUndefined();
  });

  it('should handle BadRequestException with empty errors array', () => {
    const err = new BadRequestException({
      statusCode: 400,
      message: 'Bad request',
      error: 'Bad Request',
      errors: [],
    });
    const result = mapErrorToGrpcStatus(err);
    // No BadRequest detail (empty errors), but has ErrorInfo
    const badRequest = result.details?.find((d) => d.type === 'google.rpc.BadRequest');
    expect(badRequest).toBeUndefined();
  });
});
