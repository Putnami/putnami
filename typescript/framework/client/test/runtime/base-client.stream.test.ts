import { afterAll, afterEach, beforeAll, describe, expect, test } from 'bun:test';
import { createEnvelope } from '@putnami/application';
import { BaseClient } from '../../src/runtime/base-client';
import { ClientTransportUnavailableError } from '../../src/runtime/errors';
import { GrpcStatus } from '../../src/runtime/grpc-status';
import type { DuplexStream, StreamObserver } from '../../src/runtime/stream.type';
import type { Interceptor } from '../../src/runtime/transport.type';

const encoder = new TextEncoder();

class StreamingClient extends BaseClient {
  readonly serviceName = 'streaming-test';

  watch<T>(path: string, body?: unknown): StreamObserver<T> {
    return this.stream<T>(path, { body });
  }

  chat<TIn, TOut>(path: string, body?: unknown): DuplexStream<TIn, TOut> {
    return this.streamDuplex<TIn, TOut>(path, { body });
  }
}

/** Replace a client's WebSocket transport with a controllable stub. */
function stubWs(client: BaseClient): {
  path: () => string | undefined;
  headers: () => Promise<Headers>;
  emit: (data: unknown) => void;
  finish: () => void;
  cancelled: () => boolean;
} {
  let path: string | undefined;
  let headers: Headers | Promise<Headers> | undefined;
  let onMessage: ((data: unknown) => void) | undefined;
  let onComplete: (() => void) | undefined;
  let cancelled = false;

  (
    client as unknown as {
      wsTransport: {
        stream: (path: string, body?: unknown, headers?: Headers | Promise<Headers>) => StreamObserver<unknown>;
      };
    }
  ).wsTransport = {
    stream: (streamPath, _body, streamHeaders) => {
      path = streamPath;
      headers = streamHeaders;
      return {
        onMessage: (handler) => {
          onMessage = handler;
        },
        onError: () => {},
        onComplete: (handler) => {
          onComplete = handler;
        },
        cancel: () => {
          cancelled = true;
        },
      };
    },
  };

  return {
    path: () => path,
    headers: async () => (await headers) ?? new Headers(),
    emit: (data) => onMessage?.(data),
    finish: () => onComplete?.(),
    cancelled: () => cancelled,
  };
}

/** Run a body with `globalThis.WebSocket` removed, restoring it afterwards. */
async function withoutWebSocket(body: () => Promise<void>): Promise<void> {
  const original = globalThis.WebSocket;
  // biome-ignore lint/performance/noDelete: the absence of the global is exactly what is under test
  delete (globalThis as { WebSocket?: unknown }).WebSocket;
  try {
    await body();
  } finally {
    (globalThis as { WebSocket?: unknown }).WebSocket = original;
  }
}

let server: ReturnType<typeof Bun.serve>;
let baseUrl: string;
let lastHeaders: Headers | undefined;

beforeAll(() => {
  server = Bun.serve({
    port: 0,
    fetch(req) {
      lastHeaders = new Headers(req.headers);
      const path = new URL(req.url).pathname;

      if (path === '/myapp.v1.UsersService/WatchUsers') {
        const body = new ReadableStream({
          start(controller) {
            controller.enqueue(createEnvelope(0x00, encoder.encode(JSON.stringify({ user_id: 'alice' }))));
            controller.enqueue(createEnvelope(0x02, encoder.encode('{}')));
            controller.close();
          },
        });
        return new Response(body, { headers: { 'Content-Type': 'application/connect+json' } });
      }

      if (path === '/myapp.v1.UsersService/ChatUsers') {
        // Exactly what `buildUnimplementedRpcHandler` answers for a bidi RPC.
        return Response.json(
          {
            code: 'unimplemented',
            message:
              'bidirectional streaming is not implemented over Connect/HTTP1.1; use the WebSocket stream transport for this RPC',
          },
          { status: 501 },
        );
      }

      return new Response('Not found', { status: 404 });
    },
  });
  baseUrl = `http://localhost:${server.port}`;
});

afterAll(() => {
  server.stop(true);
});

afterEach(() => {
  lastHeaders = undefined;
});

function nextMessage<T>(observer: StreamObserver<T>, timeoutMs = 3000): Promise<{ data?: T; error?: Error }> {
  return new Promise((resolve) => {
    const timer = setTimeout(() => resolve({}), timeoutMs);
    observer.onMessage((data) => {
      clearTimeout(timer);
      resolve({ data });
    });
    observer.onError((error) => {
      clearTimeout(timer);
      resolve({ error });
    });
  });
}

