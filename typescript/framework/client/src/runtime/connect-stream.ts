import { ENVELOPE_FLAG_END_STREAM, ENVELOPE_HEADER_BYTES, readEnvelope } from '@putnami/application';
import { ClientResponseContractError, ClientResponseSizeError } from './errors';
import type { ResponseCapContext } from './response-cap';
import type { StreamObserver } from './stream.type';

/**
 * Connect envelope framing and gzip codec for the client side of the Connect
 * protocol.
 *
 * An `Enveloped-Message` is one flag byte, a 4-byte big-endian length, then the
 * payload. Bit 0 marks a compressed message, bit 1 marks the
 * `EndStreamResponse`, and the six most significant bits are reserved. Both
 * directions use the same framing: a Connect request stream is enveloped too,
 * which is what distinguishes it from a unary call carrying a bare body.
 *
 * The flag values and the header size come from `@putnami/application`'s
 * `connect-protocol`, the module the provider reads them from as well.
 */

export { ENVELOPE_FLAG_COMPRESSED, ENVELOPE_FLAG_END_STREAM } from '@putnami/application';

/** Frame a payload as an `Enveloped-Message`. */
export function writeEnvelope(flags: number, payload: Uint8Array): Uint8Array {
  const frame = new Uint8Array(ENVELOPE_HEADER_BYTES + payload.length);
  frame[0] = flags;
  new DataView(frame.buffer).setUint32(1, payload.length, false);
  frame.set(payload, ENVELOPE_HEADER_BYTES);
  return frame;
}

/** A single decoded envelope frame. */
export interface ConnectFrame {
  /** Raw envelope flags byte. */
  flags: number;
  /** Frame payload, already gunzipped when the compressed flag was set. */
  payload: Uint8Array;
  /** True when this is the end-of-stream (trailers) frame. */
  endStream: boolean;
}

/**
 * Incremental Connect envelope frame decoder.
 *
 * `push()` accepts arbitrary fetch chunks — frames may straddle chunk
 * boundaries, and a single chunk may hold several frames — and yields whole
 * frames only. A frame whose declared length exceeds `maxFrameBytes` fails with
 * {@link ClientResponseSizeError} before any of it is buffered, so a hostile
 * length prefix cannot force an unbounded allocation.
 */
export class ConnectFrameDecoder {
  private buffer = new Uint8Array(0);
  private readonly maxFrameBytes: number;
  private readonly context?: ResponseCapContext;

  constructor(maxFrameBytes: number, context?: ResponseCapContext) {
    this.maxFrameBytes = maxFrameBytes;
    this.context = context;
  }

  /** Append a chunk and return every frame that is now complete. */
  push(chunk: Uint8Array): ConnectFrame[] {
    if (chunk.length > 0) {
      const merged = new Uint8Array(this.buffer.length + chunk.length);
      merged.set(this.buffer, 0);
      merged.set(chunk, this.buffer.length);
      this.buffer = merged;
    }

    const frames: ConnectFrame[] = [];
    let offset = 0;
    while (offset < this.buffer.length) {
      // Reject an oversized frame from its length prefix alone, before the body
      // is buffered — the cap must bound memory, not merely report afterwards.
      if (this.buffer.length - offset >= ENVELOPE_HEADER_BYTES) {
        const declared = new DataView(this.buffer.buffer, this.buffer.byteOffset + offset + 1, 4).getUint32(0, false);
        if (declared > this.maxFrameBytes) {
          throw new ClientResponseSizeError({ ...this.context, maxResponseSize: this.maxFrameBytes });
        }
      }
      const envelope = readEnvelope(this.buffer, offset);
      if (!envelope) break;
      frames.push({
        flags: envelope.flags,
        // Copy out of the accumulator: the payload is a subarray view and the
        // accumulator is replaced (and reused) on the next push.
        payload: this.buffer.slice(offset + ENVELOPE_HEADER_BYTES, offset + envelope.consumed),
        endStream: (envelope.flags & ENVELOPE_FLAG_END_STREAM) !== 0,
      });
      offset += envelope.consumed;
    }

    this.buffer = offset === 0 ? this.buffer : this.buffer.slice(offset);
    return frames;
  }
}

/** True when the bytes carry a gzip magic header (`1f 8b`). */
export function isGzipped(bytes: Uint8Array): boolean {
  return bytes.length >= 2 && bytes[0] === 0x1f && bytes[1] === 0x8b;
}

/**
 * Gunzip bytes, bounding the decompressed output.
 *
 * A small gzip payload can inflate to an arbitrarily large buffer, so the
 * output is counted as it is produced and the read is abandoned with
 * {@link ClientResponseSizeError} the moment it passes the cap. Mirrors the
 * server's `decompressGzip` bound.
 */
export async function gunzip(
  bytes: Uint8Array,
  maxOutputBytes: number,
  context?: ResponseCapContext,
): Promise<Uint8Array> {
  const stream = new DecompressionStream('gzip');
  const writer = stream.writable.getWriter();
  // Errors surface on the readable side; a rejected write must not become an
  // unhandled rejection.
  const write = writer
    .write(bytes as unknown as BufferSource)
    .then(() => writer.close())
    .catch(() => {});

  const reader = stream.readable.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    while (true) {
      // biome-ignore lint/performance/noAwaitInLoops: stream chunks must be consumed sequentially
      const { done, value } = await reader.read();
      if (done) break;
      if (!value || value.length === 0) continue;
      total += value.length;
      if (total > maxOutputBytes) {
        throw new ClientResponseSizeError({ ...context, maxResponseSize: maxOutputBytes });
      }
      chunks.push(value);
    }
  } finally {
    await reader.cancel().catch(() => {});
    await write;
  }

  const out = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.length;
  }
  return out;
}

