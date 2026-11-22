import { afterEach, describe, expect } from 'bun:test';
import http2 from 'node:http2';
import { NotFoundException, Stream } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { api, endpoint } from '../../src/api';
import { type Application, application } from '../../src/application';
import { grpc } from '../../src/grpc/grpc.plugin';
import { http, type HttpPlugin } from '../../src/http/http.plugin';
import { proto } from '../../src/proto';

// Client and bidirectional streams ride Connect over HTTP/2 (prior knowledge,
// cleartext), on the same port that keeps serving HTTP/1.1 and WebSocket.

const CLIENT_PATH = '/test.v1.TalliesService/ListTalliesByIdSum';
const BIDI_PATH = '/test.v1.TalliesService/ListTalliesByIdRunning';

interface Frame {
  flags: number;
  value: unknown;
}

function envelope(value: unknown, flags = 0): Uint8Array<ArrayBuffer> {
  const payload = new TextEncoder().encode(JSON.stringify(value));
  const frame = new Uint8Array(new ArrayBuffer(5 + payload.length));
  frame[0] = flags;
  new DataView(frame.buffer).setUint32(1, payload.length);
  frame.set(payload, 5);
  return frame;
}

/** Incremental envelope reader: whole frames only, in arrival order. */
class FrameReader {
  private buffer = new Uint8Array(0);

  push(chunk: Uint8Array): Frame[] {
    const merged = new Uint8Array(this.buffer.length + chunk.length);
    merged.set(this.buffer);
    merged.set(chunk, this.buffer.length);
    this.buffer = merged;
    const frames: Frame[] = [];
    while (this.buffer.length >= 5) {
      const size = new DataView(this.buffer.buffer, this.buffer.byteOffset + 1, 4).getUint32(0);
      if (this.buffer.length < 5 + size) break;
      const text = new TextDecoder().decode(this.buffer.subarray(5, 5 + size));
      frames.push({ flags: this.buffer[0] ?? 0, value: text.length > 0 ? JSON.parse(text) : undefined });
      this.buffer = this.buffer.slice(5 + size);
    }
    return frames;
  }
}

/**
 * One Connect stream over an HTTP/2 session: messages are written when the
 * test says so, and every frame the provider answers is recorded with the
 * number of messages written before it arrived.
 */
function openH2Stream(
  session: http2.ClientHttp2Session,
  path: string,
  headers: Record<string, string> = {},
): {
  write: (value: unknown) => void;
  end: () => void;
  frames: Array<Frame & { afterWrites: number }>;
  status: Promise<number>;
  next: () => Promise<Frame & { afterWrites: number }>;
  done: Promise<void>;
} {
  const stream = session.request({
    ':method': 'POST',
    ':path': path,
    'content-type': 'application/connect+json',
    'connect-protocol-version': '1',
    ...headers,
  });
  const reader = new FrameReader();
  const frames: Array<Frame & { afterWrites: number }> = [];
  const waiters: Array<(frame: Frame & { afterWrites: number }) => void> = [];
  let writes = 0;
  let delivered = 0;
  const status = new Promise<number>((resolve) => stream.on('response', (h) => resolve(Number(h[':status']))));
  stream.on('data', (chunk: Buffer) => {
    for (const frame of reader.push(new Uint8Array(chunk))) {
      const recorded = { ...frame, afterWrites: writes };
      frames.push(recorded);
      const waiter = waiters.shift();
      if (waiter) {
        delivered++;
        waiter(recorded);
      }
    }
  });
  const done = new Promise<void>((resolve, reject) => {
    stream.on('end', () => resolve());
    stream.on('error', reject);
  });
  return {
    write: (value) => {
      writes++;
      stream.write(envelope(value));
    },
    end: () => stream.end(),
    frames,
    status,
    next: () =>
      new Promise((resolve) => {
        const pending = frames[delivered];
        if (pending) {
          delivered++;
          resolve(pending);
        } else waiters.push(resolve);
      }),
    done,
  };
}

