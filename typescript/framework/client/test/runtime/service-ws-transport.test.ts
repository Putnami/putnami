import { afterEach, describe, expect, test } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import type { ClientContractOperation, ClientSchema, WebSocketStreamMode } from '@putnami/application';
import { SERVICE_WEBSOCKET_SUBPROTOCOL, WebSocketConversationV1 } from '@putnami/application';
import {
  ClientCanceledError,
  ClientFrameworkError,
  ClientResponseContractError,
  ClientTransportUnavailableError,
} from '../../src/runtime/errors';
import { ServiceWebSocketTransport } from '../../src/runtime/service-ws-transport';
import { StreamSession } from '../../src/runtime/stream-session';
import type { StreamObserver } from '../../src/runtime/stream.type';
import type { ClientRequest } from '../../src/runtime/transport.type';

const CORPUS = join(import.meta.dir, '../../../../../protocols/clientcontract/fixtures/websocket');

const output = {
  type: 'object',
  properties: { id: { type: 'string' } },
  required: ['id'],
  additionalProperties: false,
} as const satisfies ClientSchema;

interface CorpusScene {
  readonly operationId: string;
  readonly stream: WebSocketStreamMode;
  readonly encoding: 'json' | 'proto';
  readonly resume: boolean;
  readonly maxFrameBytes: number;
  readonly maxBufferedMessages: number;
  readonly steps: readonly { readonly direction: string; readonly frame: Record<string, unknown> }[];
}

function scene(kind: 'valid' | 'invalid', name: string): CorpusScene {
  return JSON.parse(readFileSync(join(CORPUS, kind, name), 'utf8')) as CorpusScene;
}

function expectations(): { valid: string[]; invalid: Record<string, string> } {
  return JSON.parse(readFileSync(join(CORPUS, 'expectations.json'), 'utf8')) as {
    valid: string[];
    invalid: Record<string, string>;
  };
}

// ---------------------------------------------------------------------------
// A scripted provider: it does the handshake and writes exactly what a scene says.
// ---------------------------------------------------------------------------

interface Scripted {
  readonly url: string;
  /** Every frame the real client wrote, as the provider saw it. */
  readonly received: Record<string, unknown>[];
  stop(): void;
}

const servers: (() => void)[] = [];

afterEach(() => {
  for (const stop of servers.splice(0)) stop();
});

function scriptedProvider(options: {
  respond: (frame: Record<string, unknown>, send: (frame: unknown) => void, index: number) => void;
  subprotocol?: string | undefined;
}): Scripted {
  const received: Record<string, unknown>[] = [];
  const echoed = options.subprotocol === undefined ? SERVICE_WEBSOCKET_SUBPROTOCOL : options.subprotocol;
  const server = Bun.serve<{ index: number }, Record<string, never>>({
    port: 0,
    fetch(request, target) {
      const upgraded = target.upgrade(request, {
        data: { index: 0 },
        ...(echoed ? { headers: { 'Sec-WebSocket-Protocol': echoed } } : {}),
      });
      return upgraded ? undefined : new Response('expected a websocket upgrade', { status: 400 });
    },
    websocket: {
      message(socket, message) {
        const frame = JSON.parse(String(message)) as Record<string, unknown>;
        received.push(frame);
        const index = socket.data.index;
        socket.data.index += 1;
        options.respond(frame, (out) => socket.send(JSON.stringify(out)), index);
      },
    },
  });
  const scripted: Scripted = {
    url: `http://localhost:${server.port}/watch`,
    received,
    stop: () => server.stop(true),
  };
  servers.push(scripted.stop);
  return scripted;
}

function operationContract(overrides: Partial<ClientContractOperation> = {}): ClientContractOperation {
  return {
    stream: 'server',
    messages: { output },
    transports: [
      {
        protocol: 'websocket',
        path: '/watch',
        encoding: 'json',
        websocket: { subprotocol: SERVICE_WEBSOCKET_SUBPROTOCOL, resume: false },
      },
    ],
    security: { alternatives: [{ allOf: [] }] },
    errors: [],
    idempotency: { kind: 'safe' },
    ...overrides,
  };
}

