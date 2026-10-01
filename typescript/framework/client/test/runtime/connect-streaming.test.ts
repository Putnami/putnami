import { afterAll, beforeAll, describe, expect, test } from 'bun:test';
import { gzipSync } from 'node:zlib';
import { createEnvelope, encodeProto } from '@putnami/application';
import { ConnectTransport } from '../../src/runtime/connect-transport';
import { ClientResponseSizeError, ClientStreamError } from '../../src/runtime/errors';
import { GrpcStatus } from '../../src/runtime/grpc-status';
import type { StreamObserver } from '../../src/runtime/stream.type';

// The fixtures below reproduce, byte for byte, what `GrpcPlugin`'s
// `buildStreamRpcHandler` writes: Connect envelope frames built with the
// server's own `createEnvelope`, payloads encoded with the server's own
// `encodeProto`, per-message gzip via `gzipSync` (the server's `compressGzip`),
// and a final flag-0x02 frame carrying the `EndStreamResponse`. Whether those
// bytes are the protocol's is settled by
// `@putnami/application`'s `test/grpc/connect-conformance`, against a corpus
// transcribed from the published specification.

const encoder = new TextEncoder();

const REQUEST_FIELDS = [{ name: 'filter', number: 1, type: 'string', optional: true, repeated: false }];
const RESPONSE_FIELDS = [
  { name: 'user_id', number: 1, type: 'string', optional: true, repeated: false },
  { name: 'seq', number: 2, type: 'int32', optional: true, repeated: false },
];

/** Every streaming RPC in this fixture shares the Watch request/response shape. */
const RPC_NAMES = ['WatchUsers', 'WatchUsersPlain', 'WatchUsersChunked'];

const PROTO_META = {
  messageMeta: Object.fromEntries(
    RPC_NAMES.flatMap((rpc) => [
      [`${rpc}Request`, REQUEST_FIELDS],
      [`${rpc}Response`, RESPONSE_FIELDS],
    ]),
  ),
  enumTypes: [] as string[],
};

const MESSAGES = [
  { user_id: 'alice', seq: 1 },
  { user_id: 'bob', seq: 2 },
];

interface StreamFixture {
  messages?: Record<string, unknown>[];
  trailer?: Record<string, unknown> | null;
  /** Server-side compression toggle (`grpc({ compression })`). */
  compression?: boolean;
  /** Split every frame in half across two chunks. */
  chunked?: boolean;
}

function connectStream(req: Request, fixture: StreamFixture): Response {
  const accept = req.headers.get('accept') ?? '';
  const useProto = accept.includes('application/connect+proto');
  const useGzip = (fixture.compression ?? true) && (req.headers.get('connect-accept-encoding') ?? '').includes('gzip');
  const responseMeta = RESPONSE_FIELDS;

  const frames: Uint8Array[] = [];
  for (const message of fixture.messages ?? MESSAGES) {
    const encoded = useProto
      ? encodeProto(message, responseMeta, PROTO_META.messageMeta, new Set())
      : encoder.encode(JSON.stringify(message));
    frames.push(useGzip ? createEnvelope(0x01, new Uint8Array(gzipSync(encoded))) : createEnvelope(0x00, encoded));
  }
  if (fixture.trailer !== null) {
    frames.push(createEnvelope(0x02, encoder.encode(JSON.stringify(fixture.trailer ?? {}))));
  }

  const body = new ReadableStream({
    start(controller) {
      for (const frame of frames) {
        if (fixture.chunked) {
          const split = Math.max(1, Math.floor(frame.length / 2));
          controller.enqueue(frame.subarray(0, split));
          controller.enqueue(frame.subarray(split));
        } else {
          controller.enqueue(frame);
        }
      }
      controller.close();
    },
  });

  const headers: Record<string, string> = {
    'Content-Type': useProto ? 'application/connect+proto' : 'application/connect+json',
    'Connect-Protocol-Version': '1',
    'Accept-Encoding': 'gzip, identity',
  };
  if (useGzip) headers['Connect-Content-Encoding'] = 'gzip';
  return new Response(body, { headers });
}

/** The Connect error body `handleGrpcError` writes for an unimplemented RPC. */
function unimplementedResponse(): Response {
  return Response.json(
    {
      code: 'unimplemented',
      message: 'bidirectional streaming is not implemented over Connect/HTTP1.1; use the WebSocket stream transport',
    },
    { status: 501, headers: { 'Connect-Protocol-Version': '1' } },
  );
}

