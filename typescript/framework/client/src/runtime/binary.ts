import { ClientRequestEncodingError } from './errors';

/**
 * Raw octet payloads.
 *
 * A binary operation is not a JSON operation with a different codec: there is
 * no document to validate, no schema to project, and no encoding step that
 * could lose an octet. What it does have is a declared bound on each side, and
 * this module is where the request-side bound is honored — before a socket
 * exists, and without draining an oversized source.
 */

/** One raw octet success payload, with the facts a caller would otherwise re-read from the wire. */
export interface BinaryPayload<TStatus extends number = number> {
  /** The declared success status the provider answered. */
  readonly status: TStatus;
  /** The media type the provider labeled the payload with. It matched the declared one, or the call failed. */
  readonly contentType: string;
  /** The payload, verbatim: no base64, no JSON, no re-encoding. */
  readonly body: Uint8Array;
}

/** Caller-owned raw HTTP response; consume or cancel the body to release it. */
export interface StreamedBinaryPayload<TStatus extends number = number> {
  readonly status: TStatus;
  readonly contentType: string;
  readonly body: ReadableStream<Uint8Array>;
}

/** Every source a generated method accepts for a raw octet request payload. */
export type BinarySource = Uint8Array | ArrayBuffer | ReadableStream<Uint8Array>;

/**
 * Read at most `maxBytes` octets from a generated request payload and refuse
 * anything longer.
 *
 * A stream is read one octet past the bound and stopped there, so an oversized
 * source is refused without being read to the end. An absent source carries no
 * payload, which is not the same as an empty one being invalid: an empty
 * declared binary body is a legitimate zero-length payload.
 */
export async function readBoundedBody(source: BinarySource | undefined, maxBytes: number): Promise<Uint8Array> {
  if (!Number.isSafeInteger(maxBytes) || maxBytes <= 0) {
    throw new ClientRequestEncodingError('generated binary request has no declared bound');
  }
  if (source === undefined) return new Uint8Array(0);
  if (source instanceof Uint8Array) return refuseOversized(source, maxBytes);
  if (source instanceof ArrayBuffer) return refuseOversized(new Uint8Array(source), maxBytes);

  const reader = source.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    while (total <= maxBytes) {
      // biome-ignore lint/performance/noAwaitInLoops: payload chunks arrive sequentially under the declared bound
      const { done, value } = await reader.read();
      if (done) break;
      if (value && value.byteLength > 0) {
        chunks.push(value);
        total += value.byteLength;
      }
    }
  } finally {
    // Stop pulling: the rest of an oversized source is never read.
    await reader.cancel().catch(() => {});
  }
  if (total > maxBytes) {
    throw new ClientRequestEncodingError('service request body exceeds the declared bound');
  }
  const body = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    body.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return body;
}

function refuseOversized(body: Uint8Array, maxBytes: number): Uint8Array {
  if (body.byteLength > maxBytes) {
    throw new ClientRequestEncodingError('service request body exceeds the declared bound');
  }
  return body;
}
