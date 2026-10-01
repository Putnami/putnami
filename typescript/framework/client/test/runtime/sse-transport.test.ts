import { describe, expect, test } from 'bun:test';
import type { ClientContractOperation, ClientSchema } from '@putnami/application';
import { SseTransport } from '../../src/runtime/sse-transport';
import { CredentialManager, rejectRequestCredentials, serviceAuthInterceptor } from '../../src/runtime/credential';
import { StreamSession } from '../../src/runtime/stream-session';
import type { StreamObserver } from '../../src/runtime/stream.type';
import type { ClientRequest } from '../../src/runtime/transport.type';

const output = {
  type: 'object',
  properties: { id: { type: 'string' } },
  required: ['id'],
  additionalProperties: false,
} as const satisfies ClientSchema;

describe('SseTransport first-party stream', () => {
  test('splits coalesced events before applying the per-frame cap and retains late delivery', async () => {
    const server = Bun.serve({
      port: 0,
      fetch: () =>
        new Response('data: {"id":"one"}\n\ndata: {"id":"two"}\n\n', {
          headers: { 'Content-Type': 'text/event-stream' },
        }),
    });
    try {
      const stream = new SseTransport(`http://localhost:${server.port}`, 'items').stream<{ id: string }>(
        request(),
        { output },
        session({ maxFrameBytes: 24 }),
      );
      await new Promise((resolve) => setTimeout(resolve, 10));
      await expect(collect(stream)).resolves.toEqual([{ id: 'one' }, { id: 'two' }]);
    } finally {
      server.stop(true);
    }
  });

  test('decodes a declared terminal error envelope without exposing undeclared fields', async () => {
    const detailSchema = {
      type: 'object',
      properties: { reason: { type: 'string' } },
      required: ['reason'],
      additionalProperties: false,
    } as const satisfies ClientSchema;
    const server = Bun.serve({
      port: 0,
      fetch: () =>
        new Response('event: error\ndata: {"status":404,"code":"not_found","details":{"reason":"gone"}}\n\n', {
          headers: { 'Content-Type': 'text/event-stream' },
        }),
    });
    try {
      const operation = operationContract([{ status: 404, code: 'not_found', schema: detailSchema }]);
      const stream = new SseTransport(`http://localhost:${server.port}`, 'items').stream(
        request(operation),
        { output },
        session({ maxFrameBytes: 256 }),
      );
      await expect(collect(stream)).rejects.toMatchObject({ code: 'not_found', details: { reason: 'gone' } });
    } finally {
      server.stop(true);
    }
  });

  // The SSE terminal frame both providers write is the first-party
  // error envelope plus `status`, flat, with `details` holding the declared
  // body alone. Its free-text `message` follows the one consumer gate every
  // transport reads: nothing by default, the provider's prose on an opted-in
  // request, and the same inline redaction of this call's own bearer. The Go
  // half reads the same bytes in TestSSETerminalCarriesTheMessageUnderTheSameOptIn
  // (go/framework/client/server_stream_test.go).
  const sseTerminalFixture =
    '{"status":404,"code":"not_found","error":"Not Found","message":"widget 4f0c does not exist","details":{"resource":"widget"}}';
  const sseTerminalOperation = () =>
    operationContract([
      {
        status: 404,
        code: 'not_found',
        schema: {
          type: 'object',
          properties: { resource: { type: 'string' } },
          required: ['resource'],
          additionalProperties: false,
        },
      },
    ]);

  for (const scene of [
    { name: 'keeps the synthetic message on a default request', expected: 'items request failed with not_found' },
    {
      name: 'carries the terminal message when the request opts in',
      opted: true,
      expected: 'widget 4f0c does not exist',
    },
  ]) {
    test(scene.name, async () => {
      const server = Bun.serve({
        port: 0,
        fetch: () =>
          new Response(`event: error\ndata: ${sseTerminalFixture}\n\n`, {
            headers: { 'Content-Type': 'text/event-stream' },
          }),
      });
      try {
        const stream = new SseTransport(`http://localhost:${server.port}`, 'items').stream(
          { ...request(sseTerminalOperation()), ...(scene.opted ? { carryRemoteMessage: true } : {}) },
          { output },
          session({ maxFrameBytes: 512 }),
        );
        const failure = await collect(stream).catch((error: unknown) => error);
        expect(failure).toMatchObject({ status: 404, code: 'not_found', details: { resource: 'widget' } });
        expect((failure as Error).message).toBe(scene.expected);
      } finally {
        server.stop(true);
      }
    });
  }

  test('redacts this call bearer inline from a carried terminal message', async () => {
    const server = Bun.serve({
      port: 0,
      fetch: () =>
        new Response(
          'event: error\ndata: {"status":400,"code":"errors.invalid","error":"Bad Request","message":"credential active-service-token expired"}\n\n',
          { headers: { 'Content-Type': 'text/event-stream' } },
        ),
    });
    try {
      const stream = new SseTransport(`http://localhost:${server.port}`, 'items').stream(
        { ...request(sseTerminalOperation()), carryRemoteMessage: true, secretValues: ['active-service-token'] },
        { output },
        session({ maxFrameBytes: 512 }),
      );
      const failure = await collect(stream).catch((error: unknown) => error);
      expect(failure).toMatchObject({ status: 400, code: 'client.remote' });
      expect((failure as Error).message).toBe('credential [REDACTED] expired');
      expect(JSON.stringify(failure)).not.toContain('active-service-token');
    } finally {
      server.stop(true);
    }
  });

  test('preserves a declared nullable terminal detail instead of decoding the envelope', async () => {
    const server = Bun.serve({
      port: 0,
      fetch: () =>
        new Response('event: error\ndata: {"status":409,"code":"conflict","details":null}\n\n', {
          headers: { 'Content-Type': 'text/event-stream' },
        }),
    });
    try {
      const operation = operationContract([
        { status: 409, code: 'conflict', schema: { type: 'string', nullable: true } },
      ]);
      const stream = new SseTransport(`http://localhost:${server.port}`, 'items').stream(
        request(operation),
        { output },
        session({ maxFrameBytes: 256 }),
      );
      await expect(collect(stream)).rejects.toMatchObject({ code: 'conflict', details: null });
    } finally {
      server.stop(true);
    }
  });

  test('refuses to type a terminal envelope whose code is not declared at that status', async () => {
    const detailSchema = {
      type: 'object',
      properties: { reason: { type: 'string' } },
      required: ['reason'],
      additionalProperties: false,
    } as const satisfies ClientSchema;
    const server = Bun.serve({
      port: 0,
      fetch: () =>
        // `NotFound` is the PascalCase authoring affordance, never a wire code
        // (D0.1). An envelope carrying it against a contract that declares
        // `not_found` has no declared identity, so no declared detail may be
        // exposed either.
        new Response('event: error\ndata: {"status":404,"code":"NotFound","details":{"reason":"gone"}}\n\n', {
          headers: { 'Content-Type': 'text/event-stream' },
        }),
    });
    try {
      const operation = operationContract([{ status: 404, code: 'not_found', schema: detailSchema }]);
      const stream = new SseTransport(`http://localhost:${server.port}`, 'items').stream(
        request(operation),
        { output },
        session({ maxFrameBytes: 256 }),
      );
      const failure = await collect(stream).catch((error: unknown) => error);
      expect(failure).toMatchObject({ status: 404, code: 'client.remote' });
      expect((failure as { details?: unknown }).details).toBeUndefined();
      expect(JSON.stringify(failure)).not.toContain('gone');
    } finally {
      server.stop(true);
    }
  });

  test('invalidates the exact credential acquisition when the SSE handshake rejects it', async () => {
    let acquisitions = 0;
    const manager = new CredentialManager();
    const authentication = serviceAuthInterceptor({
      serviceId: 'items',
      serviceUrl: 'https://items.example.test',
      audience: 'api://items',
      clientId: 'consumer',
      profiles: { service: { kind: 'service-token' } },
      credentials: {
        service: {
          source: 'oauth-client-credentials',
          provider: async () => ({
            value: `token-${++acquisitions}`,
            expiresAt: new Date(Date.now() + 60_000),
          }),
        },
      },
      manager,
    });
    const server = Bun.serve({
      port: 0,
      fetch: () => Response.json({ code: 'Unauthorized' }, { status: 401 }),
    });
    const transport = new SseTransport(`http://localhost:${server.port}`, 'items');
    try {
      const rejectedHandshake = async () => {
        const authenticated = request({
          ...operationContract(),
          security: { alternatives: [{ allOf: [{ profile: 'service' }] }] },
        });
        await authentication(authenticated, async (prepared) => {
          await expect(
            collect(transport.stream(prepared, { output }, session({ maxFrameBytes: 256, request: prepared }))),
          ).rejects.toMatchObject({ status: 401 });
          return { data: undefined, status: 200, headers: new Headers() };
        });
      };
      await rejectedHandshake();
      await rejectedHandshake();
      expect(acquisitions).toBe(2);
    } finally {
      server.stop(true);
    }
  });
});

