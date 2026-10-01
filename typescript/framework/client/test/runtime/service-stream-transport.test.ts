import { afterEach, describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import type {
  ClientContractOperation,
  ClientSchema,
  ClientTransportContract,
  ClientTransportProtocol,
} from '@putnami/application';
import { SERVICE_WEBSOCKET_SUBPROTOCOL } from '@putnami/application';
import { BaseClient } from '../../src/runtime/base-client';
import type { StreamObserver } from '../../src/runtime/stream.type';

const WATCH_PATH = '/items/watch';

const output = {
  type: 'object',
  properties: { value: { type: 'string' } },
  required: ['value'],
  additionalProperties: false,
} as const satisfies ClientSchema;

interface WatchMessage {
  readonly value: string;
}

// ---------------------------------------------------------------------------
// A provider that answers both declared halves of one path
// ---------------------------------------------------------------------------

interface DualProvider {
  readonly url: string;
  /** How many ordinary HTTP requests reached the SSE half. */
  sse(): number;
  /** How many upgrade requests reached the WebSocket half. */
  upgrades(): number;
  /** Every init frame the WebSocket half read, in connection order. */
  inits(): Record<string, unknown>[];
  stop(): void;
}

interface DualProviderOptions {
  /** Answers the SSE half with an ordinary status. Unset streams the events. */
  readonly sseStatus?: number;
  /** The scripted SSE stream. */
  readonly sseEvents?: string;
  /** Refuses the upgrade with an ordinary status. Unset completes the handshake. */
  readonly upgradeStatus?: number;
  /** Overrides the echoed subprotocol. An empty string echoes nothing at all. */
  readonly subprotocol?: string;
  /** Scripts one framed conversation, `connection` counting from 1. */
  readonly play?: (
    frame: Record<string, unknown>,
    send: (frame: unknown) => void,
    close: () => void,
    connection: number,
  ) => void;
}

const servers: (() => void)[] = [];

afterEach(() => {
  for (const stop of servers.splice(0)) stop();
});

function dualProvider(options: DualProviderOptions): DualProvider {
  let sseCount = 0;
  let upgradeCount = 0;
  const inits: Record<string, unknown>[] = [];
  const echoed = options.subprotocol === undefined ? SERVICE_WEBSOCKET_SUBPROTOCOL : options.subprotocol;
  const server = Bun.serve<{ connection: number }, Record<string, never>>({
    port: 0,
    fetch(request, target) {
      if (request.headers.get('upgrade')?.toLowerCase() !== 'websocket') {
        sseCount += 1;
        if (options.sseStatus !== undefined) {
          return Response.json({ code: 'not_found', message: 'no' }, { status: options.sseStatus });
        }
        return new Response(options.sseEvents ?? '', { headers: { 'Content-Type': 'text/event-stream' } });
      }
      upgradeCount += 1;
      if (options.upgradeStatus !== undefined) {
        return Response.json({ code: 'not_found', message: 'no' }, { status: options.upgradeStatus });
      }
      const upgraded = target.upgrade(request, {
        data: { connection: upgradeCount },
        ...(echoed ? { headers: { 'Sec-WebSocket-Protocol': echoed } } : {}),
      });
      return upgraded ? undefined : new Response('expected a websocket upgrade', { status: 400 });
    },
    websocket: {
      message(socket, message) {
        const frame = JSON.parse(String(message)) as Record<string, unknown>;
        if (frame['type'] === 'init') inits.push(frame);
        options.play?.(
          frame,
          (out) => socket.send(JSON.stringify(out)),
          () => socket.close(),
          socket.data.connection,
        );
      },
    },
  });
  const provider: DualProvider = {
    url: `http://localhost:${server.port}`,
    sse: () => sseCount,
    upgrades: () => upgradeCount,
    inits: () => inits,
    stop: () => server.stop(true),
  };
  servers.push(provider.stop);
  return provider;
}

/** Answers admission and delivers the scripted values over the WebSocket half. */
function playServerStream(...values: string[]): DualProviderOptions['play'] {
  return (frame, send) => {
    if (frame['type'] !== 'init') return;
    send({ v: 1, type: 'ready', resumed: false });
    values.forEach((value, index) => {
      send({
        v: 1,
        type: 'message',
        sequence: String(index + 1),
        payload: { encoding: 'json', value: { value } },
      });
    });
    send({ v: 1, type: 'result' });
  };
}

// ---------------------------------------------------------------------------
// A generated client that declares its transports and nothing else
// ---------------------------------------------------------------------------

function transport(protocol: ClientTransportProtocol, resume = false): ClientTransportContract {
  if (protocol === 'websocket') {
    return {
      protocol,
      path: WATCH_PATH,
      encoding: 'json',
      websocket: { subprotocol: SERVICE_WEBSOCKET_SUBPROTOCOL, resume },
    };
  }
  return { protocol, path: WATCH_PATH, encoding: 'json' };
}

function watchOperation(options: {
  readonly order: readonly ClientTransportProtocol[];
  readonly idempotency?: 'safe' | 'idempotent' | 'non-idempotent';
  readonly resume?: boolean;
  readonly reconnect?: boolean;
}): ClientContractOperation {
  return {
    stream: 'server',
    messages: { output },
    transports: options.order.map((protocol) => transport(protocol, options.resume ?? false)),
    security: { alternatives: [{ allOf: [] }] },
    errors: [],
    idempotency: { kind: options.idempotency ?? 'safe' },
    ...(options.reconnect === undefined ? {} : { resilience: { stream: { reconnect: options.reconnect } } }),
  };
}

class WatchClient extends BaseClient {
  readonly serviceName = 'catalog.items';

  watch(): StreamObserver<WatchMessage> {
    return this.serviceStream<WatchMessage>('GET', WATCH_PATH, { operationId: 'watchItems' });
  }
}

function watchClient(url: string, operation: ClientContractOperation): WatchClient {
  return new WatchClient({
    baseUrl: url,
    transport: 'http',
    serviceId: 'catalog.items',
    operationContracts: { watchItems: operation },
    // The identity a generated client carries is injected by its auth
    // interceptor, on the same chain a stream handshake runs.
    streamInterceptors: [
      async (request, next) => {
        request.headers.set('X-Client-Id', 'consumer.workload');
        return next(request);
      },
    ],
  });
}

function collect(stream: StreamObserver<WatchMessage>): Promise<{ values: string[]; failure?: unknown }> {
  return new Promise((resolve) => {
    const values: string[] = [];
    stream.onMessage((message) => values.push(message.value));
    stream.onError((failure) => resolve({ values, failure }));
    stream.onComplete(() => resolve({ values }));
  });
}

// ---------------------------------------------------------------------------

describe('a generated server stream follows the declared transport order', () => {
  specTest(
    'opens the first transport the provider declared, whichever it is',
    {
      feature: 'typescript/service-clients',
      requirement: 'declared-transport-preference',
      check: 'the-declared-order-is-the-dispatch-order',
    },
    async () => {
      const cases = [
        { order: ['sse', 'websocket'] as const, sse: 1, upgrades: 0 },
        { order: ['websocket', 'sse'] as const, sse: 0, upgrades: 1 },
      ];
      for (const testCase of cases) {
        const provider = dualProvider({
          sseEvents: 'data: {"value":"one"}\n\n',
          play: playServerStream('one'),
        });
        const client = watchClient(provider.url, watchOperation({ order: testCase.order }));
        // biome-ignore lint/performance/noAwaitInLoops: each case owns a port
        const outcome = await collect(client.watch());
        expect({ order: testCase.order, ...outcome }).toEqual({ order: testCase.order, values: ['one'] });
        expect({ order: testCase.order, sse: provider.sse(), upgrades: provider.upgrades() }).toEqual({
          order: testCase.order,
          sse: testCase.sse,
          upgrades: testCase.upgrades,
        });
      }
    },
  );

  specTest(
    'falls back when the provider says the first declared wire is not served here',
    {
      feature: 'typescript/service-clients',
      requirement: 'declared-transport-preference',
      check: 'a-transport-the-provider-does-not-serve-falls-back-to-the-next-declared-one',
    },
    async () => {
      for (const status of [404, 405, 426]) {
        const provider = dualProvider({ sseStatus: status, play: playServerStream('one', 'two') });
        const client = watchClient(provider.url, watchOperation({ order: ['sse', 'websocket'] }));
        // biome-ignore lint/performance/noAwaitInLoops: each status owns a port
        const outcome = await collect(client.watch());
        expect({ status, ...outcome }).toEqual({ status, values: ['one', 'two'] });
        expect({ status, sse: provider.sse(), upgrades: provider.upgrades() }).toEqual({
          status,
          sse: 1,
          upgrades: 1,
        });
      }
    },
  );

  specTest(
    'surfaces an answer about the call instead of opening the next wire',
    {
      feature: 'typescript/service-clients',
      requirement: 'declared-transport-preference',
      check: 'an-answer-about-the-call-is-surfaced-instead-of-trying-another-wire',
    },
    async () => {
      for (const status of [401, 403, 429, 500]) {
        const provider = dualProvider({ sseStatus: status, play: playServerStream('one') });
        const client = watchClient(provider.url, watchOperation({ order: ['sse', 'websocket'] }));
        // biome-ignore lint/performance/noAwaitInLoops: each status owns a port
        const outcome = await collect(client.watch());
        expect({ status, failed: outcome.failure !== undefined }).toEqual({ status, failed: true });
        expect({ status, sse: provider.sse(), upgrades: provider.upgrades() }).toEqual({
          status,
          sse: 1,
          upgrades: 0,
        });
      }
    },
  );

  specTest(
    'never reopens an operation the provider did not declare replayable',
    {
      feature: 'typescript/service-clients',
      requirement: 'declared-transport-preference',
      check: 'an-operation-that-is-not-declared-replayable-is-never-opened-twice',
    },
    async () => {
      const provider = dualProvider({ sseStatus: 404, play: playServerStream('one') });
      const client = watchClient(
        provider.url,
        watchOperation({ order: ['sse', 'websocket'], idempotency: 'non-idempotent' }),
      );
      const outcome = await collect(client.watch());
      expect(outcome.failure).toBeDefined();
      expect({ sse: provider.sse(), upgrades: provider.upgrades() }).toEqual({ sse: 1, upgrades: 0 });
    },
  );

  specTest(
    'falls back when the provider completes the handshake without the first-party wire',
    {
      feature: 'typescript/service-clients',
      requirement: 'declared-transport-preference',
      check: 'a-provider-that-does-not-negotiate-the-first-party-wire-falls-back',
    },
    async () => {
      // Bun reports the requested subprotocol rather than the selected one, so
      // the stub reports what a browser reports.
      const original = globalThis.WebSocket;
      globalThis.WebSocket = class extends original {
        override get protocol(): string {
          return '';
        }
      } as unknown as typeof WebSocket;
      try {
        const provider = dualProvider({
          sseEvents: 'data: {"value":"one"}\n\n',
          subprotocol: '',
        });
        const client = watchClient(provider.url, watchOperation({ order: ['websocket', 'sse'] }));
        const outcome = await collect(client.watch());
        expect(outcome).toEqual({ values: ['one'] });
        expect({ sse: provider.sse(), upgrades: provider.upgrades() }).toEqual({ sse: 1, upgrades: 1 });
      } finally {
        globalThis.WebSocket = original;
      }
    },
  );

  specTest(
    'surfaces the last declared transport answer when every declared wire is absent',
    {
      feature: 'typescript/service-clients',
      requirement: 'declared-transport-preference',
      check: 'an-exhausted-fallback-surfaces-the-answer-of-the-last-declared-transport',
    },
    async () => {
      const provider = dualProvider({ sseStatus: 404, upgradeStatus: 404 });
      const client = watchClient(provider.url, watchOperation({ order: ['sse', 'websocket'] }));
      const outcome = await collect(client.watch());
      expect(outcome.failure).toBeDefined();
      expect({ sse: provider.sse(), upgrades: provider.upgrades() }).toEqual({ sse: 1, upgrades: 1 });
    },
  );

  specTest(
    'never falls back once a message has been delivered',
    {
      feature: 'typescript/service-clients',
      requirement: 'declared-transport-preference',
      check: 'a-break-after-admission-never-opens-another-declared-transport',
    },
    async () => {
      const provider = dualProvider({
        sseEvents: 'data: {"value":"sse"}\n\n',
        play: (frame, send, close) => {
          if (frame['type'] !== 'init') return;
          send({ v: 1, type: 'ready', resumed: false });
          send({ v: 1, type: 'message', sequence: '1', payload: { encoding: 'json', value: { value: 'one' } } });
          // The socket ends without a terminal frame. Resume is not declared,
          // so this is where the stream stops.
          setTimeout(close, 5);
        },
      });
      const client = watchClient(provider.url, watchOperation({ order: ['websocket', 'sse'] }));
      const outcome = await collect(client.watch());
      expect(outcome.values).toEqual(['one']);
      expect(outcome.failure).toBeDefined();
      expect({ sse: provider.sse(), upgrades: provider.upgrades() }).toEqual({ sse: 0, upgrades: 1 });
    },
  );

  specTest(
    'keeps working instead of opening the circuit when a declared wire is permanently absent',
    {
      feature: 'typescript/service-clients',
      requirement: 'declared-transport-preference',
      check: 'a-repeated-fallback-does-not-open-the-circuit-for-the-operation',
    },
    async () => {
      const provider = dualProvider({ sseEvents: 'data: {"value":"one"}\n\n', sseStatus: undefined });
      const client = watchClient(provider.url, watchOperation({ order: ['sse', 'websocket'] }));
      // A provider that serves SSE but not the WebSocket half of the path. The
      // framework circuit opens after five consecutive failures, so six
      // fallbacks in a row is past the threshold a silent declaration holds.
      const absent = dualProvider({ sseStatus: 404, play: playServerStream('one') });
      const falling = watchClient(absent.url, watchOperation({ order: ['sse', 'websocket'] }));
      for (let attempt = 0; attempt < 6; attempt += 1) {
        // biome-ignore lint/performance/noAwaitInLoops: the attempts are consecutive by construction
        const outcome = await collect(falling.watch());
        expect({ attempt, ...outcome }).toEqual({ attempt, values: ['one'] });
      }
      expect({ sse: absent.sse(), upgrades: absent.upgrades() }).toEqual({ sse: 6, upgrades: 6 });
      // The client that never had to fall back is unaffected either way.
      expect(await collect(client.watch())).toEqual({ values: ['one'] });
    },
  );

  specTest(
    'refuses a declaration with no carriable transport before any socket opens',
    {
      feature: 'typescript/service-clients',
      requirement: 'declared-transport-preference',
      check: 'a-declaration-with-no-carriable-transport-is-refused-before-any-socket',
    },
    async () => {
      const provider = dualProvider({});
      const client = watchClient(provider.url, watchOperation({ order: ['rest-json'] }));
      const outcome = await collect(client.watch());
      expect(outcome.failure).toBeDefined();
      expect({ sse: provider.sse(), upgrades: provider.upgrades() }).toEqual({ sse: 0, upgrades: 0 });
    },
  );
});
