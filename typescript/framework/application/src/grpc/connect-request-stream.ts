import { HttpException } from '@putnami/runtime';
import {
  ENVELOPE_FLAG_COMPRESSED,
  ENVELOPE_FLAG_END_STREAM,
  ENVELOPE_HEADER_BYTES,
  ENVELOPE_RESERVED_FLAGS,
} from './connect-protocol';
import { MAX_GRPC_MESSAGE_BYTES, decompressGzip } from './grpc-protocol';

/**
 * The request side of a Connect client or bidirectional stream, read as it
 * arrives.
 *
 * Each call to {@link next} pulls just enough of the body to return one
 * message, so a handler that is slow to read slows the caller down through
 * the transport's own flow control instead of buffering the stream in memory.
 * A message whose length prefix is past the bound is refused from the prefix
 * alone, before any of it is buffered.
 */
export class ConnectRequestStream {
  private readonly reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
  private buffer = new Uint8Array(0);
  private ended = false;

  constructor(
    body: ReadableStream<Uint8Array> | null,
    private readonly streamEncoding: string | null,
    private readonly maxMessageBytes: number = MAX_GRPC_MESSAGE_BYTES,
  ) {
    this.reader = body?.getReader();
    if (!this.reader) this.ended = true;
  }

  /** The payload of the next message, decompressed, or `undefined` once the caller ended its side. */
  async next(): Promise<Uint8Array | undefined> {
    for (;;) {
      if (this.buffer.length >= ENVELOPE_HEADER_BYTES) {
        const flags = this.buffer[0] ?? 0;
        const length = new DataView(this.buffer.buffer, this.buffer.byteOffset + 1, 4).getUint32(0, false);
        if (length > this.maxMessageBytes) {
          throw new HttpException(`Payload Too Large: gRPC message exceeds ${this.maxMessageBytes} bytes`, 413);
        }
        const size = ENVELOPE_HEADER_BYTES + length;
        if (this.buffer.length >= size) {
          const payload = this.buffer.slice(ENVELOPE_HEADER_BYTES, size);
          this.buffer = this.buffer.subarray(size);
          return this.decode(flags, payload);
        }
      }
      if (this.ended) {
        if (this.buffer.length > 0) {
          throw new HttpException('Bad Request: truncated Connect stream request envelope', 400);
        }
        return undefined;
      }
      // biome-ignore lint/performance/noAwaitInLoops: one message is read at a time, in order
      const chunk = await this.reader?.read();
      if (!chunk || chunk.done) {
        this.ended = true;
        continue;
      }
      this.append(chunk.value);
    }
  }

  /** Stop reading: the response ended before the caller ended its side. */
  cancel(): void {
    if (this.ended) return;
    this.ended = true;
    this.reader?.cancel().catch(() => {});
  }

  private append(chunk: Uint8Array): void {
    if (chunk.length === 0) return;
    // The runtime may reuse a chunk's memory once the next read starts, so a
    // pending partial message is always kept in a copy of its own.
    const merged = new Uint8Array(this.buffer.length + chunk.length);
    merged.set(this.buffer, 0);
    merged.set(chunk, this.buffer.length);
    this.buffer = merged;
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