describe('Connect client and bidirectional streams', () => {
  let app: Application | undefined;
  let httpPlugin: HttpPlugin;
  let session: http2.ClientHttp2Session | undefined;

  afterEach(async () => {
    session?.close();
    session = undefined;
    await app?.stop();
    app = undefined;
  });

  async function start(): Promise<number> {
    httpPlugin = http({ port: 0 });
    const apiPlugin = api({ autoScan: false });
    apiPlugin.register(
      '/tallies/[id]/sum',
      {
        GET: endpoint()
          .params({ id: String })
          .body(Stream({ delta: Number }))
          .returns({ id: String, count: Number, total: Number })
          .handle(async (ctx) => {
            if (ctx.params.id === 'missing') throw new NotFoundException('tally not found');
            let count = 0;
            let total = 0;
            for await (const message of ctx.messages()) {
              count++;
              total += message.delta;
            }
            return { id: ctx.params.id, count, total };
          }),
      },
      'GET',
    );
    apiPlugin.register(
      '/tallies/[id]/running',
      {
        GET: endpoint()
          .params({ id: String })
          .body(Stream({ delta: Number }))
          .returns(Stream({ id: String, count: Number, total: Number }))
          .handle(async (ctx) => {
            let count = 0;
            let total = 0;
            for await (const message of ctx.messages()) {
              count++;
              total += message.delta;
              ctx.send({ id: ctx.params.id, count, total });
            }
            // The declared result of a bidirectional stream is its last message.
            return { id: ctx.params.id, count: -1, total };
          }),
      },
      'GET',
    );
    app = application()
      .use(httpPlugin)
      .use(apiPlugin)
      .use(proto({ packageName: 'test.v1', output: false }))
      .use(grpc());
    await app.start();
    return httpPlugin.getServer()?.port as number;
  }

  specTest(
    'answers a client stream over HTTP/2 with its declared result',
    {
      feature: 'typescript/grpc-services',
      requirement: 'duplex-streams-over-http2',
      check: 'a-connect-client-stream-over-http2-answers-its-declared-result',
    },
    async () => {
      const port = await start();
      session = http2.connect(`http://localhost:${port}`);
      const call = openH2Stream(session, CLIENT_PATH);
      call.write({ params: { id: 'a' }, body: { delta: 2 } });
      call.write({ body: { delta: 3 } });
      call.write({ body: { delta: 5 } });
      call.end();
      await call.done;

      expect(await call.status).toBe(200);
      expect(call.frames.map((frame) => [frame.flags, frame.value])).toEqual([
        [0, { id: 'a', count: 3, total: 10 }],
        [2, {}],
      ]);
    },
  );

  specTest(
    'answers a bidirectional stream message by message while the caller still sends',
    {
      feature: 'typescript/grpc-services',
      requirement: 'duplex-streams-over-http2',
      check: 'a-connect-bidirectional-stream-over-http2-answers-while-the-caller-still-sends',
    },
    async () => {
      const port = await start();
      session = http2.connect(`http://localhost:${port}`);
      const call = openH2Stream(session, BIDI_PATH);

      // Each answer arrives before the next message is written: the request
      // body is read as it arrives and the response is written as it goes.
      call.write({ params: { id: 'b' }, body: { delta: 1 } });
      const first = await call.next();
      call.write({ body: { delta: 4 } });
      const second = await call.next();
      call.end();
      const result = await call.next();
      const terminal = await call.next();
      await call.done;

      expect(first).toEqual({ flags: 0, value: { id: 'b', count: 1, total: 1 }, afterWrites: 1 });
      expect(second).toEqual({ flags: 0, value: { id: 'b', count: 2, total: 5 }, afterWrites: 2 });
      expect(result).toEqual({ flags: 0, value: { id: 'b', count: -1, total: 5 }, afterWrites: 2 });
      expect(terminal).toEqual({ flags: 2, value: {}, afterWrites: 2 });
    },
  );

  specTest(
    'ends a failed duplex stream with the error in its one terminal',
    {
      feature: 'typescript/grpc-services',
      requirement: 'duplex-streams-over-http2',
      check: 'a-connect-client-stream-over-http2-answers-its-declared-result',
    },
    async () => {
      const port = await start();
      session = http2.connect(`http://localhost:${port}`);
      const call = openH2Stream(session, CLIENT_PATH);
      call.write({ params: { id: 'missing' } });
      call.end();
      await call.done;

      expect(await call.status).toBe(200);
      expect(call.frames).toHaveLength(1);
      expect(call.frames[0]?.flags).toBe(2);
      expect(call.frames[0]?.value).toMatchObject({ error: { code: 'not_found' } });
    },
  );

  specTest(
    'refuses a message that does not match the declared stream input',
    {
      feature: 'typescript/grpc-services',
      requirement: 'duplex-streams-over-http2',
      check: 'a-connect-client-stream-over-http2-answers-its-declared-result',
    },
    async () => {
      const port = await start();
      session = http2.connect(`http://localhost:${port}`);
      const call = openH2Stream(session, CLIENT_PATH);
      call.write({ params: { id: 'a' }, body: { delta: 'not a number' } });
      call.end();
      await call.done;

      expect(call.frames).toHaveLength(1);
      expect(call.frames[0]?.value).toMatchObject({ error: { code: 'invalid_argument' } });
    },
  );

  specTest(
    'keeps answering the same RPCs unimplemented over HTTP/1.1',
    {
      feature: 'typescript/grpc-services',
      requirement: 'stream-shapes',
      check: 'a-connect-caller-of-a-client-or-bidirectional-rpc-over-http1-gets-unimplemented',
    },
    async () => {
      const port = await start();
      for (const path of [CLIENT_PATH, BIDI_PATH]) {
        // biome-ignore lint/performance/noAwaitInLoops: two independent calls, checked one after the other
        const response = await fetch(`http://localhost:${port}${path}`, {
          method: 'POST',
          headers: { 'content-type': 'application/connect+json', 'connect-protocol-version': '1' },
          body: envelope({ params: { id: 'a' }, body: { delta: 1 } }),
        });
        expect(response.status).toBe(501);
        expect(await response.json()).toMatchObject({ code: 'unimplemented' });
      }
    },
  );
});