function request(overrides: Partial<ClientRequest> = {}): ClientRequest {
  const headers = new Headers({ 'X-Client-Id': 'fixtures-consumer' });
  return {
    method: 'GET',
    path: '/watch',
    headers,
    operationId: 'widgets.watch',
    clientOperation: operationContract(),
    ...overrides,
  };
}

function wsSession<T>(overrides: { maxFrameBytes?: number; maxBufferedMessages?: number } = {}): StreamSession<T> {
  return new StreamSession<T>({
    serviceId: 'catalog.items',
    operationId: 'widgets.watch',
    protocol: 'websocket',
    budgets: {
      handshakeMs: 2000,
      idleMs: 2000,
      sessionMs: 0,
      heartbeatMs: 0,
      maxFrameBytes: overrides.maxFrameBytes ?? 4096,
      maxBufferedMessages: overrides.maxBufferedMessages ?? 8,
    },
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

/** Replay one corpus scene: the provider writes its own steps, the real client reads them. */
async function replayScene(source: CorpusScene): Promise<{ values?: unknown[]; failure?: unknown }> {
  const providerSteps = source.steps.filter((step) => step.direction === 'server-to-client');
  const provider = scriptedProvider({
    respond: (frame, send) => {
      if (frame['type'] !== 'init') return;
      for (const step of providerSteps) send(step.frame);
    },
  });
  const session = wsSession<unknown>({ maxFrameBytes: source.maxFrameBytes });
  const resumeStep = source.steps.find((step) => step.direction === 'client-to-server' && step.frame['type'] === 'init')
    ?.frame['resume'] as { token: string; afterSequence: string } | undefined;
  const stream = new ServiceWebSocketTransport(provider.url, 'catalog.items').stream<unknown>(
    request({ clientOperation: operationContract({ stream: source.stream }) }),
    {
      stream: source.stream,
      output,
      input: output,
      resumeDeclared: source.resume,
      ...(resumeStep ? { resume: resumeStep } : {}),
    },
    session,
  );
  try {
    return { values: await collect(stream) };
  } catch (failure) {
    return { failure };
  }
}

describe('first-party websocket client, corpus conformance', () => {
  test('accepts the provider half of every valid scene it can carry', async () => {
    // `client-stream-empty-proto` declares an encoding this runtime cannot
    // decode; it is proved refused, with the wire's own code, below.
    const carried = expectations().valid.filter((name) => scene('valid', name).encoding === 'json');
    expect(carried.length).toBeGreaterThan(0);
    for (const name of carried) {
      const source = scene('valid', name);
      // biome-ignore lint/performance/noAwaitInLoops: each scene owns a port
      const outcome = await replayScene(source);
      const failure = outcome.failure as { contractCode?: string } | undefined;
      expect({ name, contractCode: failure?.contractCode }).toEqual({ name, contractCode: undefined });
    }
  });

  specTest(
    'refuses every provider-composed invalid scene with the diagnostic code the corpus declares',
    {
      feature: 'typescript/service-clients',
      requirement: 'websocket-streams',
      check: 'the-corpus-invalid-scenes-are-refused-with-the-declared-code',
    },
    async () => {
      const providerDriven = Object.entries(expectations().invalid).filter(([name]) =>
        scene('invalid', name).steps.some((step) => step.direction === 'server-to-client'),
      );
      expect(providerDriven).toHaveLength(6);
      for (const [name, code] of providerDriven) {
        // biome-ignore lint/performance/noAwaitInLoops: each scene owns a port
        const outcome = await replayScene(scene('invalid', name));
        const failure = outcome.failure as { contractCode?: string } | undefined;
        expect({ name, code: failure?.contractCode }).toEqual({ name, code });
      }
    },
  );

  test('cannot compose the three client-side invalid scenes at all', async () => {
    // `cancel-before-init`: the conversation refuses a cancel that precedes the
    // operation identity, so the client has nothing to cancel and never writes it.
    const conversation = new WebSocketConversationV1({ stream: 'server', encoding: 'json', resumeDeclared: false });
    expect(conversation.accept({ v: 1, type: 'cancel', code: 'canceled' }, 'client-to-server')[0]?.code).toBe(
      expectations().invalid['cancel-before-init.json'],
    );

    // `client-resume`: a resume request on a transport that does not declare
    // resume is refused before the socket carries it.
    const provider = scriptedProvider({ respond: () => {} });
    const session = wsSession();
    const stream = new ServiceWebSocketTransport(provider.url, 'catalog.items').stream(
      request(),
      {
        stream: 'server',
        output,
        resumeDeclared: false,
        resume: { token: 'fixture-resume-token-1', afterSequence: '4' },
      },
      session,
    );
    const failure = (await collect(stream).catch((error: unknown) => error)) as { contractCode?: string };
    expect(failure.contractCode).toBe(expectations().invalid['client-resume.json']);

    // `secret-shaped-unknown-field`: every frame this client writes is read back
    // by the strict parser before it leaves, so an unknown member cannot exist.
    expect(provider.received.every((frame) => Object.hasOwn(frame, 'type'))).toBe(true);
    expect(JSON.stringify(provider.received)).not.toContain('super-secret-frame-field');
  });

  test('covers every fixture on disk', () => {
    const declared = expectations();
    expect(readdirSync(join(CORPUS, 'valid')).sort()).toEqual([...declared.valid].sort());
    expect(readdirSync(join(CORPUS, 'invalid')).sort()).toEqual(Object.keys(declared.invalid).sort());
  });
});

describe('first-party websocket client, admission', () => {
  specTest(
    'opens the socket the way a browser must: subprotocol only, no header, no URL credential',
    {
      feature: 'typescript/service-clients',
      requirement: 'websocket-streams',
      check: 'the-socket-is-opened-with-the-subprotocol-and-no-header',
    },
    async () => {
      const provider = scriptedProvider({
        respond: (frame, send) => {
          if (frame['type'] === 'init') send({ v: 1, type: 'ready', resumed: false });
          if (frame['type'] === 'init') send({ v: 1, type: 'result' });
        },
      });
      const outgoing = request();
      outgoing.headers.set('Authorization', 'Bearer never-on-the-wire');
      outgoing.credentialValues = [{ profile: 'service', value: 'Bearer secret-token' }];
      outgoing.credentialHeaderNames = ['Authorization'];
      const session = wsSession();
      await collect(
        new ServiceWebSocketTransport(provider.url, 'catalog.items').stream(
          outgoing,
          { stream: 'server', output, resumeDeclared: false },
          session,
        ),
      );
      const init = provider.received[0] as { credentials: { value: string }[]; headers: unknown[] };
      expect(init.credentials).toEqual([{ profile: 'service', value: 'Bearer secret-token' }]);
      expect(JSON.stringify(init.headers)).not.toContain('secret-token');
      expect(provider.url).not.toContain('secret-token');
    },
  );

  test('refuses a provider that does not echo the first-party subprotocol', async () => {
    // Bun's `WebSocket.protocol` reports the token the client *requested*, not
    // the one the provider selected, so a Bun server that declines to negotiate
    // cannot exercise this guard. The stub reports what a browser reports.
    const original = globalThis.WebSocket;
    globalThis.WebSocket = class extends original {
      override get protocol(): string {
        return '';
      }
    } as unknown as typeof WebSocket;
    const provider = scriptedProvider({ respond: () => {}, subprotocol: '' });
    try {
      const session = wsSession();
      const failure = await collect(
        new ServiceWebSocketTransport(provider.url, 'catalog.items').stream(
          request(),
          { stream: 'server', output, resumeDeclared: false },
          session,
        ),
      ).catch((error: unknown) => error);
      // The provider serves WebSocket here but not this wire, which is the same
      // class as a 404 on another declared transport: a declared fallback may
      // act on it, and nothing about the call has been decided.
      expect(failure).toBeInstanceOf(ClientTransportUnavailableError);
      expect((failure as Error).message).toContain('did not negotiate the first-party websocket subprotocol');
      expect(provider.received).toEqual([]);
    } finally {
      globalThis.WebSocket = original;
    }
  });

  test('accepts ready resumed=true when it asked for a resume on a resume-capable transport', async () => {
    const provider = scriptedProvider({
      respond: (frame, send) => {
        if (frame['type'] !== 'init') return;
        send({ v: 1, type: 'ready', resumed: true, resumeToken: 'next-token' });
        send({ v: 1, type: 'message', sequence: '5', payload: { encoding: 'json', value: { id: 'after-resume' } } });
        send({ v: 1, type: 'result' });
      },
    });
    const session = wsSession();
    const values = await collect(
      new ServiceWebSocketTransport(provider.url, 'catalog.items').stream(
        request(),
        {
          stream: 'server',
          output,
          resumeDeclared: true,
          resume: { token: 'held-token', afterSequence: '4' },
        },
        session,
      ),
    );
    expect(values).toEqual([{ id: 'after-resume' }]);
  });

  specTest(
    'refuses ready resumed=true when it never asked for one',
    {
      feature: 'typescript/service-clients',
      requirement: 'websocket-streams',
      check: 'a-resumed-ready-is-accepted-only-when-the-init-requested-it',
    },
    async () => {
      const provider = scriptedProvider({
        respond: (frame, send) => {
          if (frame['type'] === 'init') send({ v: 1, type: 'ready', resumed: true, resumeToken: 'next-token' });
        },
      });
      const session = wsSession();
      const failure = (await collect(
        new ServiceWebSocketTransport(provider.url, 'catalog.items').stream(
          request(),
          { stream: 'server', output, resumeDeclared: true },
          session,
        ),
      ).catch((error: unknown) => error)) as { contractCode?: string };
      expect(failure.contractCode).toBe('client_contract.invalid_resilience');
    },
  );

  specTest(
    'invalidates the credential exactly once on a 401 admission refusal',
    {
      feature: 'typescript/service-clients',
      requirement: 'websocket-streams',
      check: 'a-401-admission-refusal-invalidates-the-credential-once',
    },
    async () => {
      let invalidations = 0;
      const provider = scriptedProvider({
        respond: (frame, send) => {
          if (frame['type'] === 'init') {
            send({ v: 1, type: 'error', error: { status: 401, code: 'unauthorized', message: 'Unauthorized' } });
          }
        },
      });
      const session = new StreamSession<unknown>({
        serviceId: 'catalog.items',
        operationId: 'widgets.watch',
        protocol: 'websocket',
        budgets: {
          handshakeMs: 2000,
          idleMs: 2000,
          sessionMs: 0,
          heartbeatMs: 0,
          maxFrameBytes: 4096,
          maxBufferedMessages: 8,
        },
        invalidateCredentials: () => {
          invalidations += 1;
        },
      });
      const failure = await collect(
        new ServiceWebSocketTransport(provider.url, 'catalog.items').stream(
          request(),
          { stream: 'server', output, resumeDeclared: false },
          session,
        ),
      ).catch((error: unknown) => error);
      expect(failure).toBeInstanceOf(ClientFrameworkError);
      expect((failure as ClientFrameworkError).status).toBe(401);
      expect(invalidations).toBe(1);
    },
  );

  specTest(
    'an init that throws is a typed terminal, not an uncaught rejection',
    {
      feature: 'typescript/service-clients',
      requirement: 'websocket-streams',
      check: 'an-init-that-cannot-be-composed-is-a-typed-terminal',
    },
    async () => {
      const provider = scriptedProvider({ respond: () => {} });
      const session = wsSession();
      // No `X-Client-Id`: the init frame has no identity to carry, which the
      // saved transport reported as an uncaught throw from the open handler.
      const anonymous = request();
      anonymous.headers.delete('X-Client-Id');
      const failure = await collect(
        new ServiceWebSocketTransport(provider.url, 'catalog.items').stream(
          anonymous,
          { stream: 'server', output, resumeDeclared: false },
          session,
        ),
      ).catch((error: unknown) => error);
      expect((failure as { code?: string }).code).toBe('client.credential');
    },
  );
});

describe('first-party websocket client, conversation', () => {
  test('numbers its own messages, half-closes once, and reads the single terminal value', async () => {
    const provider = scriptedProvider({
      respond: (frame, send) => {
        if (frame['type'] === 'init') send({ v: 1, type: 'ready', resumed: false });
        if (frame['type'] === 'half-close')
          send({ v: 1, type: 'result', payload: { encoding: 'json', value: { id: 'done' } } });
      },
    });
    const session = wsSession<{ id: string }>();
    const duplex = new ServiceWebSocketTransport(provider.url, 'catalog.items').streamDuplex<
      { id: string },
      { id: string }
    >(
      request({ clientOperation: operationContract({ stream: 'client', messages: { input: output, output } }) }),
      { stream: 'client', input: output, output, resumeDeclared: false },
      session,
    );
    const values = collect(duplex);
    duplex.send({ id: 'a' });
    duplex.send({ id: 'b' });
    duplex.end();
    duplex.end();
    expect(await values).toEqual([{ id: 'done' }]);
    const sequences = provider.received
      .filter((frame) => frame['type'] === 'message')
      .map((frame) => frame['sequence']);
    expect(sequences).toEqual(['1', '2']);
    expect(provider.received.filter((frame) => frame['type'] === 'half-close')).toHaveLength(1);
  });

  test('queues sends made before admission and writes them in order once ready', async () => {
    const provider = scriptedProvider({
      respond: (frame, send) => {
        if (frame['type'] === 'init') {
          send({ v: 1, type: 'ready', resumed: false });
          return;
        }
        if (frame['type'] === 'half-close')
          send({ v: 1, type: 'result', payload: { encoding: 'json', value: { id: 'ok' } } });
      },
    });
    const session = wsSession<{ id: string }>();
    const duplex = new ServiceWebSocketTransport(provider.url, 'catalog.items').streamDuplex<
      { id: string },
      { id: string }
    >(
      request({ clientOperation: operationContract({ stream: 'client', messages: { input: output, output } }) }),
      { stream: 'client', input: output, output, resumeDeclared: false },
      session,
    );
    duplex.send({ id: 'first' });
    const values = collect(duplex);
    duplex.end();
    expect(await values).toEqual([{ id: 'ok' }]);
    expect(provider.received.map((frame) => frame['type'])).toEqual(['init', 'message', 'half-close']);
  });

  test('answers a provider ping with the same nonce', async () => {
    const provider = scriptedProvider({
      respond: (frame, send) => {
        if (frame['type'] === 'init') {
          send({ v: 1, type: 'ready', resumed: false });
          send({ v: 1, type: 'ping', nonce: 'liveness-1' });
        }
        if (frame['type'] === 'pong') send({ v: 1, type: 'result' });
      },
    });
    const session = wsSession();
    await collect(
      new ServiceWebSocketTransport(provider.url, 'catalog.items').stream(
        request(),
        { stream: 'server', output, resumeDeclared: false },
        session,
      ),
    );
    expect(provider.received).toContainEqual({ v: 1, type: 'pong', nonce: 'liveness-1' });
  });

  test('reports a socket that closes without a terminal frame instead of completing silently', async () => {
    const provider = scriptedProvider({ respond: () => {} });
    const session = wsSession();
    const stream = new ServiceWebSocketTransport(provider.url, 'catalog.items').stream(
      request(),
      { stream: 'server', output, resumeDeclared: false },
      session,
    );
    const failure = collect(stream).catch((error: unknown) => error);
    await Bun.sleep(20);
    provider.stop();
    expect(await failure).toBeInstanceOf(ClientResponseContractError);
  });

  test('a caller cancel writes exactly one cancel frame and no second terminal', async () => {
    const provider = scriptedProvider({
      respond: (frame, send) => {
        if (frame['type'] === 'init') send({ v: 1, type: 'ready', resumed: false });
      },
    });
    const session = wsSession();
    const stream = new ServiceWebSocketTransport(provider.url, 'catalog.items').stream(
      request(),
      { stream: 'server', output, resumeDeclared: false },
      session,
    );
    await Bun.sleep(30);
    stream.cancel();
    await Bun.sleep(20);
    expect(provider.received.filter((frame) => frame['type'] === 'cancel')).toEqual([
      { v: 1, type: 'cancel', code: 'canceled' },
    ]);
    expect(session.error).toBeInstanceOf(ClientCanceledError);
  });

  test('refuses a provider frame past the declared frame bound', async () => {
    const provider = scriptedProvider({
      respond: (frame, send) => {
        if (frame['type'] !== 'init') return;
        send({ v: 1, type: 'ready', resumed: false });
        send({ v: 1, type: 'message', sequence: '1', payload: { encoding: 'json', value: { id: 'x'.repeat(400) } } });
      },
    });
    const session = wsSession({ maxFrameBytes: 200 });
    const failure = (await collect(
      new ServiceWebSocketTransport(provider.url, 'catalog.items').stream(
        request(),
        { stream: 'server', output, resumeDeclared: false },
        session,
      ),
    ).catch((error: unknown) => error)) as { contractCode?: string };
    expect(failure.contractCode).toBe('client_contract.invalid_transport');
  });

  test('delivers an empty declared string as empty, not as absent', async () => {
    const provider = scriptedProvider({
      respond: (frame, send) => {
        if (frame['type'] !== 'init') return;
        send({ v: 1, type: 'ready', resumed: false });
        send({ v: 1, type: 'message', sequence: '1', payload: { encoding: 'json', value: { id: '' } } });
        send({ v: 1, type: 'result' });
      },
    });
    const session = wsSession();
    expect(
      await collect(
        new ServiceWebSocketTransport(provider.url, 'catalog.items').stream(
          request(),
          { stream: 'server', output, resumeDeclared: false },
          session,
        ),
      ),
    ).toEqual([{ id: '' }]);
  });

  test('reads sequences past the safe-integer boundary exactly', async () => {
    const provider = scriptedProvider({
      respond: (frame, send) => {
        if (frame['type'] !== 'init') return;
        send({ v: 1, type: 'ready', resumed: true, resumeToken: 'next' });
        send({
          v: 1,
          type: 'message',
          sequence: '9007199254740993',
          payload: { encoding: 'json', value: { id: 'after-safe-integer' } },
        });
        send({
          v: 1,
          type: 'message',
          sequence: '9007199254740994',
          payload: { encoding: 'json', value: { id: 'and-the-next-one' } },
        });
        send({ v: 1, type: 'result' });
      },
    });
    const session = wsSession();
    expect(
      await collect(
        new ServiceWebSocketTransport(provider.url, 'catalog.items').stream(
          request(),
          {
            stream: 'server',
            output,
            resumeDeclared: true,
            resume: { token: 'held', afterSequence: '9007199254740992' },
          },
          session,
        ),
      ),
    ).toEqual([{ id: 'after-safe-integer' }, { id: 'and-the-next-one' }]);
  });
});