/** Gzip bytes with the platform `CompressionStream` (used for request compression). */
export async function gzip(bytes: Uint8Array): Promise<Uint8Array> {
  const stream = new CompressionStream('gzip');
  const writer = stream.writable.getWriter();
  const writing = writer.write(bytes as unknown as BufferSource).then(() => writer.close());
  const [buffer] = await Promise.all([new Response(stream.readable).arrayBuffer(), writing]);
  return new Uint8Array(buffer);
}

/**
 * Decode a response body according to the encoding the server advertised.
 *
 * Three headers can carry it: `grpc-encoding` (gRPC message encoding),
 * `connect-content-encoding` (Connect's per-message encoding for streams), and
 * plain `content-encoding`. The last one is normally handled by the fetch
 * implementation itself, so the bytes are only gunzipped when they still carry
 * the gzip magic — decoding twice would corrupt an already-decoded body.
 */
export async function decodeContentEncoding(
  bytes: Uint8Array,
  headers: Headers,
  maxOutputBytes: number,
  context?: ResponseCapContext,
): Promise<Uint8Array> {
  const encoding =
    headers.get('grpc-encoding') ?? headers.get('connect-content-encoding') ?? headers.get('content-encoding');
  if (!encoding || !advertisesGzip(encoding)) return bytes;
  if (!isGzipped(bytes)) return bytes;
  return gunzip(bytes, maxOutputBytes, context);
}

/** True when a comma-separated encoding list names gzip. */
export function advertisesGzip(header: string): boolean {
  return header
    .toLowerCase()
    .split(',')
    .some((entry) => entry.split(';')[0].trim() === 'gzip');
}

/** Mutable handler set shared by a stream and any transport it falls back to. */
export interface StreamHandlers<T> {
  message?: (data: T) => void;
  error?: (error: Error) => void;
  complete?: () => void;
}

/**
 * A {@link StreamObserver} whose delivery source can be swapped after the
 * caller has subscribed.
 *
 * Transport selection for a server-streaming RPC is decided by the server: the
 * client attempts Connect and only learns it is unsupported once the response
 * (gRPC status 12 `UNIMPLEMENTED`) arrives — long after `onMessage`/`onError`
 * were registered. The relay owns the handlers so the fallback transport can be
 * adopted without the caller re-subscribing, and remembers a `cancel()` that
 * arrived before a source was attached so an early cancel is never lost.
 */
export class StreamRelay<T> implements StreamObserver<T> {
  readonly handlers: StreamHandlers<T> = {};
  private cancelSource: (() => void) | undefined;
  private cancelled = false;
  private readonly pending: T[] = [];
  private readonly maxBufferedMessages: number;
  private terminal: { error?: Error; complete?: true } | undefined;
  private readonly subscribeListeners: (() => void)[] = [];

  constructor(maxBufferedMessages = 64) {
    this.maxBufferedMessages = Math.max(1, maxBufferedMessages);
  }

  onMessage(handler: (data: T) => void): void {
    this.handlers.message = handler;
    for (const value of this.pending.splice(0)) handler(value);
    for (const listener of [...this.subscribeListeners]) listener();
  }

  /**
   * True once the caller registered a message handler: from then on a
   * delivered value reaches them at once instead of waiting in the retained
   * queue.
   */
  get hasSubscriber(): boolean {
    return this.handlers.message !== undefined;
  }

  /**
   * Run `listener` each time the caller registers a message handler, after the
   * retained values were handed to it — and at once when one is already
   * registered. A source that hands values over only while the caller can
   * receive them (a declared SSE continuation, whose position is the last
   * value the caller took) learns here when that is.
   */
  onSubscribe(listener: () => void): void {
    this.subscribeListeners.push(listener);
    if (this.hasSubscriber) listener();
  }

  onError(handler: (error: Error) => void): void {
    this.handlers.error = handler;
    if (this.terminal?.error) handler(this.terminal.error);
  }

  onComplete(handler: () => void): void {
    this.handlers.complete = handler;
    if (this.terminal?.complete) handler();
  }

  cancel(): void {
    this.cancelled = true;
    this.cancelSource?.();
  }

  /** True once {@link cancel} has been called. */
  get isCancelled(): boolean {
    return this.cancelled;
  }

  /**
   * Attach the current source's cancel function. Applies a cancel that already
   * happened, so a source that starts after `cancel()` is torn down at once.
   */
  attach(cancel: () => void): void {
    this.cancelSource = cancel;
    if (this.cancelled) cancel();
  }

  /** Adopt another observer (a fallback transport) as the delivery source. */
  adopt(source: StreamObserver<T>): void {
    source.onMessage((data) => this.deliver(data));
    source.onError((error) => this.fail(error));
    source.onComplete(() => this.complete());
    this.attach(() => source.cancel());
  }

  /** Deliver a message or retain it until the caller subscribes. */
  deliver(data: T): void {
    if (this.terminal || this.cancelled) return;
    if (this.handlers.message) {
      this.handlers.message(data);
      return;
    }
    if (this.pending.length >= this.maxBufferedMessages) {
      this.fail(new ClientResponseContractError('service stream receive queue exceeds the declared maximum'));
      this.cancelSource?.();
      return;
    }
    this.pending.push(data);
  }

  /** Deliver a terminal error to the caller. */
  fail(error: Error): void {
    if (this.terminal || this.cancelled) return;
    this.terminal = { error };
    this.handlers.error?.(error);
  }

  /** Retain successful completion when the caller subscribes after the terminal frame. */
  complete(): void {
    if (this.terminal || this.cancelled) return;
    this.terminal = { complete: true };
    this.handlers.complete?.();
  }
}