/** A session with the declared bounds these fixtures use; the transport owns nothing else. */
function session<T>(options: { maxFrameBytes: number; request?: ClientRequest }): StreamSession<T> {
  return new StreamSession<T>({
    serviceId: 'items',
    operationId: 'watch',
    protocol: 'sse',
    budgets: {
      handshakeMs: 1000,
      idleMs: 1000,
      sessionMs: 0,
      heartbeatMs: 0,
      maxFrameBytes: options.maxFrameBytes,
      maxBufferedMessages: 2,
    },
    ...(options.request
      ? { invalidateCredentials: () => rejectRequestCredentials(options.request as ClientRequest) }
      : {}),
  });
}

function request(operation = operationContract()): ClientRequest {
  return {
    method: 'GET',
    path: '/watch',
    headers: new Headers(),
    operationId: 'watch',
    clientOperation: operation,
  };
}

function operationContract(errors: ClientContractOperation['errors'] = []): ClientContractOperation {
  return {
    stream: 'server',
    messages: { output },
    transports: [{ protocol: 'sse', path: '/watch', encoding: 'json' }],
    security: { alternatives: [{ allOf: [] }] },
    errors,
    idempotency: { kind: 'safe' },
  };
}

function collect<T>(stream: StreamObserver<T>): Promise<T[]> {
  return new Promise((resolve, reject) => {
    const values: T[] = [];
    stream.onMessage((value) => values.push(value));
    stream.onError(reject);
    stream.onComplete(() => resolve(values));
  });
}
