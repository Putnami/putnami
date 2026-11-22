import { afterEach, describe, expect } from 'bun:test';
import http2 from 'node:http2';
import { gunzipSync, gzipSync } from 'node:zlib';
import { NotFoundException, Stream } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { api, endpoint } from '../../src/api';
import { type Application, application } from '../../src/application';
import { grpc } from '../../src/grpc/grpc.plugin';
import { http, type HttpPlugin } from '../../src/http/http.plugin';
import { ProtoPlugin, proto } from '../../src/proto';
import { decodeProto } from '../../src/grpc/proto-decode';
import { encodeProto } from '../../src/grpc/proto-codec';
import type { DiscoveredRoute } from '../../src/api';
import type { StreamEndpointDefinition } from '../../src/api/route/stream-endpoint';
import { ConnectRequestStream } from '../../src/grpc/connect-request-stream';
import { buildDuplexRpcHandler } from '../../src/grpc/grpc-handlers';
import { buildHttpContext } from '../../src/http/http-context.builder';
import { arrivedOverHttp2 } from '../../src/http/http-version';

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
  cancel: () => void;
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
    stream.on('close', () => resolve());
    stream.on('error', reject);
  });
  return {
    write: (value) => {
      writes++;
      stream.write(envelope(value));
    },
    end: () => stream.end(),
    cancel: () => stream.close(http2.constants.NGHTTP2_CANCEL),
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

/** What the ledger route did with each stream: committed with its total, or failed. */
const ledger: string[] = [];

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
    apiPlugin.register(
      '/counters/[n]/doubled',
      {
        GET: endpoint()
          .params({ n: Number })
          .body(Stream({ delta: Number }))
          .returns({ doubled: Number })
          .handle(async (ctx) => {
            for await (const _ of ctx.messages()) {
              // drained
            }
            return { doubled: ctx.params.n * 2 };
          }),
      },
      'GET',
    );
    apiPlugin.register(
      '/budgets/[id]/small',
      {
        GET: endpoint()
          .params({ id: String })
          .body(Stream({ text: String }))
          .returns({ count: Number })
          .client({ resilience: { stream: { maxFrameBytes: 64, idleTimeoutMs: 150 } } })
          .handle(async (ctx) => {
            let count = 0;
            for await (const _ of ctx.messages()) count++;
            return { count };
          }),
      },
      'GET',
    );
    apiPlugin.register(
      '/ledger/[id]/commit',
      {
        GET: endpoint()
          .params({ id: String })
          .body(Stream({ delta: Number }))
          .returns({ total: Number })
          .handle(async (ctx) => {
            let total = 0;
            try {
              for await (const message of ctx.messages()) total += message.delta;
            } catch (error) {
              ledger.push(`failed ${ctx.params.id}`);
              throw error;
            }
            ledger.push(ctx.signal.aborted ? `aborted ${ctx.params.id}` : `committed ${ctx.params.id} ${total}`);
            return { total };
          }),
      },
      'GET',
    );
    apiPlugin.register(
      '/vault/[id]/deposit',
      {
        GET: endpoint()
          .params({ id: String })
          .body(Stream({ delta: Number }))
          .returns({ total: Number })
          .secure()
          .handle(async () => ({ total: 0 })),
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
    'validates and coerces the declared params of the opening message',
    {
      feature: 'typescript/grpc-services',
      requirement: 'duplex-streams-over-http2',
      check: 'a-connect-client-stream-over-http2-answers-its-declared-result',
    },
    async () => {
      const port = await start();
      session = http2.connect(`http://localhost:${port}`);
      const path = '/test.v1.CountersService/ListCountersByNDoubled';
      const accepted = openH2Stream(session, path);
      accepted.write({ params: { n: '21' } });
      accepted.end();
      await accepted.done;
      expect(accepted.frames.map((frame) => frame.value)).toEqual([{ doubled: 42 }, {}]);

      const refused = openH2Stream(session, path);
      refused.write({ params: { n: 'abc' } });
      refused.end();
      await refused.done;
      expect(refused.frames).toHaveLength(1);
      expect(refused.frames[0]?.value).toMatchObject({ error: { code: 'invalid_argument' } });
    },
  );

  specTest(
    'answers a non-Connect media type over HTTP/2 unimplemented',
    {
      feature: 'typescript/grpc-services',
      requirement: 'stream-shapes',
      check: 'a-connect-caller-of-a-client-or-bidirectional-rpc-over-http1-gets-unimplemented',
    },
    async () => {
      const port = await start();
      session = http2.connect(`http://localhost:${port}`);
      const call = openH2Stream(session, CLIENT_PATH, { 'content-type': 'application/json' });
      call.end();
      await call.done;
      expect(await call.status).toBe(501);
    },
  );

  specTest(
    'refuses a message larger than the declared maxFrameBytes',
    {
      feature: 'typescript/grpc-services',
      requirement: 'duplex-stream-budgets',
      check: 'a-connect-duplex-stream-runs-under-its-declared-budgets',
    },
    async () => {
      const port = await start();
      session = http2.connect(`http://localhost:${port}`);
      const call = openH2Stream(session, '/test.v1.BudgetsService/ListBudgetsByIdSmall');
      call.write({ params: { id: 'a' }, body: { text: 'x'.repeat(200) } });
      call.end();
      await call.done;
      expect(call.frames).toHaveLength(1);
      expect(call.frames[0]?.flags).toBe(2);
      expect(call.frames[0]?.value).toMatchObject({ error: { code: 'resource_exhausted' } });
    },
  );

  specTest(
    'ends a stream idle past the declared idleTimeoutMs with deadline_exceeded',
    {
      feature: 'typescript/grpc-services',
      requirement: 'duplex-stream-budgets',
      check: 'a-connect-duplex-stream-runs-under-its-declared-budgets',
    },
    async () => {
      const port = await start();
      session = http2.connect(`http://localhost:${port}`);
      const call = openH2Stream(session, '/test.v1.BudgetsService/ListBudgetsByIdSmall');
      call.write({ params: { id: 'a' } });
      // The caller never ends its side: the idle budget ends the stream.
      const terminal = await call.next();
      expect(terminal.flags).toBe(2);
      expect(terminal.value).toMatchObject({ error: { code: 'deadline_exceeded' } });
    },
  );

  specTest(
    'refuses a secured duplex stream before any message with the error terminal',
    {
      feature: 'typescript/grpc-services',
      requirement: 'endpoint-pipeline',
      check: 'a-refused-server-stream-yields-only-the-error-terminal',
    },
    async () => {
      const port = await start();
      session = http2.connect(`http://localhost:${port}`);
      const call = openH2Stream(session, '/test.v1.VaultService/ListVaultByIdDeposit');
      call.write({ params: { id: 'a' }, body: { delta: 1 } });
      call.end();
      await call.done;
      expect(await call.status).toBe(200);
      expect(call.frames).toHaveLength(1);
      expect(call.frames[0]?.flags).toBe(2);
      expect(call.frames[0]?.value).toMatchObject({ error: { code: 'unauthenticated' } });
    },
  );

  specTest(
    'never commits a client stream its caller canceled',
    {
      feature: 'typescript/grpc-services',
      requirement: 'duplex-streams-over-http2',
      check: 'a-connect-client-stream-over-http2-answers-its-declared-result',
    },
    async () => {
      const port = await start();
      session = http2.connect(`http://localhost:${port}`);
      const canceled = openH2Stream(session, '/test.v1.LedgerService/ListLedgerByIdCommit');
      canceled.write({ params: { id: 'partial' }, body: { delta: 1 } });
      canceled.write({ body: { delta: 2 } });
      await Bun.sleep(50);
      canceled.cancel();
      const ended = openH2Stream(session, '/test.v1.LedgerService/ListLedgerByIdCommit');
      ended.write({ params: { id: 'whole' }, body: { delta: 1 } });
      ended.write({ body: { delta: 2 } });
      ended.end();
      await ended.done;
      await Bun.sleep(100);

      // The reset stream is never read as an end: the part it delivered is not
      // committed as if it were whole.
      expect(ledger).toContain('committed whole 3');
      expect(ledger.some((entry) => entry.startsWith('committed partial'))).toBe(false);
      expect(ledger.some((entry) => entry.endsWith(' partial'))).toBe(true);
    },
  );

  specTest(
    'carries binary protobuf and gzip-compressed messages',
    {
      feature: 'typescript/grpc-services',
      requirement: 'duplex-streams-over-http2',
      check: 'a-connect-client-stream-over-http2-answers-its-declared-result',
    },
    async () => {
      const port = await start();
      const meta = app?.getPlugin(ProtoPlugin).proto()?.messageMeta ?? {};
      const encode = (message: Record<string, unknown>) =>
        encodeProto(message, meta['ListTalliesByIdSumRequest'] ?? [], meta);
      const frame = (payload: Uint8Array, flags = 0) => {
        const bytes = new Uint8Array(5 + payload.length);
        bytes[0] = flags;
        new DataView(bytes.buffer).setUint32(1, payload.length);
        bytes.set(payload, 5);
        return bytes;
      };
      session = http2.connect(`http://localhost:${port}`);
      const stream = session.request({
        ':method': 'POST',
        ':path': CLIENT_PATH,
        'content-type': 'application/connect+proto',
        'connect-protocol-version': '1',
        'connect-content-encoding': 'gzip',
        'connect-accept-encoding': 'gzip',
      });
      const chunks: Buffer[] = [];
      stream.on('data', (chunk: Buffer) => chunks.push(chunk));
      const ended = new Promise<void>((resolve) => stream.on('end', () => resolve()));
      stream.write(frame(encode({ params: { id: 'p' }, body: { delta: 4 } })));
      stream.write(frame(new Uint8Array(gzipSync(encode({ body: { delta: 5 } }))), 0x01));
      stream.end();
      await ended;

      const bytes = new Uint8Array(Buffer.concat(chunks));
      const flags = bytes[0] ?? 0;
      const length = new DataView(bytes.buffer, bytes.byteOffset).getUint32(1);
      let payload = bytes.subarray(5, 5 + length);
      // The caller named gzip in Connect-Accept-Encoding, so the answer is compressed per message.
      expect(flags).toBe(0x01);
      payload = new Uint8Array(gunzipSync(payload));
      expect(decodeProto(payload, meta['ListTalliesByIdSumResponse'] ?? [], meta)).toEqual({
        id: 'p',
        count: 2,
        total: 9,
      });
      expect(bytes[5 + length]).toBe(0x02);
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

describe('Connect duplex stream bounds', () => {
  /** A body that delivers `bytes` in chunks of `size`. */
  function chunked(bytes: Uint8Array, size: number): ReadableStream<Uint8Array> {
    let offset = 0;
    return new ReadableStream({
      pull(controller) {
        if (offset >= bytes.length) {
          controller.close();
          return;
        }
        controller.enqueue(bytes.slice(offset, offset + size));
        offset += size;
      },
    });
  }

  specTest(
    'reads a large message in small chunks in linear time',
    {
      feature: 'typescript/grpc-services',
      requirement: 'duplex-stream-budgets',
      check: 'a-connect-duplex-stream-runs-under-its-declared-budgets',
    },
    async () => {
      const length = 4 * 1024 * 1024;
      const message = new Uint8Array(5 + length);
      new DataView(message.buffer).setUint32(1, length);
      message.fill(7, 5);
      const requests = new ConnectRequestStream(chunked(message, 256), null);
      const started = performance.now();
      const payload = await requests.next();
      const elapsed = performance.now() - started;
      expect(payload?.length).toBe(length);
      expect(payload?.[length - 1]).toBe(7);
      expect(await requests.next()).toBeUndefined();
      // A reader that copies its whole pending buffer per chunk takes seconds
      // here; one that fills a single buffer takes milliseconds.
      expect(elapsed).toBeLessThan(1500);
    },
  );

  specTest(
    'ends a stream whose caller does not read with resource_exhausted',
    {
      feature: 'typescript/grpc-services',
      requirement: 'duplex-stream-budgets',
      check: 'a-connect-duplex-stream-runs-under-its-declared-budgets',
    },
    async () => {
      const definition = endpoint()
        .body(Stream({ n: Number }))
        .returns(Stream({ blob: String }))
        .handle(async (ctx) => {
          for (let i = 0; i < 64; i++) ctx.send({ blob: 'x'.repeat(1024) });
          return { blob: '' };
        }) as unknown as StreamEndpointDefinition;
      const handler = buildDuplexRpcHandler(
        definition,
        { method: 'GET', path: '/flood', streamMode: 'bidirectional' } as DiscoveredRoute,
        {},
        undefined,
        false,
        { maxQueuedBytes: 8 * 1024 },
      );
      const req = new Request('http://localhost/test.v1.FloodService/ListFlood', {
        method: 'POST',
        headers: { 'content-type': 'application/connect+json' },
        body: chunked(envelope({}), 64),
        duplex: 'half',
      } as RequestInit);
      expect(arrivedOverHttp2(req)).toBe(true);
      const response = await handler(buildHttpContext({ req }) as never);
      // Nobody reads while the handler sends; only then is the body drained.
      await Bun.sleep(50);
      const bytes = new Uint8Array(await new Response(response.getBodyInit()).arrayBuffer());
      const frames = new FrameReader().push(bytes);
      const terminal = frames.at(-1);
      expect(frames.length).toBeLessThan(20);
      expect(terminal?.flags).toBe(2);
      expect(terminal?.value).toMatchObject({ error: { code: 'resource_exhausted' } });
    },
  );
});
