import { afterEach, describe, expect, it } from 'bun:test';
import { application, type ClientContractOperation, setCollector, TelemetryCollector } from '@putnami/application';
import { runInContext } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { BaseClient } from '../../src/runtime/base-client';
import type { BinarySource } from '../../src/runtime/binary';
import {
  ClientFrameworkError,
  ClientRequestEncodingError,
  ClientResponseContractError,
} from '../../src/runtime/errors';
import { registerServiceClient } from '../../src/runtime/service-binding';
import { CredentialRegistryClosedError } from '../../src/runtime/credential';
import { serviceAttemptTelemetryInterceptor } from '../../src/runtime/service-telemetry';

const operation: ClientContractOperation = {
  stream: 'unary',
  transports: [{ protocol: 'rest-json', path: '/stream', encoding: 'json' }],
  security: { alternatives: [{ allOf: [] }] },
  errors: [{ status: 503, code: 'unavailable', retryable: true }],
  idempotency: { kind: 'idempotent' },
  resilience: { retry: { maxAttempts: 3 } },
};
const successes = [
  {
    status: 200,
    description: 'OK',
    content: [
      { mediaType: '*/*', schema: { type: 'string' as const, format: 'binary' }, maxBytes: 4096, streamed: true },
    ],
  },
];
class RawClient extends BaseClient {
  readonly serviceName = 'raw';
  read(signal?: AbortSignal) {
    return this.requestBinaryStream<200>('GET', '/stream', {
      operationId: 'read',
      streamedResponse: true,
      maxPayloadBytes: 4096,
      successes,
      signal,
    });
  }
  upload(body: BinarySource, contentType: string) {
    return this.requestBinaryStream<200>('POST', '/stream', {
      operationId: 'read',
      body,
      requestMediaType: contentType,
      streamedRequest: true,
      streamedResponse: true,
      maxRequestBytes: 4096,
      maxPayloadBytes: 4096,
      successes,
    });
  }
}
const servers: ReturnType<typeof Bun.serve>[] = [];
afterEach(() => {
  for (const server of servers.splice(0)) server.stop(true);
});
function serve(fetch: (request: Request) => Response | Promise<Response>) {
  const server = Bun.serve({ port: 0, fetch });
  servers.push(server);
  return `http://localhost:${server.port}`;
}

