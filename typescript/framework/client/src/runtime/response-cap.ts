import { ClientResponseSizeError } from './errors';

/**
 * Default maximum response body size (32 MiB) — mirrors the Go transports'
 * `defaultMaxResponseSize` (`32 << 20`, see `go/framework/client/http_transport.go`).
 *
 * The TS and Go clients are bounded-by-default: a response larger than this cap
 * fails the request rather than being buffered unbounded. Override per client via
 * `ClientConfig.maxResponseSize`; `0`/unset selects this default (Go parity).
 */
export const DEFAULT_MAX_RESPONSE_SIZE = 32 * 1024 * 1024;

/**
 * Resolve the effective response-size cap, mirroring Go's `MaxResponseSize`
 * handling where a non-positive value selects the 32 MiB default.
 */
export function resolveMaxResponseSize(maxResponseSize?: number): number {
  if (maxResponseSize === undefined || !Number.isFinite(maxResponseSize) || maxResponseSize <= 0) {
    return DEFAULT_MAX_RESPONSE_SIZE;
  }
  return Math.trunc(maxResponseSize);
}

/** Context threaded into {@link ClientResponseSizeError} when the cap trips. */
export interface ResponseCapContext {
  service?: string;
  method?: string;
}

/**
 * Read a fetch response body into memory, capped at `maxResponseSize` bytes.
 *
 * Mirrors the Go transports' `io.ReadAll(io.LimitReader(body, maxResponseSize+1))`
 * pattern: it reads at most one byte past the cap so an oversized body is
 *detected* (and rejected with {@link ClientResponseSizeError}) rather than
 * silently truncated. A body exactly at the cap is accepted. The single stream
 * read replaces `response.json()`/`text()`/`arrayBuffer()`, so there is no
 * double-read on the hot path.
 */
export async function readBodyBytesCapped(
  response: Response,
  maxResponseSize: number,
  context?: ResponseCapContext,
): Promise<Uint8Array> {
  const body = response.body;
  if (!body) {
    // Some environments/mocks expose no stream body; fall back to arrayBuffer,
    // still enforcing the cap on the materialized bytes.
    const buf = new Uint8Array(await response.arrayBuffer());
    if (buf.length > maxResponseSize) {
      throw new ClientResponseSizeError({ ...context, maxResponseSize });
    }
    return buf;
  }

  // Read one byte past the cap (Go parity) so `total > maxResponseSize` means
  // the body overflowed, not merely reached, the limit.
  const limit = maxResponseSize + 1;
  const reader = body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    while (total < limit) {
      // biome-ignore lint/performance/noAwaitInLoops: response chunks must be consumed sequentially under the byte cap
      const { done, value } = await reader.read();
      if (done) break;
      if (value && value.length > 0) {
        chunks.push(value);
        total += value.length;
      }
    }
  } finally {
    // Release the connection; we deliberately stop reading once over the cap.
    await reader.cancel().catch(() => {});
  }

  if (total > maxResponseSize) {
    throw new ClientResponseSizeError({ ...context, maxResponseSize });
  }

  const out = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.length;
  }
  return out;
}

/**
 * Read a fetch response body as UTF-8 text, capped at `maxResponseSize` bytes.
 * Convenience wrapper over {@link readBodyBytesCapped} for the JSON/text paths.
 */
export async function readBodyTextCapped(
  response: Response,
  maxResponseSize: number,
  context?: ResponseCapContext,
): Promise<string> {
  const bytes = await readBodyBytesCapped(response, maxResponseSize, context);
  return new TextDecoder().decode(bytes);
}
