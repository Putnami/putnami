import { afterEach, describe, expect, test } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import {
  api,
  apiKeyStrategy,
  application,
  authenticate,
  endpoint,
  http,
  openapi,
  SERVICE_WEBSOCKET_SUBPROTOCOL,
} from '@putnami/application';
import type { ClientContractOperation, ClientResiliencePolicy } from '@putnami/application';
import { NotFoundException, OneOf, resetConfigLoader, Stream } from '@putnami/runtime';
import { BaseClient } from '../../src/runtime/base-client';
import { CredentialManager, serviceAuthInterceptor } from '../../src/runtime/credential';
import { ClientFrameworkError } from '../../src/runtime/errors';
import { ServiceWebSocketTransport } from '../../src/runtime/service-ws-transport';
import { StreamSession } from '../../src/runtime/stream-session';
import type { DuplexStream, StreamObserver } from '../../src/runtime/stream.type';
import type { ClientRequest } from '../../src/runtime/transport.type';

const API_KEY = 'runtime-only-api-key';
const SECURITY = { alternatives: [{ allOf: [{ profile: 'service-key' }] }] } as const;

const running: (() => Promise<void>)[] = [];

afterEach(async () => {
  for (const stop of running.splice(0)) await stop();
  resetConfigLoader();
});

interface Provider {
  readonly baseUrl: string;
  readonly operations: Readonly<Record<string, ClientContractOperation>>;
  readonly defaults?: ClientResiliencePolicy;
}

/**
 * A real Putnami provider on a real port, with the three stream shapes.
 *
 * The client contracts handed to the consumer are read back from the document
 * the provider publishes, so the whole chain under test is
 * declare -> publish -> bind -> call, not a hand-written descriptor.
 */
async function startProvider(): Promise<Provider> {
  const providerHttp = http({ port: 0 });
  providerHttp.prepend(authenticate({ anyOf: [apiKeyStrategy({ keys: [API_KEY], header: 'X-Api-Key' })] }));
  const providerApi = api({
    autoScan: false,
    client: {
      service: { id: 'catalog.items', audience: 'api://widgets' },
      credentials: { 'service-key': { kind: 'api-key', header: 'X-Api-Key' } },
      defaults: { resilience: { stream: { idleTimeoutMs: 5000, maxBufferedMessages: 8 } } },
    },
  });
  providerApi.register(
    '/widgets/watch',
    endpoint()
      .returns(Stream({ id: String }))
      .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'safe' } })
      .handle(async (context) => {
        context.send({ id: 'one' });
        context.send({ id: 'two' });
      }),
    'GET',
  );
  providerApi.register(
    '/widgets/securewatch',
    endpoint()
      .returns(Stream({ client: String }))
      .secure({ principalKind: 'apikey' })
      .client({ security: SECURITY, idempotency: { kind: 'safe' } })
      .handle(async (context) => {
        context.send({ client: context.req.headers.get('X-Client-Id') ?? 'anonymous' });
      }),
    'GET',
  );
  providerApi.register(
    '/widgets/inbox',
    endpoint()
      .body(Stream({ id: String }))
      .returns({ received: Number })
      .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'idempotent' } })
      .handle(async (context) => {
        let received = 0;
        for await (const _message of context.messages()) received += 1;
        return { received };
      }),
    'GET',
  );
  providerApi.register(
    '/widgets/duplex',
    endpoint()
      .body(Stream({ id: String }))
      .returns(Stream({ echo: String }))
      .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'idempotent' } })
      .handle(async (context) => {
        for await (const message of context.messages()) context.send({ echo: (message as { id: string }).id });
        context.send({ echo: 'after-half-close' });
        return { echo: 'done' };
      }),
    'GET',
  );
  providerApi.register(
    '/widgets/failingwatch',
    endpoint()
      .returns(Stream({ id: String }))
      .mayThrow('NotFound')
      .throws(404, 'Missing widget stream', { code: OneOf('NotFound'), reason: String })
      .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'safe' } })
      .handle(async () => {
        throw new NotFoundException({ code: 'NotFound', reason: 'safe missing reason' });
      }),
    'GET',
  );
  const providerOpenApi = openapi({ title: 'Widgets', version: '1.0.0' });
  const app = application().use(providerHttp).use(providerApi).use(providerOpenApi);
  await app.start();
  running.push(async () => {
    await app.stop();
  });
  const port = providerHttp.getServer()?.port;
  const document = providerOpenApi.spec();
  if (!document) throw new Error('provider OpenAPI contract was not emitted');
  const operations: Record<string, ClientContractOperation> = {};
  for (const path of Object.values(document.paths ?? {})) {
    const get = (path as { get?: Record<string, unknown> }).get;
    const contract = get?.['x-putnami-client'] as ClientContractOperation | undefined;
    if (get && contract) operations[get['operationId'] as string] = contract;
  }
  const defaults = (document as { 'x-putnami-client'?: { defaults?: { resilience?: ClientResiliencePolicy } } })[
    'x-putnami-client'
  ]?.defaults?.resilience;
  return { baseUrl: `http://localhost:${port}`, operations, ...(defaults ? { defaults } : {}) };
}