describe('raw HTTP stream runtime', () => {
  it('finishes attempt telemetry once at EOF, failure, cancellation or abort, even without a pending read', async () => {
    for (const terminal of ['eof', 'failure', 'cancel', 'abort']) {
      const collector = new TelemetryCollector();
      setCollector(collector);
      const abort = new AbortController();
      let source!: ReadableStreamDefaultController<Uint8Array>;
      let canceled = 0;
      const body = new ReadableStream<Uint8Array>(
        {
          start(controller) {
            source = controller;
            controller.enqueue(new Uint8Array([1]));
          },
          cancel() {
            canceled++;
          },
        },
        { highWaterMark: 0 },
      );
      try {
        const response = await serviceAttemptTelemetryInterceptor('raw')(
          { method: 'GET', path: '/stream', headers: new Headers(), clientOperation: operation, signal: abort.signal },
          async () => ({ data: body, status: 200, headers: new Headers() }),
        );
        expect(collector.drainSpans()).toHaveLength(0);
        const reader = (response.data as ReadableStream<Uint8Array>).getReader();
        expect((await reader.read()).value).toEqual(new Uint8Array([1]));
        expect(collector.drainSpans()).toHaveLength(0);
        if (terminal === 'eof') {
          source.close();
          expect((await reader.read()).done).toBe(true);
        } else if (terminal === 'failure') {
          source.error(new Error('private source failure'));
          await expect(reader.read()).rejects.toThrow('private source failure');
        } else if (terminal === 'cancel') {
          await reader.cancel();
        } else {
          abort.abort();
          await Promise.resolve();
          expect(canceled).toBe(1);
          await expect(reader.read()).rejects.toBeInstanceOf(Error);
        }
        const spans = collector.drainSpans();
        expect(spans).toHaveLength(1);
        expect(spans[0]?.attributes['http.response.status_code']).toBe(200);
        expect(spans[0]?.attributes['error.type']).toBe(
          terminal === 'failure' ? 'client.response' : terminal === 'abort' ? 'client.canceled' : undefined,
        );
        await reader.cancel().catch(() => {});
        expect(collector.drainSpans()).toHaveLength(0);
      } finally {
        setCollector(undefined);
      }
    }
  });

  it('closes an open raw response when its application registry stops', async () => {
    const app = application();
    registerServiceClient(
      app,
      RawClient,
      {
        contract: { protocolVersion: 1, service: { id: 'raw', audience: 'raw' }, credentials: {} },
        service: 'Raw',
        transport: 'http',
        operations: { read: operation },
      },
      {
        url: serve(
          () =>
            new Response(
              new ReadableStream<Uint8Array>({
                start(c) {
                  c.enqueue(new Uint8Array([1]));
                },
              }),
              { headers: { 'Content-Type': 'image/png' } },
            ),
        ),
        allowInsecure: true,
        clientId: 'raw-consumer',
      },
    );
    await app.start();
    try {
      const result = await app.context.get(RawClient).read();
      const reader = result.body.getReader();
      expect((await reader.read()).value).toEqual(new Uint8Array([1]));
      const pending = reader.read().catch((error: unknown) => error);
      await app.stop();
      expect(await pending).toBeInstanceOf(CredentialRegistryClosedError);
    } finally {
      await app.stop();
    }
  });
  it('never follows a redirect with a streamed upload', async () => {
    let calls = 0;
    const client = new RawClient({
      baseUrl: serve((request) => {
        calls++;
        return new URL(request.url).pathname === '/stream'
          ? new Response(null, { status: 307, headers: { Location: '/replayed' } })
          : new Response(new Uint8Array([1]), { headers: { 'Content-Type': 'image/png' } });
      }),
      transport: 'http',
    });
    try {
      await expect(client.upload(new Uint8Array([255]), 'image/png')).rejects.toBeInstanceOf(Error);
      expect(calls).toBe(1);
    } finally {
      client.dispose();
    }
  });

  it('keeps caller cancellation attached after the generated method returns', async () => {
    const abort = new AbortController();
    const client = new RawClient({
      baseUrl: serve(
        () =>
          new Response(
            new ReadableStream<Uint8Array>({
              start(c) {
                c.enqueue(new Uint8Array([1]));
              },
            }),
            { headers: { 'Content-Type': 'image/png' } },
          ),
      ),
      transport: 'http',
    });
    try {
      const response = await client.read(abort.signal);
      const reader = response.body.getReader();
      expect((await reader.read()).value).toEqual(new Uint8Array([1]));
      abort.abort();
      await expect(reader.read()).rejects.toBeInstanceOf(Error);
    } finally {
      client.dispose();
    }
  });
  specTest(
    'returns headers and initial octets before EOF and bypasses whole-body limits',
    {
      feature: 'typescript/service-clients',
      requirement: 'streamed-binary-payloads',
      check: 'raw-http-streams-return-before-eof-and-do-not-buffer',
    },
    async () => {
      let control: ReadableStreamDefaultController<Uint8Array> | undefined;
      const client = new RawClient({
        baseUrl: serve(
          () =>
            new Response(
              new ReadableStream<Uint8Array>({
                start(controller) {
                  control = controller;
                  controller.enqueue(new Uint8Array([255, 128]));
                },
              }),
              { headers: { 'Content-Type': 'application/json; profile="opaque octets"' } },
            ),
        ),
        transport: 'http',
        maxResponseSize: 1,
        operationContracts: { read: operation },
      });
      try {
        const result = await client.read();
        expect(result.contentType).toBe('application/json; profile="opaque octets"');
        const reader = result.body.getReader();
        expect((await reader.read()).value).toEqual(new Uint8Array([255, 128]));
        control?.enqueue(new Uint8Array([0, 254]));
        control?.close();
        expect((await reader.read()).value).toEqual(new Uint8Array([0, 254]));
        expect((await reader.read()).done).toBe(true);
      } finally {
        client.dispose();
      }
    },
  );

  it('preserves an uploaded reader and refuses malformed content types before dialing', async () => {
    let calls = 0;
    const client = new RawClient({
      baseUrl: serve(async (req) => {
        calls++;
        return new Response(req.body, { headers: { 'Content-Type': req.headers.get('Content-Type')! } });
      }),
      transport: 'http',
    });
    const payload = new Uint8Array([0, 255, 128, 10]);
    try {
      const stream = new ReadableStream<Uint8Array>({
        start(c) {
          c.enqueue(payload);
          c.close();
        },
      });
      const result = await client.upload(stream, 'image/png; name="pixel"');
      expect(result.contentType).toBe('image/png; name="pixel"');
      expect(new Uint8Array(await new Response(result.body).arrayBuffer())).toEqual(payload);
      for (const contentType of ['', '*/*', 'image/*', 'bad', 'image/png;broken']) {
        await expect(client.upload(payload, contentType)).rejects.toBeInstanceOf(ClientRequestEncodingError);
      }
      expect(calls).toBe(1);
    } finally {
      client.dispose();
    }
  });

  it('enforces streamed request and response bounds incrementally', async () => {
    const client = new RawClient({
      baseUrl: serve(async (request) => {
        if (request.method === 'POST') {
          await request.arrayBuffer();
          return new Response(null, { status: 204 });
        }
        return new Response(new Uint8Array(4097), { headers: { 'Content-Type': 'application/octet-stream' } });
      }),
      transport: 'http',
    });
    try {
      const response = await client.read();
      await expect(new Response(response.body).arrayBuffer()).rejects.toBeInstanceOf(ClientResponseContractError);
      await expect(
        client.upload(
          new ReadableStream<Uint8Array>({
            start(controller) {
              controller.enqueue(new Uint8Array(4097));
              controller.close();
            },
          }),
          'application/octet-stream',
        ),
      ).rejects.toBeInstanceOf(ClientRequestEncodingError);
    } finally {
      client.dispose();
    }
  });

  it('refuses undeclared status and malformed response media before consuming the source', async () => {
    for (const status of [200, 201]) {
      const client = new RawClient({
        baseUrl: serve(
          () =>
            new Response(new Uint8Array([255]), {
              status,
              headers: { 'Content-Type': status === 200 ? '*/*' : 'image/png' },
            }),
        ),
        transport: 'http',
      });
      try {
        await expect(client.read()).rejects.toBeInstanceOf(ClientResponseContractError);
      } finally {
        client.dispose();
      }
    }
  });

  specTest(
    'never retries or remints a consumed streamed upload',
    {
      feature: 'typescript/service-clients',
      requirement: 'streamed-binary-payloads',
      check: 'raw-http-stream-uploads-are-never-replayed',
    },
    async () => {
      let calls = 0;
      const client = new RawClient({
        baseUrl: serve(() => {
          calls++;
          return Response.json({ code: 'unavailable', message: 'later' }, { status: 503 });
        }),
        transport: 'http',
        operationContracts: { read: operation },
      });
      try {
        await expect(
          client.upload(
            new ReadableStream({
              start(c) {
                c.close();
              },
            }),
            'image/png',
          ),
        ).rejects.toBeInstanceOf(ClientFrameworkError);
        expect(calls).toBe(1);
      } finally {
        client.dispose();
      }
      let refreshes = 0;
      const app = application();
      registerServiceClient(
        app,
        RawClient,
        {
          contract: {
            protocolVersion: 1,
            service: { id: 'raw', audience: 'raw' },
            credentials: { user: { kind: 'forwarded-user-token' } },
          },
          service: 'Raw',
          transport: 'http',
          operations: { read: { ...operation, security: { alternatives: [{ allOf: [{ profile: 'user' }] }] } } },
        },
        {
          url: serve(() => {
            calls++;
            return Response.json({ code: 'unauthorized' }, { status: 401 });
          }),
          allowInsecure: true,
          clientId: 'raw-consumer',
          credentials: {
            user: {
              source: 'forwarded-user',
              refresh: async () => {
                refreshes++;
                return 'fresh';
              },
            },
          },
        },
      );
      await app.start();
      try {
        await expect(
          runInContext({ __authorizationHeader: 'Bearer old' }, () =>
            app.context.get(RawClient).upload(new Uint8Array([1]), 'image/png'),
          ),
        ).rejects.toBeInstanceOf(ClientFrameworkError);
        expect(calls).toBe(2);
        expect(refreshes).toBe(0);
      } finally {
        await app.stop();
      }
    },
  );

  it('rejects stream cache policies before making a call', async () => {
    let calls = 0;
    const client = new RawClient({
      baseUrl: serve(() => {
        calls++;
        return new Response();
      }),
      transport: 'http',
      operationContracts: { read: { ...operation, resilience: { cache: { freshMs: 1000 } } } },
    });
    try {
      await expect(client.read()).rejects.toBeInstanceOf(ClientRequestEncodingError);
      expect(calls).toBe(0);
    } finally {
      client.dispose();
    }
  });
});
