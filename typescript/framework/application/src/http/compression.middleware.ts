import type { HttpMiddleware } from './http-middleware.type';
import { HttpResponse } from './http-response';
import type { HttpRequestContextInternal } from './http-context.type';

/** Configuration for the compression middleware. */
export interface CompressionOptions {
  /** Minimum response size in bytes before compressing. Default: `1024` */
  threshold?: number;
  /** Preferred encodings in priority order. Default: `['gzip', 'deflate']` */
  encodings?: ('gzip' | 'deflate')[];
}

type SupportedEncoding = 'gzip' | 'deflate';

const COMPRESSIBLE_TYPES = new Set([
  'text/plain',
  'text/html',
  'text/css',
  'text/javascript',
  'application/json',
  'application/javascript',
  'application/xml',
  'image/svg+xml',
]);

function isCompressibleType(contentType: string | undefined): boolean {
  if (!contentType) return false;
  const baseType = contentType.split(';')[0]?.trim();
  return COMPRESSIBLE_TYPES.has(baseType ?? '');
}

function negotiateEncoding(
  acceptEncoding: string | null,
  preferred: SupportedEncoding[],
): SupportedEncoding | undefined {
  if (!acceptEncoding) return undefined;
  const accepted = acceptEncoding.toLowerCase();
  for (const encoding of preferred) {
    if (accepted.includes(encoding)) return encoding;
  }
  return undefined;
}

async function compressBody(body: ArrayBuffer, encoding: SupportedEncoding): Promise<Uint8Array> {
  const input = new Uint8Array(body);
  const format = encoding === 'gzip' ? 'gzip' : 'deflate';
  const cs = new CompressionStream(format);
  const writer = cs.writable.getWriter();
  writer.write(input);
  writer.close();

  const reader = cs.readable.getReader();
  const chunks: Uint8Array[] = [];
  let totalLength = 0;
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    chunks.push(value);
    totalLength += value.byteLength;
  }

  const result = new Uint8Array(totalLength);
  let offset = 0;
  for (const chunk of chunks) {
    result.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return result;
}

/** Creates a middleware that compresses compressible responses using gzip or deflate based on the client's Accept-Encoding. */
export const CompressionMiddleware = (options: CompressionOptions = {}): HttpMiddleware => {
  const threshold = options.threshold ?? 1024;
  const encodings = options.encodings ?? ['gzip', 'deflate'];

  return async (ctx, next) => {
    const res = await next();
    if (!res) return res;
    // A raw HTTP stream owns its reader; compression's whole-body read would
    // turn an unbounded stream into an unbounded buffer.
    if ((ctx as HttpRequestContextInternal).__rawHttpStreamResponse) return res;

    const contentType = res.getHeader('Content-Type');
    if (!isCompressibleType(contentType)) return res;

    // Already encoded
    if (res.getHeader('Content-Encoding')) return res;

    const acceptEncoding = ctx.req.headers.get('Accept-Encoding');
    const encoding = negotiateEncoding(acceptEncoding, encodings);
    if (!encoding) return res;

    // Fast path: if Content-Length is known and below threshold, skip entirely
    const knownLength = res.getHeader('Content-Length');
    if (knownLength && Number.parseInt(knownLength, 10) < threshold) {
      return res;
    }

    // Consume the HttpResponse to read body bytes
    const raw = res.get();
    const bodyBytes = await raw.arrayBuffer();

    if (bodyBytes.byteLength < threshold) {
      // Below threshold — rebuild uncompressed HttpResponse from raw bytes
      const originalHeaders: [string, string][] = [];
      raw.headers.forEach((v, k) => {
        originalHeaders.push([k, v]);
      });
      // Uint8Array satisfies BufferSource at runtime; TS DOM lib types are overly narrow
      return new HttpResponse(new Uint8Array(bodyBytes) as unknown as BufferSource, {
        status: res.status,
        headers: originalHeaders,
      });
    }

    const compressed = await compressBody(bodyBytes, encoding);

    const headers: [string, string][] = [];
    raw.headers.forEach((value, name) => {
      const lower = name.toLowerCase();
      if (lower !== 'content-length' && lower !== 'content-encoding') {
        headers.push([name, value]);
      }
    });
    headers.push(['Content-Encoding', encoding]);
    headers.push(['Content-Length', String(compressed.byteLength)]);

    // Ensure Vary includes Accept-Encoding
    const varyIdx = headers.findIndex(([name]) => name.toLowerCase() === 'vary');
    if (varyIdx < 0) {
      headers.push(['Vary', 'Accept-Encoding']);
    } else {
      const current = headers[varyIdx][1];
      if (!current.toLowerCase().includes('accept-encoding')) {
        headers[varyIdx] = [headers[varyIdx][0], `${current}, Accept-Encoding`];
      }
    }

    // Compressed Uint8Array satisfies BufferSource at runtime; TS DOM lib types are overly narrow
    return new HttpResponse(compressed as unknown as BufferSource, { status: res.status, headers });
  };
};
