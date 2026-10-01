import { afterAll, beforeAll, describe, expect, test } from 'bun:test';
import { gunzipSync, gzipSync } from 'node:zlib';
import { specTest } from '@putnami/runtime/spectest';
import { ConnectTransport } from '../../src/runtime/connect-transport';
import { ClientRequestError, ClientResponseSizeError, ClientServerError } from '../../src/runtime/errors';
import { GrpcStatus } from '../../src/runtime/grpc-status';

let server: ReturnType<typeof Bun.serve>;
let baseUrl: string;

// Minimal protoMeta for a ListUsers RPC
const SIMPLE_PROTO_META = {
  messageMeta: {
    ListUsersRequest: [{ name: 'filter', number: 1, type: 'string', optional: true, repeated: false }],
    ListUsersResponse: [{ name: 'user_id', number: 1, type: 'string', optional: true, repeated: false }],
  },
  enumTypes: [] as string[],
};

function handleListUsers(req: Request, contentType: string): Promise<Response> {
  if (contentType.includes('proto')) {
    return Promise.resolve(Response.json({ user_id: 'proto-user' }));
  }
  return req
    .json()
    .then((body: { page?: number }) => Response.json({ users: [{ id: '1', name: 'Alice' }], page: body.page ?? 1 }));
}

const ROUTES: Record<string, (req: Request, ct: string) => Promise<Response>> = {
  '/myapp.v1.UsersService/ListUsers': handleListUsers,
  '/myapp.v1.UsersService/GetUser': (req) =>
    req.json().then((b: { id: string }) => Response.json({ id: b.id, name: 'Bob' })),
  '/myapp.v1.UsersService/Error': () =>
    Promise.resolve(Response.json({ code: 'invalid_argument', message: 'Name is required' }, { status: 400 })),
  '/myapp.v1.UsersService/ServerError': () =>
    Promise.resolve(Response.json({ code: 'internal', message: 'Something broke' }, { status: 500 })),
  '/myapp.v1.UsersService/ProtoError': () =>
    Promise.resolve(
      new Response(JSON.stringify({ message: 'Proto request error' }), {
        status: 422,
        headers: { 'Content-Type': 'application/json' },
      }),
    ),
  '/myapp.v1.UsersService/PlainText': () => Promise.resolve(new Response('plain text error body', { status: 400 })),
  // A provider naming its own backoff, which the retry interceptor never sees
  // because it sits outside the transport.
  '/myapp.v1.UsersService/Throttled': () =>
    Promise.resolve(
      Response.json(
        { code: 'resource_exhausted', message: 'slow down' },
        { status: 429, headers: { 'Retry-After': '3' } },
      ),
    ),
  // The gRPC spelling of a status name, which the Connect protocol does not define.
  '/myapp.v1.UsersService/LegacyCode': () =>
    Promise.resolve(Response.json({ code: 'NOT_FOUND', message: 'gone' }, { status: 404 })),
  // Size-cap fixture: a JSON body whose serialization is well over a small cap.
  '/myapp.v1.UsersService/Big': () => Promise.resolve(Response.json({ blob: 'a'.repeat(1024) })),
  // The Connect error body `buildUnimplementedRpcHandler` produces for a
  // client/bidi RPC on the Connect route.
  '/myapp.v1.UsersService/Unimplemented': () =>
    Promise.resolve(
      Response.json(
        {
          code: 'unimplemented',
          message: 'client streaming is not implemented over Connect/HTTP1.1; use the WebSocket stream transport',
        },
        { status: 501 },
      ),
    ),
  // Validation failure carrying Connect error details (`google.rpc.BadRequest`).
  '/myapp.v1.UsersService/Invalid': () =>
    Promise.resolve(
      Response.json(
        {
          code: 'invalid_argument',
          message: 'Validation failed',
          details: [
            { type: 'google.rpc.BadRequest', value: 'CgYKBG5hbWU', debug: { fieldViolations: [{ field: 'name' }] } },
          ],
        },
        { status: 422 },
      ),
    ),
  // What the server's `compressResponse` writes: gzip bytes + Content-Encoding.
  '/myapp.v1.UsersService/GzipResponse': () =>
    Promise.resolve(gzipBody(JSON.stringify({ user_id: 'gzipped', blob: 'z'.repeat(256) }), 'content-encoding')),
  // gRPC message-level encoding: the fetch layer leaves this one to the client.
  '/myapp.v1.UsersService/GrpcGzipResponse': () =>
    Promise.resolve(gzipBody(JSON.stringify({ user_id: 'grpc-gzipped' }), 'grpc-encoding')),
  // Echoes back the protocol headers and the (possibly gunzipped) request body.
  '/myapp.v1.UsersService/Echo': async (req) => {
    const raw = new Uint8Array(await req.arrayBuffer());
    const contentEncoding = req.headers.get('content-encoding');
    const bytes = contentEncoding === 'gzip' ? new Uint8Array(gunzipSync(Buffer.from(raw))) : raw;
    return Response.json({
      body: new TextDecoder().decode(bytes),
      contentEncoding,
      connectTimeout: req.headers.get('connect-timeout-ms'),
      grpcTimeout: req.headers.get('grpc-timeout'),
      acceptEncoding: req.headers.get('accept-encoding'),
      connectVersion: req.headers.get('connect-protocol-version'),
      contentType: req.headers.get('content-type'),
    });
  },
};