describe('BaseClient server-streaming over Connect', () => {
  test('a Connect client consumes the stream without touching WebSocket', async () => {
    const client = new StreamingClient({ baseUrl, transport: 'connect', packageName: 'myapp.v1' });
    const ws = stubWs(client);

    const result = await nextMessage<{ user_id: string }>(
      client.watch('/myapp.v1.UsersService/WatchUsers', { filter: 'all' }),
    );

    expect(result.data).toEqual({ user_id: 'alice' });
    expect(ws.path()).toBeUndefined();
    expect(lastHeaders?.get('accept')).toBe('application/connect+json');
  });

  test('interceptor headers reach the Connect stream request', async () => {
    const tagging: Interceptor = async (request, next) => {
      request.headers.set('Authorization', 'Bearer stream-token');
      return next(request);
    };
    const client = new StreamingClient({
      baseUrl,
      transport: 'connect',
      packageName: 'myapp.v1',
      interceptors: [tagging],
    });

    await nextMessage(client.watch('/myapp.v1.UsersService/WatchUsers'));
    expect(lastHeaders?.get('authorization')).toBe('Bearer stream-token');
  });

  test('an HTTP client still streams over WebSocket', async () => {
    const client = new StreamingClient({ baseUrl, transport: 'http' });
    const ws = stubWs(client);

    const pending = nextMessage<{ user_id: string }>(client.watch('/events'));
    ws.emit({ user_id: 'ws-alice' });

    expect((await pending).data).toEqual({ user_id: 'ws-alice' });
    expect(ws.path()).toBe('/events');
  });
});

describe('BaseClient UNIMPLEMENTED fallback', () => {
  test('auto-routes to WebSocket after a typed UNIMPLEMENTED', async () => {
    const client = new StreamingClient({ baseUrl, transport: 'connect', packageName: 'myapp.v1' });
    const ws = stubWs(client);

    const observer = client.watch<{ text: string }>('/myapp.v1.UsersService/ChatUsers', { room: 'general' });
    const pending = nextMessage(observer);

    // The fallback only engages once the server has answered, so wait for the
    // adopted transport to appear before driving it.
    while (ws.path() === undefined) {
      // biome-ignore lint/performance/noAwaitInLoops: poll until the asynchronous fallback adopts the transport
      await Bun.sleep(5);
    }

    expect(ws.path()).toBe('/myapp.v1.UsersService/ChatUsers');
    expect((await ws.headers()).has('Content-Type')).toBe(false);

    ws.emit({ text: 'hello' });
    expect((await pending).data).toEqual({ text: 'hello' });

    let completed = false;
    observer.onComplete(() => {
      completed = true;
    });
    ws.finish();
    expect(completed).toBe(true);

    observer.cancel();
    expect(ws.cancelled()).toBe(true);
  });

  test('reports a typed error when no WebSocket transport exists', async () => {
    await withoutWebSocket(async () => {
      const client = new StreamingClient({ baseUrl, transport: 'connect', packageName: 'myapp.v1' });
      const result = await nextMessage(client.watch('/myapp.v1.UsersService/ChatUsers'));

      expect(result.error).toBeInstanceOf(ClientTransportUnavailableError);
      const error = result.error as ClientTransportUnavailableError;
      expect(error.transport).toBe('websocket');
      expect(error.method).toBe('/myapp.v1.UsersService/ChatUsers');
      expect(error.service).toBe('streaming-test');
    });
  });

  test('a WebSocket-only stream reports the same typed error', async () => {
    await withoutWebSocket(async () => {
      const client = new StreamingClient({ baseUrl, transport: 'http' });
      const result = await nextMessage(client.watch('/events'));

      expect(result.error).toBeInstanceOf(ClientTransportUnavailableError);
    });
  });
});

describe('BaseClient duplex streams', () => {
  test('client/bidi streams go straight to WebSocket', async () => {
    const client = new StreamingClient({ baseUrl, transport: 'connect', packageName: 'myapp.v1' });
    let capturedPath: string | undefined;
    (
      client as unknown as {
        wsTransport: { streamDuplex: (path: string) => DuplexStream<unknown, unknown> };
      }
    ).wsTransport = {
      streamDuplex: (path) => {
        capturedPath = path;
        return {
          send: () => {},
          end: () => {},
          cancel: () => {},
          onMessage: () => {},
          onError: () => {},
          onComplete: () => {},
        };
      },
    };

    client.chat('/myapp.v1.UsersService/ChatUsers');
    expect(capturedPath).toBe('/myapp.v1.UsersService/ChatUsers');
    // No Connect round trip is spent learning what the protocol cannot carry.
    expect(lastHeaders).toBeUndefined();
  });

  test('a duplex stream reports a typed error when WebSocket is missing', async () => {
    await withoutWebSocket(async () => {
      const client = new StreamingClient({ baseUrl, transport: 'connect', packageName: 'myapp.v1' });
      const duplex = client.chat<{ text: string }, { text: string }>('/myapp.v1.UsersService/ChatUsers');

      const result = await nextMessage(duplex);
      expect(result.error).toBeInstanceOf(ClientTransportUnavailableError);

      // The stream is inert rather than throwing on use.
      expect(() => {
        duplex.send({ text: 'ignored' });
        duplex.end();
        duplex.cancel();
      }).not.toThrow();
    });
  });
});

describe('BaseClient stream cancellation', () => {
  test('cancelling before the response arrives delivers nothing', async () => {
    const client = new StreamingClient({ baseUrl, transport: 'connect', packageName: 'myapp.v1' });
    const observer = client.watch('/myapp.v1.UsersService/WatchUsers');

    let seen = 0;
    let failed: Error | undefined;
    observer.onMessage(() => {
      seen++;
    });
    observer.onError((error) => {
      failed = error;
    });
    observer.cancel();

    await Bun.sleep(80);
    expect(seen).toBe(0);
    expect(failed).toBeUndefined();
  });
});

describe('gRPC status constants', () => {
  test('UNIMPLEMENTED is 12, matching the server', () => {
    expect(GrpcStatus.UNIMPLEMENTED).toBe(12);
  });
});
