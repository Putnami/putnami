import { HttpException } from '@putnami/runtime';
import {
  ENVELOPE_FLAG_COMPRESSED,
  ENVELOPE_FLAG_END_STREAM,
  ENVELOPE_HEADER_BYTES,
  ENVELOPE_RESERVED_FLAGS,
} from './connect-protocol';
import { MAX_GRPC_MESSAGE_BYTES, decompressGzip } from './grpc-protocol';

/**
 * How many event-loop turns the end of a request body waits for the request
 * signal to abort before it is read as the caller's end rather than its reset:
 * the two turns Bun has been observed to need, and one turn of margin.
 */
export const CANCELLATION_TURNS = 3;

/**
 * The request side of a Connect client or bidirectional stream, read as it
 * arrives.
 *
 * Each call to {@link next} pulls just enough of the body to return one
 * message, so a handler that is slow to read slows the caller down through
 * the transport's own flow control instead of buffering the stream in memory.
 * At most one message is held at a time. A message whose length prefix is
 * past the bound is refused from the prefix alone, before any of it is
 * buffered, and a message is assembled into one buffer of its declared length,
 * so reading it costs time linear in its size however small the chunks are.
 */
export class ConnectRequestStream {
  private readonly reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
  /** The unread rest of the last chunk the body delivered. */
  private chunk = new Uint8Array(0);
  private ended = false;

  constructor(
    body: ReadableStream<Uint8Array> | null,
    private readonly streamEncoding: string | null,
    private readonly maxMessageBytes: number = MAX_GRPC_MESSAGE_BYTES,
    private readonly signal?: AbortSignal,
  ) {
    this.reader = body?.getReader();
    if (!this.reader) this.ended = true;
  }

  /** The payload of the next message, decompressed, or `undefined` once the caller ended its side. */
  async next(): Promise<Uint8Array | undefined> {
    const header = new Uint8Array(ENVELOPE_HEADER_BYTES);
    const headerRead = await this.fill(header);
    if (headerRead === 0) return undefined;
    if (headerRead < ENVELOPE_HEADER_BYTES) throw truncated();
    const flags = header[0] ?? 0;
    const length = new DataView(header.buffer).getUint32(1, false);
    if (length > this.maxMessageBytes) {
      throw new HttpException(`Payload Too Large: gRPC message exceeds ${this.maxMessageBytes} bytes`, 413);
    }
    const payload = new Uint8Array(length);
    if ((await this.fill(payload)) < length) throw truncated();
    return this.decode(flags, payload);
  }

  /** Stop reading: the response ended before the caller ended its side. */
  cancel(): void {
    if (this.ended) return;
    this.ended = true;
    this.reader?.cancel().catch(() => {});
  }

  /**
   * Copy body bytes into `target` until it is full or the body ended, and
   * return how many were copied. Each body byte is copied once.
   */
  private async fill(target: Uint8Array): Promise<number> {
    let filled = 0;
    while (filled < target.length) {
      if (this.chunk.length === 0) {
        if (this.ended) return filled;
        // biome-ignore lint/performance/noAwaitInLoops: the body is read in order, one chunk at a time
        const read = await this.reader?.read();
        if (!read || read.done) {
          this.ended = true;
          await this.refuseCancellation();
          return filled;
        }
        // The runtime may reuse a chunk's memory once the next read starts, so
        // the unread rest is kept in a copy of its own.
        this.chunk = new Uint8Array(read.value);
        continue;
      }
      const take = Math.min(this.chunk.length, target.length - filled);
      target.set(this.chunk.subarray(0, take), filled);
      this.chunk = this.chunk.subarray(take);
      filled += take;
    }
    return filled;
  }

  /**
   * A caller that resets its stream ends the body exactly as a caller that
   * ended its side does, and Bun aborts the request signal in a later turn of
   * the event loop, observed no later than the turn after the one that ended
   * the body. Reading a cancellation as the end would let a client stream commit
   * the part it received as if it were whole, so the end is confirmed only
   * after {@link CANCELLATION_TURNS} turns, counted with `setImmediate`, gave
   * the signal its chance to abort. The wait counts turns, not time: a turn
   * slowed by load or by a burst of resets delays the abort and the
   * confirmation alike, where a wall-clock grace would expire first. The
   * guarantee rests on that observed Bun scheduling; the duplex stream tests
   * check it on every Bun the workspace pins.
   */
  private async refuseCancellation(): Promise<void> {
    const signal = this.signal;
    if (!signal) return;
    for (let turn = 0; turn < CANCELLATION_TURNS && !signal.aborted; turn++) {
      // biome-ignore lint/performance/noAwaitInLoops: each turn must follow the previous one
      await new Promise<void>((resolve) => setImmediate(resolve));
    }
    if (signal.aborted) {
      throw new HttpException('Client Closed Request: the caller canceled the stream', 499);
    }
  }

  private decode(flags: number, payload: Uint8Array): Uint8Array {
    if ((flags & ENVELOPE_FLAG_END_STREAM) !== 0) {
      throw new HttpException('Bad Request: a Connect request stream must not set the end-stream flag', 400);
    }
    if ((flags & ENVELOPE_RESERVED_FLAGS) !== 0) {
      throw new HttpException('Bad Request: reserved Connect envelope flags are set', 400);
    }
    if ((flags & ENVELOPE_FLAG_COMPRESSED) === 0) return payload;
    if (this.streamEncoding !== 'gzip') {
      throw new HttpException(
        'Bad Request: a compressed Connect envelope requires connect-content-encoding: gzip',
        400,
      );
    }
    return decompressGzip(payload, this.maxMessageBytes);
  }
}

function truncated(): HttpException {
  return new HttpException('Bad Request: truncated Connect stream request envelope', 400);
}