/** Build a gzip-compressed JSON response, declaring the encoding on `header`. */
function gzipBody(json: string, header: 'content-encoding' | 'grpc-encoding'): Response {
  const compressed = gzipSync(Buffer.from(json));
  return new Response(compressed.buffer.slice(compressed.byteOffset, compressed.byteOffset + compressed.byteLength), {
    headers: { 'Content-Type': 'application/json', [header]: 'gzip' },
  });
}

interface EchoBody {
  body: string;
  contentEncoding: string | null;
  connectTimeout: string | null;
  grpcTimeout: string | null;
  contentType: string | null;
  acceptEncoding: string | null;
  connectVersion: string | null;
}

beforeAll(() => {
  server = Bun.serve({
    port: 0,
    fetch(req) {
      const url = new URL(req.url);
      const ct = req.headers.get('Content-Type') ?? '';
      const handler = ROUTES[url.pathname];
      return handler ? handler(req, ct) : Promise.resolve(new Response('Not found', { status: 404 }));
    },
  });
  baseUrl = `http://localhost:${server.port}`;
});

afterAll(() => {
  server.stop(true);
});

describe('ConnectTransport JSON mode', () => {
  test('sends POST with JSON body', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const response = await transport.execute<{ users: unknown[]; page: number }>({
      method: 'POST',
      path: '/myapp.v1.UsersService/ListUsers',
      body: { page: 2 },
      headers: new Headers(),
    });

    expect(response.status).toBe(200);
    expect(response.data.page).toBe(2);
    expect(response.data.users).toHaveLength(1);
  });

  test('sends empty body when no body provided', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const response = await transport.execute<{ users: unknown[] }>({
      method: 'POST',
      path: '/myapp.v1.UsersService/ListUsers',
      headers: new Headers(),
    });

    expect(response.status).toBe(200);
  });

  test('throws ClientRequestError for 4xx Connect error', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');

    try {
      await transport.execute({
        method: 'POST',
        path: '/myapp.v1.UsersService/Error',
        body: {},
        headers: new Headers(),
      });
      expect.unreachable('Should have thrown');
    } catch (error) {
      expect(error).toBeInstanceOf(ClientRequestError);
      expect((error as ClientRequestError).status).toBe(400);
      expect((error as ClientRequestError).message).toBe('Name is required');
    }
  });

  test('throws ClientServerError for 5xx', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');

    try {
      await transport.execute({
        method: 'POST',
        path: '/myapp.v1.UsersService/ServerError',
        body: {},
        headers: new Headers(),
      });
      expect.unreachable('Should have thrown');
    } catch (error) {
      expect(error).toBeInstanceOf(ClientServerError);
      expect((error as ClientServerError).message).toBe('Something broke');
    }
  });

  test('error carries service name and method path', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');

    try {
      await transport.execute({
        method: 'POST',
        path: '/myapp.v1.UsersService/Error',
        headers: new Headers(),
      });
      expect.unreachable();
    } catch (error) {
      expect((error as ClientRequestError).service).toBe('myapp.v1');
      expect((error as ClientRequestError).method).toBe('/myapp.v1.UsersService/Error');
    }
  });

  test('handles plain-text error response body', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');

    try {
      await transport.execute({
        method: 'POST',
        path: '/myapp.v1.UsersService/PlainText',
        headers: new Headers(),
      });
      expect.unreachable();
    } catch (error) {
      expect(error).toBeInstanceOf(ClientRequestError);
      expect((error as ClientRequestError).status).toBe(400);
      // Falls back to "HTTP 400" when body has no .message field
      expect((error as ClientRequestError).message).toContain('400');
    }
  });

  test('strips trailing slash from baseUrl', async () => {
    const transport = new ConnectTransport(`${baseUrl}/`, 'myapp.v1');
    const response = await transport.execute<{ users: unknown[] }>({
      method: 'POST',
      path: '/myapp.v1.UsersService/ListUsers',
      headers: new Headers(),
    });
    expect(response.status).toBe(200);
  });
});