class WidgetsClient extends BaseClient {
  readonly serviceName = 'catalog.items';

  watch(): StreamObserver<{ id: string }> {
    return this.serviceStream('GET', '/widgets/watch', { operationId: 'getWidgetsWatch' });
  }

  secureWatch(): StreamObserver<{ client: string }> {
    return this.serviceStream('GET', '/widgets/securewatch', { operationId: 'getWidgetsSecurewatch' });
  }

  failingWatch(): StreamObserver<{ id: string }> {
    return this.serviceStream('GET', '/widgets/failingwatch', { operationId: 'getWidgetsFailingwatch' });
  }

  inbox(): DuplexStream<{ id: string }, { received: number }> {
    return this.serviceClientStream('GET', '/widgets/inbox', { operationId: 'getWidgetsInbox' });
  }

  duplex(): DuplexStream<{ id: string }, { echo: string }> {
    return this.serviceBidiStream('GET', '/widgets/duplex', { operationId: 'getWidgetsDuplex' });
  }
}

function consumer(provider: Provider, secured = false): WidgetsClient {
  const identity = async (request: ClientRequest, next: (req: ClientRequest) => Promise<never>) => {
    request.headers.set('X-Client-Id', 'consumer');
    return next(request);
  };
  const authentication = serviceAuthInterceptor({
    serviceId: 'catalog.items',
    serviceUrl: provider.baseUrl,
    audience: 'api://widgets',
    clientId: 'consumer',
    profiles: { 'service-key': { kind: 'api-key', header: 'X-Api-Key' } },
    credentials: { 'service-key': { source: 'static', value: API_KEY } },
    manager: new CredentialManager(),
  });
  return new WidgetsClient({
    baseUrl: provider.baseUrl,
    transport: 'http',
    serviceId: 'catalog.items',
    operationContracts: provider.operations,
    ...(provider.defaults ? { clientDefaults: provider.defaults } : {}),
    streamInterceptors: secured ? [identity as never, authentication] : [identity as never],
  });
}

function collect<T>(stream: StreamObserver<T>): Promise<T[]> {
  return new Promise((resolve, reject) => {
    const values: T[] = [];
    stream.onMessage((value) => values.push(value));
    stream.onError(reject);
    stream.onComplete(() => resolve(values));
  });
}