/** Write exact frames, so a scene can break a rule the provider never breaks. */
function rawStream(frames: Uint8Array[]): Response {
  const body = new ReadableStream({
    start(controller) {
      for (const frame of frames) controller.enqueue(frame);
      controller.close();
    },
  });
  return new Response(body, {
    headers: { 'Content-Type': 'application/connect+json', 'Connect-Protocol-Version': '1' },
  });
}

let server: ReturnType<typeof Bun.serve>;
let baseUrl: string;
let lastRequestHeaders: Headers | undefined;

const ROUTES: Record<string, (req: Request) => Response> = {
  '/myapp.v1.UsersService/WatchUsers': (req) => connectStream(req, {}),
  '/myapp.v1.UsersService/WatchUsersPlain': (req) => connectStream(req, { compression: false }),
  '/myapp.v1.UsersService/WatchUsersChunked': (req) => connectStream(req, { compression: false, chunked: true }),
  '/myapp.v1.UsersService/WatchUsersFailing': (req) =>
    connectStream(req, {
      compression: false,
      messages: [MESSAGES[0]],
      trailer: {
        error: {
          code: 'not_found',
          message: 'user stream vanished',
          details: [{ type: 'google.rpc.ErrorInfo', value: 'CglOT1RfRk9VTkQ' }],
        },
      },
    }),
  '/myapp.v1.UsersService/WatchUsersTruncated': (req) =>
    connectStream(req, { compression: false, messages: [MESSAGES[0]], trailer: null }),
  '/myapp.v1.UsersService/WatchUsersHuge': (req) =>
    connectStream(req, { compression: false, messages: [{ user_id: 'x'.repeat(4096), seq: 1 }] }),
  // Two terminals, a frame after the terminal, a reserved flag, an undeclared
  // compressed frame, and an invalid terminal: the five ways a peer can break
  // the response-stream rules the specification states.
  '/myapp.v1.UsersService/WatchUsersDoubleTerminal': () =>
    rawStream([createEnvelope(0x02, encoder.encode('{}')), createEnvelope(0x02, encoder.encode('{}'))]),
  '/myapp.v1.UsersService/WatchUsersAfterTerminal': () =>
    rawStream([createEnvelope(0x02, encoder.encode('{}')), createEnvelope(0x00, encoder.encode('{"user_id":"late"}'))]),
  '/myapp.v1.UsersService/WatchUsersReservedFlag': () =>
    rawStream([
      createEnvelope(0x04, encoder.encode('{"user_id":"reserved"}')),
      createEnvelope(0x02, encoder.encode('{}')),
    ]),
  '/myapp.v1.UsersService/WatchUsersUndeclaredGzip': () =>
    rawStream([
      createEnvelope(0x01, new Uint8Array(gzipSync(encoder.encode('{"user_id":"gz"}')))),
      createEnvelope(0x02, encoder.encode('{}')),
    ]),
  '/myapp.v1.UsersService/WatchUsersInvalidTerminal': () =>
    rawStream([createEnvelope(0x02, encoder.encode('{"error":{}}'))]),
  '/myapp.v1.UsersService/ChatUsers': () => unimplementedResponse(),
  '/myapp.v1.UsersService/WatchUsersUnary': () => Response.json({ user_id: 'not-a-stream' }),
};

beforeAll(() => {
  server = Bun.serve({
    port: 0,
    fetch(req) {
      lastRequestHeaders = new Headers(req.headers);
      const handler = ROUTES[new URL(req.url).pathname];
      return handler ? handler(req) : new Response('Not found', { status: 404 });
    },
  });
  baseUrl = `http://localhost:${server.port}`;
});

afterAll(() => {
  server.stop(true);
});

interface StreamOutcome<T> {
  messages: T[];
  error?: Error;
  completed: boolean;
}

/** Drive an observer to its terminal event (or a short deadline). */
function drain<T>(observer: StreamObserver<T>, timeoutMs = 5000): Promise<StreamOutcome<T>> {
  return new Promise((resolve) => {
    const messages: T[] = [];
    const timer = setTimeout(() => resolve({ messages, completed: false }), timeoutMs);
    const settle = (outcome: Omit<StreamOutcome<T>, 'messages'>) => {
      clearTimeout(timer);
      resolve({ messages, ...outcome });
    };
    observer.onMessage((data) => messages.push(data));
    observer.onError((error) => settle({ error, completed: false }));
    observer.onComplete(() => settle({ completed: true }));
  });
}