describe('ConnectTransport response-size cap (Go parity)', () => {
  specTest(
    'a body over the cap fails with ClientResponseSizeError',
    {
      feature: 'typescript/service-clients',
      requirement: 'transport-safety',
      check: 'the-connect-transport-caps-its-response-body-too',
    },
    async () => {
      const transport = new ConnectTransport(baseUrl, 'myapp.v1', 'json', undefined, 16);

      await expect(
        transport.execute({
          method: 'POST',
          path: '/myapp.v1.UsersService/Big',
          headers: new Headers(),
        }),
      ).rejects.toBeInstanceOf(ClientResponseSizeError);
    },
  );

  test('a body under the cap (and the 32 MiB default) is accepted', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const response = await transport.execute<{ blob: string }>({
      method: 'POST',
      path: '/myapp.v1.UsersService/Big',
      headers: new Headers(),
    });

    expect(response.status).toBe(200);
    expect(response.data.blob.length).toBe(1024);
  });
});

describe('ConnectTransport proto mode', () => {
  test('sends binary proto body and falls back to JSON response', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1', 'proto', SIMPLE_PROTO_META);
    const response = await transport.execute<{ user_id: string }>({
      method: 'POST',
      path: '/myapp.v1.UsersService/ListUsers',
      body: { filter: 'active' },
      headers: new Headers(),
    });

    expect(response.status).toBe(200);
    expect(response.data.user_id).toBe('proto-user');
  });

  test('sends empty binary body when no request fields in protoMeta', async () => {
    // protoMeta without ListUsersRequest fields — should send empty bytes
    const transport = new ConnectTransport(baseUrl, 'myapp.v1', 'proto', {
      messageMeta: {},
      enumTypes: [],
    });
    const response = await transport.execute<{ user_id: string }>({
      method: 'POST',
      path: '/myapp.v1.UsersService/ListUsers',
      body: { filter: 'active' },
      headers: new Headers(),
    });
    expect(response.status).toBe(200);
  });

  test('sends empty binary body when request body is undefined', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1', 'proto', SIMPLE_PROTO_META);
    const response = await transport.execute<{ user_id: string }>({
      method: 'POST',
      path: '/myapp.v1.UsersService/ListUsers',
      headers: new Headers(),
    });
    expect(response.status).toBe(200);
  });

  test('throws ClientRequestError for 4xx in proto mode', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1', 'proto', SIMPLE_PROTO_META);

    try {
      await transport.execute({
        method: 'POST',
        path: '/myapp.v1.UsersService/ProtoError',
        body: { filter: 'x' },
        headers: new Headers(),
      });
      expect.unreachable();
    } catch (error) {
      expect(error).toBeInstanceOf(ClientRequestError);
      expect((error as ClientRequestError).status).toBe(422);
      expect((error as ClientRequestError).message).toBe('Proto request error');
    }
  });

  test('uses JSON mode when encoding is json even with protoMeta provided', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1', 'json', SIMPLE_PROTO_META);
    const response = await transport.execute<{ users: unknown[] }>({
      method: 'POST',
      path: '/myapp.v1.UsersService/ListUsers',
      body: { page: 1 },
      headers: new Headers(),
    });
    expect(response.status).toBe(200);
    expect(response.data.users).toBeDefined();
  });

  test('uses JSON mode when protoMeta is not provided despite proto encoding', async () => {
    // encoding=proto but no protoMeta → falls through to JSON path
    const transport = new ConnectTransport(baseUrl, 'myapp.v1', 'proto', undefined);
    const response = await transport.execute<{ users: unknown[] }>({
      method: 'POST',
      path: '/myapp.v1.UsersService/ListUsers',
      body: { page: 1 },
      headers: new Headers(),
    });
    expect(response.status).toBe(200);
  });
});

