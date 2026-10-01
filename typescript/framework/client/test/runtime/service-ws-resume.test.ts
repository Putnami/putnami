import { afterEach, describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import type { ClientContractOperation, ClientSchema } from '@putnami/application';
import { SERVICE_WEBSOCKET_SUBPROTOCOL } from '@putnami/application';
import { ClientCredentialError } from '../../src/runtime/errors';
import { MAX_STREAM_RESUME_ATTEMPTS, ServiceWebSocketTransport } from '../../src/runtime/service-ws-transport';
import { StreamSession } from '../../src/runtime/stream-session';
import type { StreamObserver } from '../../src/runtime/stream.type';
import type { ClientRequest } from '../../src/runtime/transport.type';

const output = {
  type: 'object',
  properties: { value: { type: 'string' } },
  required: ['value'],
  additionalProperties: false,
} as const satisfies ClientSchema;

interface WatchMessage {
  readonly value: string;
}

interface ResumeProvider {
  readonly url: string;
  /** Every init frame the provider read, in connection order. */
  readonly inits: Record<string, unknown>[];
  connections(): number;
  stop(): void;
}

const servers: (() => void)[] = [];

afterEach(() => {
  for (const stop of servers.splice(0)) stop();
});

/**
 * A provider that serves several connections of one stream, so a break and its
 * continuation are observable as separate sockets.
 */
function resumeProvider(
  play: (send: (frame: unknown) => void, close: () => void, connection: number) => void,
): ResumeProvider {
  const inits: Record<string, unknown>[] = [];
  let connections = 0;
  const server = Bun.serve<{ connection: number }, Record<string, never>>({
    port: 0,
    fetch(request, target) {
      connections += 1;
      const upgraded = target.upgrade(request, {
        data: { connection: connections },
        headers: { 'Sec-WebSocket-Protocol': SERVICE_WEBSOCKET_SUBPROTOCOL },
      });
      return upgraded ? undefined : new Response('expected a websocket upgrade', { status: 400 });
    },
    websocket: {
      message(socket, message) {
        const frame = JSON.parse(String(message)) as Record<string, unknown>;
        if (frame['type'] !== 'init') return;
        inits.push(frame);
        play(
          (out) => socket.send(JSON.stringify(out)),
          () => socket.close(),
          socket.data.connection,
        );
      },
    },
  });
  const provider: ResumeProvider = {
    url: `http://localhost:${server.port}/watch`,
    inits,
    connections: () => connections,
    stop: () => server.stop(true),
  };
  servers.push(provider.stop);
  return provider;
}

function message(sequence: number, value: string): Record<string, unknown> {
  return { v: 1, type: 'message', sequence: String(sequence), payload: { encoding: 'json', value: { value } } };
}

function operationContract(): ClientContractOperation {
  return {
    stream: 'server',
    messages: { output },
    transports: [
      {
        protocol: 'websocket',
        path: '/watch',
        encoding: 'json',
        websocket: { subprotocol: SERVICE_WEBSOCKET_SUBPROTOCOL, resume: true },
      },
    ],
    security: { alternatives: [{ allOf: [] }] },
    errors: [],
    idempotency: { kind: 'safe' },
    resilience: { stream: { reconnect: true } },
  };
}

function request(): ClientRequest {
  return {
    method: 'GET',
    path: '/watch',
    headers: new Headers({ 'X-Client-Id': 'fixtures-consumer' }),
    operationId: 'watchItems',
    clientOperation: operationContract(),
  };
}

function session(options: { credentials?: () => Promise<void> } = {}): StreamSession<WatchMessage> {
  return new StreamSession<WatchMessage>({
    serviceId: 'catalog.items',
    operationId: 'watchItems',
    protocol: 'websocket',
    budgets: {
      handshakeMs: 2000,
      idleMs: 2000,
      sessionMs: 0,
      heartbeatMs: 0,
      maxFrameBytes: 4096,
      maxBufferedMessages: 32,
    },
    ...(options.credentials ? { credentials: options.credentials } : {}),
  });
}

function open(
  provider: ResumeProvider,
  options: { resumeDeclared: boolean; reconnectDeclared: boolean; credentials?: () => Promise<void> },
): Promise<{ values: string[]; failure?: unknown }> {
  const stream = new ServiceWebSocketTransport(provider.url, 'catalog.items').stream<WatchMessage>(
    request(),
    {
      stream: 'server',
      output,
      resumeDeclared: options.resumeDeclared,
      reconnectDeclared: options.reconnectDeclared,
    },
    session(options.credentials ? { credentials: options.credentials } : {}),
  );
  return collect(stream);
}

function collect(stream: StreamObserver<WatchMessage>): Promise<{ values: string[]; failure?: unknown }> {
  return new Promise((resolve) => {
    const values: string[] = [];
    stream.onMessage((value) => values.push(value.value));
    stream.onError((failure) => resolve({ values, failure }));
    stream.onComplete(() => resolve({ values }));
  });
}

function resumeOf(frame: Record<string, unknown> | undefined): { token: string; afterSequence: string } | undefined {
  return frame?.['resume'] as { token: string; afterSequence: string } | undefined;
}

describe('a declared server stream continues over a new socket', () => {
  specTest(
    'continues after the last sequence the caller received, with no gap and no duplicate',
    {
      feature: 'typescript/service-clients',
      requirement: 'declared-stream-resume',
      check: 'a-declared-server-stream-continues-after-the-last-sequence-the-caller-received',
    },
    async () => {
      const provider = resumeProvider((send, close, connection) => {
        if (connection === 1) {
          send({ v: 1, type: 'ready', resumed: false, resumeToken: 'grant-1' });
          send(message(1, 'one'));
          send(message(2, 'two'));
          setTimeout(close, 5);
          return;
        }
        send({ v: 1, type: 'ready', resumed: true, resumeToken: 'grant-2' });
        send(message(3, 'three'));
        send({ v: 1, type: 'result' });
      });
      const outcome = await open(provider, { resumeDeclared: true, reconnectDeclared: true });
      expect(outcome).toEqual({ values: ['one', 'two', 'three'] });
      expect(resumeOf(provider.inits[1])).toEqual({ token: 'grant-1', afterSequence: '2' });
    },
  );

  specTest(
    'presents the token the previous connection issued, never the one before it',
    {
      feature: 'typescript/service-clients',
      requirement: 'declared-stream-resume',
      check: 'each-continuation-presents-the-token-the-previous-connection-issued',
    },
    async () => {
      const provider = resumeProvider((send, close, connection) => {
        if (connection <= 2) {
          send({ v: 1, type: 'ready', resumed: connection > 1, resumeToken: `grant-${connection}` });
          send(message(connection, `v${connection}`));
          setTimeout(close, 5);
          return;
        }
        send({ v: 1, type: 'ready', resumed: true, resumeToken: 'grant-3' });
        send({ v: 1, type: 'result' });
      });
      const outcome = await open(provider, { resumeDeclared: true, reconnectDeclared: true });
      expect(outcome).toEqual({ values: ['v1', 'v2'] });
      expect(resumeOf(provider.inits[1])).toEqual({ token: 'grant-1', afterSequence: '1' });
      expect(resumeOf(provider.inits[2])).toEqual({ token: 'grant-2', afterSequence: '2' });
    },
  );

  specTest(
    'ends at the break when nobody declared the stream resumable',
    {
      feature: 'typescript/service-clients',
      requirement: 'declared-stream-resume',
      check: 'a-stream-that-was-not-declared-resumable-ends-at-the-break',
    },
    async () => {
      const cases = [
        { resumeDeclared: true, reconnectDeclared: false },
        { resumeDeclared: false, reconnectDeclared: false },
      ];
      for (const declaration of cases) {
        const provider = resumeProvider((send, close) => {
          send({ v: 1, type: 'ready', resumed: false, resumeToken: 'grant-1' });
          send(message(1, 'one'));
          setTimeout(close, 5);
        });
        // biome-ignore lint/performance/noAwaitInLoops: each case owns a port
        const outcome = await open(provider, declaration);
        expect({ ...declaration, values: outcome.values }).toEqual({ ...declaration, values: ['one'] });
        expect({ ...declaration, failed: outcome.failure !== undefined }).toEqual({ ...declaration, failed: true });
        expect({ ...declaration, connections: provider.connections() }).toEqual({ ...declaration, connections: 1 });
      }
    },
  );

  specTest(
    'refuses a provider that answers a continuation with a fresh stream',
    {
      feature: 'typescript/service-clients',
      requirement: 'declared-stream-resume',
      check: 'a-provider-that-answers-a-continuation-with-a-fresh-stream-is-refused',
    },
    async () => {
      const provider = resumeProvider((send, close, connection) => {
        if (connection === 1) {
          send({ v: 1, type: 'ready', resumed: false, resumeToken: 'grant-1' });
          send(message(1, 'one'));
          setTimeout(close, 5);
          return;
        }
        send({ v: 1, type: 'ready', resumed: false, resumeToken: 'grant-2' });
        send(message(1, 'one'));
      });
      const outcome = await open(provider, { resumeDeclared: true, reconnectDeclared: true });
      // Nothing is delivered twice: the runtime refuses the answer instead of
      // consuming it.
      expect(outcome.values).toEqual(['one']);
      expect((outcome.failure as Error).message).toContain('refused to continue the stream');
    },
  );

  specTest(
    'ends the stream with a typed terminal when the provider no longer honors the token',
    {
      feature: 'typescript/service-clients',
      requirement: 'declared-stream-resume',
      check: 'a-token-the-provider-no-longer-honors-ends-the-stream-with-a-typed-terminal',
    },
    async () => {
      const provider = resumeProvider((send, close, connection) => {
        if (connection === 1) {
          send({ v: 1, type: 'ready', resumed: false, resumeToken: 'grant-1' });
          send(message(1, 'one'));
          setTimeout(close, 5);
          return;
        }
        send({
          v: 1,
          type: 'error',
          error: {
            status: 400,
            code: 'client_contract.invalid_resilience',
            message: 'resume token is unknown, expired or already spent',
          },
        });
      });
      const outcome = await open(provider, { resumeDeclared: true, reconnectDeclared: true });
      expect(outcome.values).toEqual(['one']);
      expect(outcome.failure).toBeDefined();
      expect(provider.connections()).toBe(2);
    },
  );

  specTest(
    'stops at the framework resume bound when the socket keeps breaking',
    {
      feature: 'typescript/service-clients',
      requirement: 'declared-stream-resume',
      check: 'a-stream-that-keeps-breaking-stops-at-the-framework-resume-bound',
    },
    async () => {
      const provider = resumeProvider((send, close, connection) => {
        send({ v: 1, type: 'ready', resumed: connection > 1, resumeToken: `grant-${connection}` });
        send(message(connection, `v${connection}`));
        setTimeout(close, 5);
      });
      const outcome = await open(provider, { resumeDeclared: true, reconnectDeclared: true });
      expect(outcome.failure).toBeDefined();
      expect(outcome.values).toHaveLength(MAX_STREAM_RESUME_ATTEMPTS + 1);
      expect(provider.connections()).toBe(MAX_STREAM_RESUME_ATTEMPTS + 1);
    },
  );

  specTest(
    're-resolves the credentials the continuation carries before it dials',
    {
      feature: 'typescript/service-clients',
      requirement: 'declared-stream-resume',
      check: 'a-continuation-carries-a-credential-renewed-at-the-instant-it-dials',
    },
    async () => {
      let resolutions = 0;
      const provider = resumeProvider((send, close, connection) => {
        if (connection === 1) {
          send({ v: 1, type: 'ready', resumed: false, resumeToken: 'grant-1' });
          send(message(1, 'one'));
          setTimeout(close, 5);
          return;
        }
        send({ v: 1, type: 'ready', resumed: true, resumeToken: 'grant-2' });
        send({ v: 1, type: 'result' });
      });
      const outcome = await open(provider, {
        resumeDeclared: true,
        reconnectDeclared: true,
        credentials: async () => {
          resolutions += 1;
        },
      });
      expect(outcome).toEqual({ values: ['one'] });
      // The first socket is handed credentials the client already resolved, so
      // every resolution counted here belongs to the continuation. The session
      // resolves twice — the acquisition and the send-point re-check — which is
      // exactly what the lifecycle contract asks for before a frame leaves.
      expect(resolutions).toBe(2);
    },
  );

  specTest(
    'ends the session before admission when the credentials cannot be renewed',
    {
      feature: 'typescript/service-clients',
      requirement: 'declared-stream-resume',
      check: 'a-continuation-that-cannot-renew-its-credential-ends-before-admission',
    },
    async () => {
      const provider = resumeProvider((send, close) => {
        send({ v: 1, type: 'ready', resumed: false, resumeToken: 'grant-1' });
        send(message(1, 'one'));
        setTimeout(close, 5);
      });
      // The first socket carries credentials the client already resolved, so
      // this only ever runs for the continuation.
      const outcome = await open(provider, {
        resumeDeclared: true,
        reconnectDeclared: true,
        credentials: async () => {
          throw new ClientCredentialError('service credential expired');
        },
      });
      expect(outcome.values).toEqual(['one']);
      expect(outcome.failure).toBeInstanceOf(ClientCredentialError);
      expect((outcome.failure as Error).message).toContain('service credential expired');
      // The continuation never reached the provider: the credential was
      // refused before the socket opened.
      expect(provider.connections()).toBe(1);
    },
  );
});