describe('ConnectTransport server-streaming (JSON)', () => {
  test('consumes envelope frames and completes on the OK trailer', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const outcome = await drain(
      transport.stream<{ user_id: string; seq: number }>('/myapp.v1.UsersService/WatchUsersPlain', {
        body: { filter: 'active' },
      }),
    );

    expect(outcome.error).toBeUndefined();
    expect(outcome.completed).toBe(true);
    expect(outcome.messages).toEqual(MESSAGES);
  });

  test('negotiates the Connect streaming protocol on the wire', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    await drain(transport.stream('/myapp.v1.UsersService/WatchUsersPlain'));

    expect(lastRequestHeaders?.get('accept')).toBe('application/connect+json');
    // A stream declares the streaming media type on both sides; the bare
    // `application/json` of a unary call would be read as a unary request.
    expect(lastRequestHeaders?.get('content-type')).toBe('application/connect+json');
    expect(lastRequestHeaders?.get('connect-protocol-version')).toBe('1');
    expect(lastRequestHeaders?.get('connect-accept-encoding')).toContain('gzip');
    // Streams are long-lived: the per-call timeout must not become their deadline.
    expect(lastRequestHeaders?.get('connect-timeout-ms')).toBeNull();
  });

  test('reassembles frames split across chunk boundaries', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const outcome = await drain(transport.stream('/myapp.v1.UsersService/WatchUsersChunked'));

    expect(outcome.completed).toBe(true);
    expect(outcome.messages).toEqual(MESSAGES);
  });

  test('carries interceptor-populated headers', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const headers = Promise.resolve(new Headers({ Authorization: 'Bearer stream-token' }));
    await drain(transport.stream('/myapp.v1.UsersService/WatchUsersPlain', { headers }));

    expect(lastRequestHeaders?.get('authorization')).toBe('Bearer stream-token');
  });
});

describe('ConnectTransport server-streaming (binary proto)', () => {
  test('decodes proto-encoded frames', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1', 'proto', PROTO_META);
    const outcome = await drain(
      transport.stream<{ user_id: string; seq: number }>('/myapp.v1.UsersService/WatchUsersPlain', {
        body: { filter: 'active' },
      }),
    );

    expect(outcome.completed).toBe(true);
    expect(outcome.messages).toEqual(MESSAGES);
    expect(lastRequestHeaders?.get('accept')).toBe('application/connect+proto');
    expect(lastRequestHeaders?.get('content-type')).toBe('application/connect+proto');
  });
});

describe('ConnectTransport server-streaming compression', () => {
  test('decodes per-message gzip frames (JSON)', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const outcome = await drain(transport.stream('/myapp.v1.UsersService/WatchUsers'));

    expect(outcome.completed).toBe(true);
    expect(outcome.messages).toEqual(MESSAGES);
  });

  test('decodes per-message gzip frames (binary proto)', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1', 'proto', PROTO_META);
    const outcome = await drain(transport.stream('/myapp.v1.UsersService/WatchUsers'));

    expect(outcome.completed).toBe(true);
    expect(outcome.messages).toEqual(MESSAGES);
  });
});