describe('ConnectTransport typed gRPC statuses', () => {
  test('a Connect error body names the numeric gRPC status', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');

    try {
      await transport.execute({
        method: 'POST',
        path: '/myapp.v1.UsersService/Error',
        headers: new Headers(),
      });
      expect.unreachable();
    } catch (error) {
      const err = error as ClientRequestError;
      expect(err).toBeInstanceOf(ClientRequestError);
      expect(err.grpcCode).toBe(GrpcStatus.INVALID_ARGUMENT);
      expect(err.grpcStatus).toBe('INVALID_ARGUMENT');
    }
  });

  test('a gRPC-spelled code is not a Connect code, and the status decides instead', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');

    try {
      await transport.execute({
        method: 'POST',
        path: '/myapp.v1.UsersService/LegacyCode',
        headers: new Headers(),
      });
      expect.unreachable();
    } catch (error) {
      const err = error as ClientRequestError;
      // `NOT_FOUND` is not one of the sixteen codes, so the body carries none
      // and the protocol's HTTP inference applies: a bare 404 is `unimplemented`.
      expect(err.grpcCode).toBe(GrpcStatus.UNIMPLEMENTED);
    }
  });

  test('status 12 UNIMPLEMENTED survives the 501 transport status', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');

    try {
      await transport.execute({
        method: 'POST',
        path: '/myapp.v1.UsersService/Unimplemented',
        headers: new Headers(),
      });
      expect.unreachable();
    } catch (error) {
      const err = error as ClientServerError;
      expect(err).toBeInstanceOf(ClientServerError);
      expect(err.status).toBe(501);
      expect(err.grpcCode).toBe(GrpcStatus.UNIMPLEMENTED);
      expect(err.grpcStatus).toBe('UNIMPLEMENTED');
      expect(err.message).toContain('WebSocket');
    }
  });

  test('Connect error details ride along with the typed error', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');

    try {
      await transport.execute({
        method: 'POST',
        path: '/myapp.v1.UsersService/Invalid',
        headers: new Headers(),
      });
      expect.unreachable();
    } catch (error) {
      const err = error as ClientRequestError;
      expect(err.grpcCode).toBe(GrpcStatus.INVALID_ARGUMENT);
      expect(err.details).toHaveLength(1);
      const [first] = (err.details ?? []) as { type: string; value: string }[];
      expect(first.type).toBe('google.rpc.BadRequest');
      // The specification requires a `value`; a detail carrying only `debug` is
      // not a detail a conforming client may read.
      expect(first.value).toBe('CgYKBG5hbWU');
    }
  });

  test('records the provider Retry-After budget where the retry interceptor can read it', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const retryState: { retryAfterMs?: number } = {};

    try {
      await transport.execute({
        method: 'POST',
        path: '/myapp.v1.UsersService/Throttled',
        headers: new Headers(),
        retryState,
      });
      expect.unreachable();
    } catch {
      expect(retryState.retryAfterMs).toBe(3000);
    }
  });

  test('a body without a code falls back to the protocol HTTP inference table', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');

    try {
      await transport.execute({
        method: 'POST',
        path: '/myapp.v1.UsersService/PlainText',
        headers: new Headers(),
      });
      expect.unreachable();
    } catch (error) {
      // "HTTP to Error Code": a bare 400 is `internal`, because an
      // intermediary — not the service — is the likely author of it.
      expect((error as ClientRequestError).grpcCode).toBe(GrpcStatus.INTERNAL);
    }
  });
});