describe('TypeScript provider to TypeScript consumer, in process, on a real port', () => {
  test('publishes both stream transports for a server stream, websocket second, with the subprotocol', async () => {
    const provider = await startProvider();
    const watch = provider.operations['getWidgetsWatch'];
    expect(watch?.stream).toBe('server');
    expect(watch?.transports.map((transport) => transport.protocol)).toEqual(['sse', 'websocket']);
    expect(watch?.transports[1]?.websocket).toEqual({ subprotocol: SERVICE_WEBSOCKET_SUBPROTOCOL, resume: false });
    expect(provider.operations['getWidgetsInbox']?.transports.map((t) => t.protocol)).toEqual(['websocket']);
    expect(provider.operations['getWidgetsDuplex']?.transports.map((t) => t.protocol)).toEqual(['websocket']);
  });

  test('carries a server stream over the transport the provider declared first', async () => {
    const provider = await startProvider();
    using client = consumer(provider);
    expect(await collect(client.watch())).toEqual([{ id: 'one' }, { id: 'two' }]);
  });

  specTest(
    'carries the same server stream over websocket, against the same real provider',
    {
      feature: 'typescript/service-clients',
      requirement: 'websocket-streams',
      check: 'a-server-stream-reaches-a-real-provider-over-websocket',
    },
    async () => {
      const provider = await startProvider();
      const watch = provider.operations['getWidgetsWatch'] as ClientContractOperation;
      const session = new StreamSession<{ id: string }>({
        serviceId: 'catalog.items',
        operationId: 'getWidgetsWatch',
        protocol: 'websocket',
        budgets: {
          handshakeMs: 4000,
          idleMs: 4000,
          sessionMs: 0,
          heartbeatMs: 0,
          maxFrameBytes: 65_536,
          maxBufferedMessages: 8,
        },
      });
      const stream = new ServiceWebSocketTransport(provider.baseUrl, 'catalog.items').stream<{ id: string }>(
        {
          method: 'GET',
          path: '/widgets/watch',
          headers: new Headers({ 'X-Client-Id': 'consumer' }),
          operationId: 'getWidgetsWatch',
          clientOperation: watch,
        },
        { stream: 'server', output: watch.messages?.output, resumeDeclared: false },
        session,
      );
      expect(await collect(stream)).toEqual([{ id: 'one' }, { id: 'two' }]);
    },
  );

  specTest(
    'carries a client stream to its single typed result',
    {
      feature: 'typescript/service-clients',
      requirement: 'websocket-streams',
      check: 'a-client-stream-reaches-a-real-provider',
    },
    async () => {
      const provider = await startProvider();
      using client = consumer(provider);
      const inbox = client.inbox();
      const result = collect(inbox);
      inbox.send({ id: 'a' });
      inbox.send({ id: 'b' });
      inbox.send({ id: 'c' });
      inbox.end();
      expect(await result).toEqual([{ received: 3 }]);
    },
  );

  specTest(
    'carries a bidirectional stream past the half-close to its terminal value',
    {
      feature: 'typescript/service-clients',
      requirement: 'websocket-streams',
      check: 'a-bidirectional-stream-reaches-a-real-provider',
    },
    async () => {
      const provider = await startProvider();
      using client = consumer(provider);
      const duplex = client.duplex();
      const values = collect(duplex);
      duplex.send({ id: 'a' });
      duplex.send({ id: 'b' });
      await Bun.sleep(30);
      duplex.end();
      expect(await values).toEqual([{ echo: 'a' }, { echo: 'b' }, { echo: 'after-half-close' }, { echo: 'done' }]);
    },
  );

  test('carries the declared credential in the init frame, and the provider reads it back as a header', async () => {
    const provider = await startProvider();
    using client = consumer(provider, true);
    expect(await collect(client.secureWatch())).toEqual([{ client: 'consumer' }]);
  });

  test('opens no socket at all when no configured credential satisfies the declared alternative', async () => {
    const provider = await startProvider();
    const unbound = new WidgetsClient({
      baseUrl: provider.baseUrl,
      transport: 'http',
      serviceId: 'catalog.items',
      operationContracts: provider.operations,
      streamInterceptors: [
        serviceAuthInterceptor({
          serviceId: 'catalog.items',
          serviceUrl: provider.baseUrl,
          audience: 'api://widgets',
          clientId: 'consumer',
          profiles: { 'service-key': { kind: 'api-key', header: 'X-Api-Key' } },
          credentials: {},
          manager: new CredentialManager(),
        }),
      ],
    });
    try {
      const failure = await collect(unbound.secureWatch()).catch((error: unknown) => error);
      expect((failure as { code?: string }).code).toBe('client.credential');
    } finally {
      unbound.dispose();
    }
  });

  test('narrows a declared handler error to its typed identity over the declared transport', async () => {
    const provider = await startProvider();
    using client = consumer(provider);
    const failure = (await collect(client.failingWatch()).catch((error: unknown) => error)) as ClientFrameworkError;
    expect(failure).toBeInstanceOf(ClientFrameworkError);
    expect(failure.status).toBe(404);
    expect(failure.code).toBe('not_found');
    expect(failure.details).toEqual({ code: 'NotFound', reason: 'safe missing reason' });
  });

  test('opens the socket the way a browser must, and puts nothing sensitive on the wire', async () => {
    const provider = await startProvider();
    // The consumer path is the one under test above; this asserts the shape a
    // browser is limited to — no header, no URL credential, one subprotocol.
    const socket = new WebSocket(
      `${provider.baseUrl.replace('http', 'ws')}/widgets/securewatch`,
      SERVICE_WEBSOCKET_SUBPROTOCOL,
    );
    const frames: Record<string, unknown>[] = [];
    socket.addEventListener('message', (event) => frames.push(JSON.parse(String(event.data))));
    await new Promise<void>((resolve, reject) => {
      socket.addEventListener('open', () => resolve(), { once: true });
      socket.addEventListener('error', () => reject(new Error('upgrade failed')), { once: true });
    });
    socket.send(
      JSON.stringify({
        v: 1,
        type: 'init',
        operationId: 'getWidgetsSecurewatch',
        clientId: 'browser',
        deadlineUnixMs: '0',
        budgetMs: '0',
        credentials: [{ profile: 'service-key', value: API_KEY }],
        headers: [],
      }),
    );
    await Bun.sleep(120);
    socket.close();
    expect(frames[0]).toEqual({ v: 1, type: 'ready', resumed: false });
    expect(frames.some((frame) => frame['type'] === 'message')).toBe(true);
  });
});