describe('ConnectTransport server-streaming failures', () => {
  test('a failing EndStreamResponse becomes a typed ClientStreamError', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const outcome = await drain(transport.stream('/myapp.v1.UsersService/WatchUsersFailing'));

    expect(outcome.messages).toEqual([MESSAGES[0]]);
    expect(outcome.error).toBeInstanceOf(ClientStreamError);
    const error = outcome.error as ClientStreamError;
    expect(error.grpcCode).toBe(GrpcStatus.NOT_FOUND);
    expect(error.grpcStatus).toBe('NOT_FOUND');
    expect(error.message).toBe('user stream vanished');
    expect(error.details).toHaveLength(1);
    expect(error.status).toBe(0);
  });

  test('a stream that ends without an EndStreamResponse is a failure, not a completion', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const outcome = await drain(transport.stream('/myapp.v1.UsersService/WatchUsersTruncated'));

    expect(outcome.completed).toBe(false);
    expect(outcome.error).toBeInstanceOf(ClientStreamError);
    expect((outcome.error as ClientStreamError).grpcCode).toBe(GrpcStatus.INTERNAL);
  });

  test('a second end-of-stream message is refused', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const outcome = await drain(transport.stream('/myapp.v1.UsersService/WatchUsersDoubleTerminal'));

    expect(outcome.completed).toBe(false);
    expect(outcome.error?.message).toContain('after the end-of-stream message');
  });

  test('a message after the end-of-stream message is refused', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const outcome = await drain(transport.stream('/myapp.v1.UsersService/WatchUsersAfterTerminal'));

    expect(outcome.completed).toBe(false);
    expect(outcome.error?.message).toContain('after the end-of-stream message');
  });

  test('a reserved envelope flag is refused', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const outcome = await drain(transport.stream('/myapp.v1.UsersService/WatchUsersReservedFlag'));

    expect(outcome.completed).toBe(false);
    expect(outcome.error?.message).toContain('reserved Connect envelope flag');
  });

  test('a compressed frame with no declared stream encoding is refused', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const outcome = await drain(transport.stream('/myapp.v1.UsersService/WatchUsersUndeclaredGzip'));

    expect(outcome.completed).toBe(false);
    expect(outcome.error?.message).toContain('without declaring a stream encoding');
  });

  test('a malformed end-of-stream message is a failure, not a completion', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const outcome = await drain(transport.stream('/myapp.v1.UsersService/WatchUsersInvalidTerminal'));

    expect(outcome.completed).toBe(false);
    expect(outcome.error).toBeDefined();
  });

  test('a frame over the response cap fails with ClientResponseSizeError', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1', 'json', undefined, 64);
    const outcome = await drain(transport.stream('/myapp.v1.UsersService/WatchUsersHuge'));

    expect(outcome.error).toBeInstanceOf(ClientResponseSizeError);
  });

  test('a missing route surfaces a typed error when the caller declines the fallback', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const outcome = await drain(transport.stream('/myapp.v1.UsersService/Missing', { onUnimplemented: () => false }));

    expect(outcome.error).toBeInstanceOf(ClientStreamError);
    // A bare 404 written by the HTTP layer means the RPC has no route here —
    // `unimplemented` in the protocol's inference table, not `not_found`.
    expect((outcome.error as ClientStreamError).grpcCode).toBe(GrpcStatus.UNIMPLEMENTED);
  });
});

describe('ConnectTransport server-streaming UNIMPLEMENTED', () => {
  test('routes gRPC status 12 to onUnimplemented instead of the error handler', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    let claimed: ClientStreamError | undefined;

    const outcome = await drain(
      transport.stream('/myapp.v1.UsersService/ChatUsers', {
        onUnimplemented: (error) => {
          claimed = error;
          return true;
        },
      }),
      500,
    );

    expect(claimed).toBeInstanceOf(ClientStreamError);
    expect(claimed?.grpcCode).toBe(GrpcStatus.UNIMPLEMENTED);
    expect(claimed?.grpcStatus).toBe('UNIMPLEMENTED');
    expect(claimed?.message).toContain('WebSocket');
    // Claimed by the fallback — nothing reaches the caller's handlers.
    expect(outcome.error).toBeUndefined();
    expect(outcome.completed).toBe(false);
  });

  test('surfaces the typed UNIMPLEMENTED error when no fallback claims it', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const outcome = await drain(transport.stream('/myapp.v1.UsersService/ChatUsers'));

    expect(outcome.error).toBeInstanceOf(ClientStreamError);
    expect((outcome.error as ClientStreamError).grpcCode).toBe(GrpcStatus.UNIMPLEMENTED);
  });

  test('a unary answer to a streaming RPC is treated as UNIMPLEMENTED', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    let claimed: ClientStreamError | undefined;

    await drain(
      transport.stream('/myapp.v1.UsersService/WatchUsersUnary', {
        onUnimplemented: (error) => {
          claimed = error;
          return true;
        },
      }),
      500,
    );

    expect(claimed?.grpcCode).toBe(GrpcStatus.UNIMPLEMENTED);
    expect(claimed?.message).toContain('Connect streaming not supported');
  });

  test('surfaces a non-stream answer as an error when no fallback claims it', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const outcome = await drain(transport.stream('/myapp.v1.UsersService/WatchUsersUnary'));

    expect(outcome.error).toBeInstanceOf(ClientStreamError);
    expect((outcome.error as ClientStreamError).grpcCode).toBe(GrpcStatus.UNIMPLEMENTED);
  });
});

describe('ConnectTransport server-streaming cancellation', () => {
  test('cancel() ends the stream without reporting an error', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const observer = transport.stream('/myapp.v1.UsersService/WatchUsersPlain');

    let reported: Error | undefined;
    observer.onError((error) => {
      reported = error;
    });
    observer.cancel();

    await Bun.sleep(50);
    expect(reported).toBeUndefined();
  });

  test('an external abort signal cancels the fetch', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const controller = new AbortController();
    const observer = transport.stream('/myapp.v1.UsersService/WatchUsersPlain', { signal: controller.signal });

    const outcome = drain(observer, 1000);
    controller.abort();

    const result = await outcome;
    expect(result.completed).toBe(false);
  });
});