describe('ConnectTransport compression parity', () => {
  test('advertises the encodings it can decode and the protocol version', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const response = await transport.execute<EchoBody>({
      method: 'POST',
      path: '/myapp.v1.UsersService/Echo',
      body: { hello: 'world' },
      headers: new Headers(),
    });

    expect(response.data.acceptEncoding).toContain('gzip');
    expect(response.data.connectVersion).toBe('1');
    // A unary call uses the bare codec media type, not the streaming one.
    expect(response.data.contentType).toBe('application/json');
  });

  test('round-trips a gzip response the server compressed (Content-Encoding)', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const response = await transport.execute<{ user_id: string; blob: string }>({
      method: 'POST',
      path: '/myapp.v1.UsersService/GzipResponse',
      headers: new Headers(),
    });

    expect(response.data.user_id).toBe('gzipped');
    expect(response.data.blob.length).toBe(256);
  });

  test('gunzips a gRPC message-encoded response the fetch layer leaves alone', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const response = await transport.execute<{ user_id: string }>({
      method: 'POST',
      path: '/myapp.v1.UsersService/GrpcGzipResponse',
      headers: new Headers(),
    });

    expect(response.data.user_id).toBe('grpc-gzipped');
  });

  test('does not compress request bodies by default', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const response = await transport.execute<EchoBody>({
      method: 'POST',
      path: '/myapp.v1.UsersService/Echo',
      body: { page: 1 },
      headers: new Headers(),
    });

    expect(response.data.contentEncoding).toBeNull();
    expect(JSON.parse(response.data.body)).toEqual({ page: 1 });
  });

  test('compresses request bodies when asked, declaring Content-Encoding', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1', 'json', undefined, undefined, {
      compressRequests: true,
    });
    const response = await transport.execute<EchoBody>({
      method: 'POST',
      path: '/myapp.v1.UsersService/Echo',
      body: { page: 7 },
      headers: new Headers(),
    });

    expect(response.data.contentEncoding).toBe('gzip');
    expect(JSON.parse(response.data.body)).toEqual({ page: 7 });
  });

  test('compresses binary proto request bodies too', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1', 'proto', SIMPLE_PROTO_META, undefined, {
      compressRequests: true,
    });
    const response = await transport.execute<{ user_id: string }>({
      method: 'POST',
      path: '/myapp.v1.UsersService/ListUsers',
      body: { filter: 'active' },
      headers: new Headers(),
    });

    expect(response.status).toBe(200);
    expect(response.data.user_id).toBe('proto-user');
  });
});

describe('ConnectTransport deadlines', () => {
  test('sends Connect-Timeout-Ms derived from the per-call timeout, in whole milliseconds', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1', 'json', undefined, undefined, { timeoutMs: 2500 });
    const response = await transport.execute<EchoBody>({
      method: 'POST',
      path: '/myapp.v1.UsersService/Echo',
      headers: new Headers(),
    });

    expect(response.data.connectTimeout).toBe('2500');
    // `grpc-timeout` belongs to gRPC, not to Connect; sending it would leave a
    // conforming Connect server with no deadline at all.
    expect(response.data.grpcTimeout).toBeNull();
  });

  test('sends no deadline when none is configured', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1');
    const response = await transport.execute<EchoBody>({
      method: 'POST',
      path: '/myapp.v1.UsersService/Echo',
      headers: new Headers(),
    });

    expect(response.data.connectTimeout).toBeNull();
  });

  test('carries the call budget left rather than the configured timeout', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1', 'json', undefined, undefined, { timeoutMs: 30_000 });
    const response = await transport.execute<EchoBody>({
      method: 'POST',
      path: '/myapp.v1.UsersService/Echo',
      headers: new Headers(),
      // The retry loop sets the shared absolute deadline; a later attempt has
      // less budget left than the first one did.
      deadlineAt: Date.now() + 1200,
    });

    const sent = Number(response.data.connectTimeout);
    expect(sent).toBeGreaterThan(0);
    expect(sent).toBeLessThanOrEqual(1200);
  });

  test('an explicit Connect-Timeout-Ms header wins over the configured timeout', async () => {
    const transport = new ConnectTransport(baseUrl, 'myapp.v1', 'json', undefined, undefined, { timeoutMs: 2500 });
    const response = await transport.execute<EchoBody>({
      method: 'POST',
      path: '/myapp.v1.UsersService/Echo',
      headers: new Headers({ 'connect-timeout-ms': '10000' }),
    });

    expect(response.data.connectTimeout).toBe('10000');
  });
});