describe('Go and TypeScript refuse the same corpus scenes with the same code', () => {
  /** The client-composable invalid scenes N3 drove against the Go provider. */
  const scenes: { name: string; operationId: string; frame: Record<string, unknown>; code: string }[] = [
    {
      name: 'cancel-before-init.json',
      operationId: 'getWidgetsWatch',
      frame: { v: 1, type: 'cancel', code: 'canceled' },
      code: 'client_contract.invalid_transport',
    },
    {
      name: 'data-before-ready.json',
      operationId: 'getWidgetsWatch',
      frame: { v: 1, type: 'message', sequence: '1', payload: { encoding: 'json', value: { id: 'early' } } },
      code: 'client_contract.invalid_transport',
    },
    {
      name: 'client-resume.json',
      operationId: 'getWidgetsInbox',
      frame: {
        v: 1,
        type: 'init',
        operationId: 'getWidgetsInbox',
        clientId: 'fixtures-consumer',
        deadlineUnixMs: '0',
        budgetMs: '0',
        credentials: [],
        headers: [],
        resume: { token: 'fixture-resume-token-1', afterSequence: '4' },
      },
      code: 'client_contract.invalid_resilience',
    },
    {
      name: 'secret-shaped-unknown-field.json',
      operationId: 'getWidgetsWatch',
      frame: {
        v: 1,
        type: 'init',
        operationId: 'getWidgetsWatch',
        clientId: 'fixtures-consumer',
        deadlineUnixMs: '0',
        budgetMs: '0',
        credentials: [],
        headers: [],
        'super-secret-frame-field': 'must-not-appear-in-diagnostics',
      },
      code: 'client_contract.parse_error',
    },
  ];

  test.each(scenes)('$name is refused with $code', async (item) => {
    const provider = await startProvider();
    const route = item.operationId === 'getWidgetsInbox' ? '/widgets/inbox' : '/widgets/watch';
    const socket = new WebSocket(`${provider.baseUrl.replace('http', 'ws')}${route}`, SERVICE_WEBSOCKET_SUBPROTOCOL);
    const frames: Record<string, unknown>[] = [];
    socket.addEventListener('message', (event) => frames.push(JSON.parse(String(event.data))));
    await new Promise<void>((resolve, reject) => {
      socket.addEventListener('open', () => resolve(), { once: true });
      socket.addEventListener('error', () => reject(new Error('upgrade failed')), { once: true });
    });
    socket.send(JSON.stringify(item.frame));
    await Bun.sleep(120);
    socket.close();
    expect(frames[0]).toMatchObject({ type: 'error', error: { status: 400, code: item.code } });
    expect(JSON.stringify(frames)).not.toContain('must-not-appear-in-diagnostics');
  });
});
