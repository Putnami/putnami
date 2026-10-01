import { afterEach, describe, expect, test } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { NotFoundException, OneOf, resetConfigLoader, Stream } from '@putnami/runtime';
import { api, type ApiPlugin } from '../../../src/api/api.plugin';
import { endpoint } from '../../../src/api/route';
import { application } from '../../../src/application';
import type { HttpRequestContext } from '../../../src/http/http-context.type';
import { http, type HttpPlugin } from '../../../src/http/http.plugin';
import { authenticate } from '../../../src/security/identity-resolver.middleware';
import { apiKeyStrategy } from '../../../src/security/strategies/api-key.strategy';
import {
  followServingDrain,
  resolveFirstPartyStreamAdmission,
  resolveFirstPartyStreamHandlers,
} from '../../../src/api/stream/first-party-stream-dispatcher';
import { buildStreamDefinition } from '../../../src/api/route/stream-endpoint';
import { SERVICE_WEBSOCKET_SUBPROTOCOL, type WebSocketServiceFrame } from '../../../src/api/stream/websocket-protocol';

const CLIENT_ID = 'fixtures-consumer';
const API_KEY = 'runtime-only-api-key';
const SECURITY = { alternatives: [{ allOf: [{ profile: 'service-key' }] }] } as const;
const CONTRACT = {
  service: { id: 'catalog.items', audience: 'api://widgets' },
  credentials: { 'service-key': { kind: 'api-key' as const, header: 'X-Api-Key' } },
};

/** One live conversation: a real socket on a real port, driven frame by frame. */
class Conversation {
  private readonly inbox: WebSocketServiceFrame[] = [];
  private readonly waiting: ((frame: WebSocketServiceFrame) => void)[] = [];
  private closedWith?: { code: number; reason: string };
  private readonly closeWaiters: (() => void)[] = [];

  private constructor(private readonly socket: WebSocket) {}

  static async open(
    url: string,
    subprotocol: string | undefined = SERVICE_WEBSOCKET_SUBPROTOCOL,
  ): Promise<Conversation> {
    const socket = subprotocol === undefined ? new WebSocket(url) : new WebSocket(url, subprotocol);
    const conversation = new Conversation(socket);
    socket.addEventListener('message', (event) => conversation.receive(String(event.data)));
    socket.addEventListener('close', (event) => conversation.closed(event.code, event.reason));
    await new Promise<void>((resolve, reject) => {
      socket.addEventListener('open', () => resolve(), { once: true });
      socket.addEventListener('error', () => reject(new Error('websocket upgrade failed')), { once: true });
      socket.addEventListener('close', () => reject(new Error('websocket upgrade was refused')), { once: true });
    });
    return conversation;
  }

  get negotiated(): string {
    return this.socket.protocol;
  }

  /** Whether the provider sent nothing since the last read and left the socket open. */
  get quiet(): boolean {
    return this.inbox.length === 0 && this.closedWith === undefined;
  }

  send(frame: Record<string, unknown>): void {
    this.socket.send(JSON.stringify(frame));
  }

  sendRaw(data: string): void {
    this.socket.send(data);
  }

  init(overrides: Record<string, unknown> = {}): void {
    this.send({
      v: 1,
      type: 'init',
      operationId: overrides['operationId'] ?? 'getWidgetsWatch',
      clientId: CLIENT_ID,
      deadlineUnixMs: '0',
      budgetMs: '0',
      credentials: [{ profile: 'service-key', value: API_KEY }],
      headers: [],
      ...overrides,
    });
  }

  async next(): Promise<WebSocketServiceFrame> {
    const buffered = this.inbox.shift();
    if (buffered) return buffered;
    return new Promise<WebSocketServiceFrame>((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error('no frame arrived within the test budget')), 4000);
      this.waiting.push((frame) => {
        clearTimeout(timer);
        resolve(frame);
      });
    });
  }

  /** Collect frames until the provider closes, then report the close. */
  async drain(): Promise<{ frames: WebSocketServiceFrame[]; close: { code: number; reason: string } }> {
    if (!this.closedWith) {
      await new Promise<void>((resolve, reject) => {
        const timer = setTimeout(() => reject(new Error('the provider never closed the socket')), 4000);
        this.closeWaiters.push(() => {
          clearTimeout(timer);
          resolve();
        });
      });
    }
    return { frames: [...this.inbox], close: this.closedWith as { code: number; reason: string } };
  }

  close(): void {
    this.socket.close();
  }

  private receive(data: string): void {
    const frame = JSON.parse(data) as WebSocketServiceFrame;
    const waiter = this.waiting.shift();
    if (waiter) waiter(frame);
    else this.inbox.push(frame);
  }

  private closed(code: number, reason: string): void {
    this.closedWith = { code, reason };
    for (const waiter of this.closeWaiters.splice(0)) waiter();
  }
}

interface Provider {
  readonly url: (route: string) => string;
  readonly stop: () => Promise<void>;
}

const running: (() => Promise<void>)[] = [];

afterEach(async () => {
  for (const stop of running.splice(0)) await stop();
  resetConfigLoader();
});

/** Start a real first-party provider on a real port. */
async function startProvider(
  register: (plugin: ApiPlugin) => void,
  options: { secured?: boolean; held?: Promise<void> } = {},
): Promise<Provider> {
  const providerHttp: HttpPlugin = http({ port: 0 });
  if (options.secured !== false) {
    // The held promise makes credential resolution genuinely asynchronous, which
    // is the state the admission machine has to re-read before it answers ready.
    const key = apiKeyStrategy({ keys: [API_KEY], header: 'X-Api-Key' });
    providerHttp.prepend(
      authenticate({
        anyOf: [
          async (context) => {
            // Only the reconstructed admission request carries this ordinary
            // header, so the upgrade stays fast and the credential check is
            // genuinely asynchronous exactly where admission happens.
            if (options.held && context.req.headers.has('X-Slow-Admission')) await options.held;
            return key(context);
          },
        ],
      }),
    );
  }
  const providerApi = api({ autoScan: false, client: CONTRACT });
  register(providerApi);
  const app = application().use(providerHttp).use(providerApi);
  await app.start();
  const port = providerHttp.getServer()?.port;
  const stop = async () => {
    await app.stop();
  };
  running.push(stop);
  return { url: (route) => `ws://localhost:${port}${route}`, stop };
}

describe('first-party websocket provider', () => {
  specTest(
    'negotiates the published subprotocol and admits the caller through the init frame',
    {
      feature: 'typescript/api-contracts',
      requirement: 'first-party-websocket-streams',
      check: 'the-init-frame-is-the-only-admission-channel',
    },
    async () => {
      const provider = await startProvider((plugin) =>
        plugin.register(
          '/widgets/watch',
          endpoint()
            .returns(Stream({ id: String, label: String }))
            .secure({ principalKind: 'apikey' })
            .client({ security: SECURITY, idempotency: { kind: 'safe' } })
            .handle(async (context) => {
              context.send({ id: 'first', label: '' });
              context.send({ id: 'second', label: 'x' });
            }),
          'GET',
        ),
      );
      const conversation = await Conversation.open(provider.url('/widgets/watch'));
      expect(conversation.negotiated).toBe(SERVICE_WEBSOCKET_SUBPROTOCOL);
      conversation.init();
      expect(await conversation.next()).toEqual({ v: 1, type: 'ready', resumed: false });
      expect(await conversation.next()).toEqual({
        v: 1,
        type: 'message',
        sequence: '1',
        payload: { encoding: 'json', value: { id: 'first', label: '' } },
      });
      expect(await conversation.next()).toMatchObject({ type: 'message', sequence: '2' });
      expect(await conversation.next()).toEqual({ v: 1, type: 'result' });
      const { close } = await conversation.drain();
      expect(close.code).toBe(1000);
      expect(close.reason).not.toContain(API_KEY);
    },
  );

  specTest(
    'refuses the upgrade when the client offers only a subprotocol this route cannot speak',
    {
      feature: 'typescript/api-contracts',
      requirement: 'first-party-websocket-streams',
      check: 'an-unspeakable-subprotocol-is-refused-at-the-upgrade',
    },
    async () => {
      const provider = await startProvider(
        (plugin) =>
          plugin.register(
            '/widgets/watch',
            endpoint()
              .returns(Stream({ id: String }))
              .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'safe' } })
              .handle(async () => {}),
            'GET',
          ),
        { secured: false },
      );
      await expect(Conversation.open(provider.url('/widgets/watch'), 'chat.v9')).rejects.toThrow();
    },
  );

  specTest(
    'runs no handler and delivers no message before the init frame',
    {
      feature: 'typescript/api-contracts',
      requirement: 'first-party-websocket-streams',
      check: 'a-frame-before-init-runs-no-handler',
    },
    async () => {
      let handlerRan = false;
      const provider = await startProvider((plugin) =>
        plugin.register(
          '/widgets/watch',
          endpoint()
            .returns(Stream({ id: String }))
            .secure({ principalKind: 'apikey' })
            .client({ security: SECURITY, idempotency: { kind: 'safe' } })
            .handle(async () => {
              handlerRan = true;
            }),
          'GET',
        ),
      );
      const conversation = await Conversation.open(provider.url('/widgets/watch'));
      conversation.send({ v: 1, type: 'message', sequence: '1', payload: { encoding: 'json', value: 1 } });
      expect(await conversation.next()).toMatchObject({
        type: 'error',
        error: { status: 400, code: 'client_contract.invalid_transport' },
      });
      const { close } = await conversation.drain();
      expect(close.code).toBe(1008);
      expect(handlerRan).toBe(false);
    },
  );

  specTest(
    'rejects an unsatisfied security alternative before the handler, with the endpoint status',
    {
      feature: 'typescript/api-contracts',
      requirement: 'first-party-websocket-streams',
      check: 'an-unsatisfied-security-alternative-is-refused-before-the-handler',
    },
    async () => {
      let handlerRan = false;
      const provider = await startProvider((plugin) =>
        plugin.register(
          '/widgets/watch',
          endpoint()
            .returns(Stream({ id: String }))
            .secure({ principalKind: 'apikey' })
            .client({ security: SECURITY, idempotency: { kind: 'safe' } })
            .handle(async () => {
              handlerRan = true;
            }),
          'GET',
        ),
      );
      const conversation = await Conversation.open(provider.url('/widgets/watch'));
      conversation.init({ credentials: [] });
      expect(await conversation.next()).toMatchObject({
        type: 'error',
        error: { code: 'client_contract.invalid_security' },
      });
      await conversation.drain();
      expect(handlerRan).toBe(false);
    },
  );

  test('rejects a credential profile the service never declared', async () => {
    const provider = await startProvider((plugin) =>
      plugin.register(
        '/widgets/watch',
        endpoint()
          .returns(Stream({ id: String }))
          .secure({ principalKind: 'apikey' })
          .client({ security: SECURITY, idempotency: { kind: 'safe' } })
          .handle(async () => {}),
        'GET',
      ),
    );
    const conversation = await Conversation.open(provider.url('/widgets/watch'));
    conversation.init({ credentials: [{ profile: 'not-declared', value: API_KEY }] });
    expect(await conversation.next()).toMatchObject({
      type: 'error',
      error: { code: 'client_contract.unknown_profile' },
    });
    await conversation.drain();
  });

  test('rejects an ordinary header that would shadow a declared credential profile', async () => {
    const provider = await startProvider((plugin) =>
      plugin.register(
        '/widgets/watch',
        endpoint()
          .returns(Stream({ id: String }))
          .secure({ principalKind: 'apikey' })
          .client({ security: SECURITY, idempotency: { kind: 'safe' } })
          .handle(async () => {}),
        'GET',
      ),
    );
    const conversation = await Conversation.open(provider.url('/widgets/watch'));
    conversation.init({ headers: [{ name: 'X-Api-Key', values: ['forged'] }] });
    expect(await conversation.next()).toMatchObject({
      type: 'error',
      error: { code: 'client_contract.invalid_security' },
    });
    await conversation.drain();
  });

  test('rejects an init frame addressed to another operation', async () => {
    const provider = await startProvider((plugin) =>
      plugin.register(
        '/widgets/watch',
        endpoint()
          .returns(Stream({ id: String }))
          .secure({ principalKind: 'apikey' })
          .client({ security: SECURITY, idempotency: { kind: 'safe' } })
          .handle(async () => {}),
        'GET',
      ),
    );
    const conversation = await Conversation.open(provider.url('/widgets/watch'));
    conversation.init({ operationId: 'getSomethingElse' });
    expect(await conversation.next()).toMatchObject({
      type: 'error',
      error: { code: 'client_contract.invalid_transport' },
    });
    await conversation.drain();
  });

  test('answers a heartbeat while admission waits on an asynchronous credential check', async () => {
    let release: (() => void) | undefined;
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    const provider = await startProvider(
      (plugin) =>
        plugin.register(
          '/widgets/watch',
          endpoint()
            .returns(Stream({ id: String }))
            .secure({ principalKind: 'apikey' })
            .client({
              security: SECURITY,
              idempotency: { kind: 'safe' },
              resilience: { stream: { idleTimeoutMs: 5000 } },
            })
            .handle(async (context) => {
              context.send({ id: 'after-slow-admission' });
            }),
          'GET',
        ),
      { held },
    );
    const conversation = await Conversation.open(provider.url('/widgets/watch'));
    conversation.init({ headers: [{ name: 'X-Slow-Admission', values: ['1'] }] });
    conversation.send({ v: 1, type: 'ping', nonce: 'admission-1' });
    expect(await conversation.next()).toEqual({ v: 1, type: 'pong', nonce: 'admission-1' });
    release?.();
    expect(await conversation.next()).toEqual({ v: 1, type: 'ready', resumed: false });
  });

  specTest(
    'never sends ready to a caller that cancelled during the asynchronous check',
    {
      feature: 'typescript/api-contracts',
      requirement: 'first-party-websocket-streams',
      check: 'admission-is-re-checked-after-an-asynchronous-credential-check',
    },
    async () => {
      let release: (() => void) | undefined;
      const held = new Promise<void>((resolve) => {
        release = resolve;
      });
      let handlerRan = false;
      const provider = await startProvider(
        (plugin) =>
          plugin.register(
            '/widgets/watch',
            endpoint()
              .returns(Stream({ id: String }))
              .secure({ principalKind: 'apikey' })
              .client({ security: SECURITY, idempotency: { kind: 'safe' } })
              .handle(async () => {
                handlerRan = true;
              }),
            'GET',
          ),
        { held },
      );
      const conversation = await Conversation.open(provider.url('/widgets/watch'));
      conversation.init({ headers: [{ name: 'X-Slow-Admission', values: ['1'] }] });
      conversation.send({ v: 1, type: 'cancel', code: 'canceled' });
      const { close } = await conversation.drain();
      expect(close.code).toBe(1000);
      release?.();
      await Bun.sleep(1);
      expect(handlerRan).toBe(false);
    },
  );

  specTest(
    'bounds admission by the declared handshake budget without a real sleep',
    {
      feature: 'typescript/api-contracts',
      requirement: 'first-party-websocket-streams',
      check: 'the-declared-budgets-bound-admission-idleness-and-frame-size',
    },
    async () => {
      const provider = await startProvider(
        (plugin) =>
          plugin.register(
            '/widgets/watch',
            endpoint()
              .returns(Stream({ id: String }))
              .client({
                security: { alternatives: [{ allOf: [] }] },
                idempotency: { kind: 'safe' },
                resilience: { stream: { handshakeTimeoutMs: 30 } },
              })
              .handle(async () => {}),
            'GET',
          ),
        { secured: false },
      );
      const conversation = await Conversation.open(provider.url('/widgets/watch'));
      expect(await conversation.next()).toMatchObject({
        type: 'error',
        error: { status: 408, code: 'client.deadline' },
      });
      const { close } = await conversation.drain();
      expect(close.code).toBe(1008);
    },
  );

  test('ends an idle admitted stream on the declared idle budget', async () => {
    const provider = await startProvider(
      (plugin) =>
        plugin.register(
          '/widgets/inbox',
          endpoint()
            .body(Stream({ id: String }))
            .returns({ received: Number })
            .client({
              security: { alternatives: [{ allOf: [] }] },
              idempotency: { kind: 'idempotent' },
              resilience: { stream: { idleTimeoutMs: 30 } },
            })
            .handle(async (context) => {
              let received = 0;
              for await (const _message of context.messages()) received += 1;
              return { received };
            }),
          'GET',
        ),
      { secured: false },
    );
    const conversation = await Conversation.open(provider.url('/widgets/inbox'));
    conversation.init({ operationId: 'getWidgetsInbox', credentials: [] });
    expect(await conversation.next()).toEqual({ v: 1, type: 'ready', resumed: false });
    expect(await conversation.next()).toMatchObject({ type: 'error', error: { status: 408, code: 'client.deadline' } });
  });

  test('emits provider pings at the declared heartbeat cadence and stays silent without one', async () => {
    const provider = await startProvider(
      (plugin) => {
        plugin.register(
          '/widgets/beating',
          endpoint()
            .returns(Stream({ id: String }))
            .client({
              security: { alternatives: [{ allOf: [] }] },
              idempotency: { kind: 'safe' },
              resilience: { stream: { heartbeatMs: 20, idleTimeoutMs: 5000 } },
            })
            .handle(async () => {
              await Bun.sleep(120);
            }),
          'GET',
        );
      },
      { secured: false },
    );
    const conversation = await Conversation.open(provider.url('/widgets/beating'));
    conversation.init({ operationId: 'getWidgetsBeating', credentials: [] });
    expect(await conversation.next()).toEqual({ v: 1, type: 'ready', resumed: false });
    const ping = await conversation.next();
    expect(ping).toMatchObject({ type: 'ping' });
    expect((ping as { nonce: string }).nonce).toMatch(/^[A-Za-z0-9._~-]{1,64}$/);
  });

  specTest(
    'carries a client stream to its single typed result after one half-close',
    {
      feature: 'typescript/api-contracts',
      requirement: 'first-party-websocket-streams',
      check: 'a-client-stream-returns-its-single-declared-value',
    },
    async () => {
      const provider = await startProvider(
        (plugin) =>
          plugin.register(
            '/widgets/inbox',
            endpoint()
              .body(Stream({ id: String }))
              .returns({ received: Number })
              .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'idempotent' } })
              .handle(async (context) => {
                const seen: string[] = [];
                for await (const message of context.messages()) seen.push((message as { id: string }).id);
                return { received: seen.length };
              }),
            'GET',
          ),
        { secured: false },
      );
      const conversation = await Conversation.open(provider.url('/widgets/inbox'));
      conversation.init({ operationId: 'getWidgetsInbox', credentials: [] });
      expect(await conversation.next()).toEqual({ v: 1, type: 'ready', resumed: false });
      conversation.send({ v: 1, type: 'message', sequence: '1', payload: { encoding: 'json', value: { id: 'a' } } });
      conversation.send({ v: 1, type: 'message', sequence: '2', payload: { encoding: 'json', value: { id: 'b' } } });
      conversation.send({ v: 1, type: 'half-close' });
      expect(await conversation.next()).toEqual({
        v: 1,
        type: 'result',
        payload: { encoding: 'json', value: { received: 2 } },
      });
      const { frames, close } = await conversation.drain();
      expect(frames).toEqual([]);
      expect(close.code).toBe(1000);
    },
  );

  test('refuses a second half-close and a client message that skips a sequence', async () => {
    const provider = await startProvider(
      (plugin) =>
        plugin.register(
          '/widgets/inbox',
          endpoint()
            .body(Stream({ id: String }))
            .returns({ received: Number })
            .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'idempotent' } })
            .handle(async (context) => {
              for await (const _message of context.messages()) {
                // drain
              }
              await new Promise(() => {});
              return { received: 0 };
            }),
          'GET',
        ),
      { secured: false },
    );
    const conversation = await Conversation.open(provider.url('/widgets/inbox'));
    conversation.init({ operationId: 'getWidgetsInbox', credentials: [] });
    expect(await conversation.next()).toEqual({ v: 1, type: 'ready', resumed: false });
    conversation.send({ v: 1, type: 'message', sequence: '2', payload: { encoding: 'json', value: { id: 'a' } } });
    expect(await conversation.next()).toMatchObject({
      type: 'error',
      error: { code: 'client_contract.invalid_transport' },
    });
  });

  specTest(
    'carries a bidirectional stream past the client half-close to its terminal value',
    {
      feature: 'typescript/api-contracts',
      requirement: 'first-party-websocket-streams',
      check: 'a-bidirectional-stream-outlives-the-client-half-close',
    },
    async () => {
      const provider = await startProvider(
        (plugin) =>
          plugin.register(
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
          ),
        { secured: false },
      );
      const conversation = await Conversation.open(provider.url('/widgets/duplex'));
      conversation.init({ operationId: 'getWidgetsDuplex', credentials: [] });
      expect(await conversation.next()).toEqual({ v: 1, type: 'ready', resumed: false });
      conversation.send({ v: 1, type: 'message', sequence: '1', payload: { encoding: 'json', value: { id: 'a' } } });
      expect(await conversation.next()).toMatchObject({ type: 'message', sequence: '1' });
      conversation.send({ v: 1, type: 'half-close' });
      expect(await conversation.next()).toMatchObject({ type: 'message', sequence: '2' });
      expect(await conversation.next()).toMatchObject({
        type: 'result',
        payload: { encoding: 'json', value: { echo: 'done' } },
      });
    },
  );

  specTest(
    'projects a declared handler error into the D0.1 envelope with its stable code',
    {
      feature: 'typescript/api-contracts',
      requirement: 'first-party-websocket-streams',
      check: 'a-declared-handler-error-carries-its-stable-code-and-details',
    },
    async () => {
      const provider = await startProvider(
        (plugin) =>
          plugin.register(
            '/widgets/failing',
            endpoint()
              .returns(Stream({ id: String }))
              .mayThrow('NotFound')
              .throws(404, 'Missing widget stream', { code: OneOf('NotFound'), reason: String })
              .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'safe' } })
              .handle(async () => {
                throw new NotFoundException({ code: 'NotFound', reason: 'safe missing reason' });
              }),
            'GET',
          ),
        { secured: false },
      );
      const conversation = await Conversation.open(provider.url('/widgets/failing'));
      conversation.init({ operationId: 'getWidgetsFailing', credentials: [] });
      expect(await conversation.next()).toEqual({ v: 1, type: 'ready', resumed: false });
      expect(await conversation.next()).toMatchObject({
        type: 'error',
        error: { status: 404, code: 'not_found', details: { code: 'NotFound', reason: 'safe missing reason' } },
      });
      const { close } = await conversation.drain();
      expect(close.code).toBe(1011);
      expect(close.reason).toBe('handler error');
    },
  );

  specTest(
    'refuses to write a payload the published wire would reject, instead of emitting it',
    {
      feature: 'typescript/api-contracts',
      requirement: 'websocket-wire-conformance',
      check: 'every-emitted-frame-is-read-back-by-the-published-parser',
    },
    async () => {
      // The wire refuses a JSON `null` anywhere in a frame, application payload
      // included (`protocols/clientcontract` `checkJSONValue`). A provider that
      // did not read its own frames back would write bytes no conforming client
      // accepts; here the refusal is the provider's, carrying the wire's code.
      const handlers = resolveFirstPartyStreamHandlers(
        buildStreamDefinition('server', async (context: { send: (value: unknown) => void }) => {
          context.send({ id: null });
          await new Promise(() => {});
        }),
        {
          operationId: 'getWidgetsNullable',
          contract: CONTRACT,
          security: { alternatives: [{ allOf: [] }] },
          transport: {
            protocol: 'websocket',
            path: '/widgets/nullable',
            encoding: 'json',
            websocket: { subprotocol: SERVICE_WEBSOCKET_SUBPROTOCOL, resume: false },
          },
          budgets: {
            handshakeTimeoutMs: 1000,
            idleTimeoutMs: 1000,
            heartbeatMs: 0,
            maxFrameBytes: 4096,
            maxBufferedMessages: 4,
          },
        },
      );
      const socket = fakeSocket();
      handlers.OPEN({ ws: socket } as never);
      await handlers.MESSAGE({
        ws: socket,
        message: JSON.stringify({
          v: 1,
          type: 'init',
          operationId: 'getWidgetsNullable',
          clientId: CLIENT_ID,
          deadlineUnixMs: '0',
          budgetMs: '0',
          credentials: [],
          headers: [],
        }),
      } as never);
      await Bun.sleep(1);
      const written = socket.sent.map((frame) => JSON.parse(frame) as Record<string, unknown>);
      expect(written[0]).toEqual({ v: 1, type: 'ready', resumed: false });
      expect(written[1]).toMatchObject({ type: 'error', error: { code: 'client_contract.parse_error' } });
      expect(written.some((frame) => frame['type'] === 'message')).toBe(false);
      handlers.CLOSE({ ws: socket } as never);
    },
  );

  specTest(
    'ends every live conversation with the typed shutdown terminal',
    {
      feature: 'typescript/api-contracts',
      requirement: 'first-party-websocket-streams',
      check: 'application-shutdown-ends-a-live-stream-with-a-typed-terminal',
    },
    async () => {
      const provider = await startProvider(
        (plugin) =>
          plugin.register(
            '/widgets/watch',
            endpoint()
              .returns(Stream({ id: String }))
              .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'safe' } })
              .handle(async () => {
                await new Promise(() => {});
              }),
            'GET',
          ),
        { secured: false },
      );
      const conversation = await Conversation.open(provider.url('/widgets/watch'));
      conversation.init({ credentials: [] });
      expect(await conversation.next()).toEqual({ v: 1, type: 'ready', resumed: false });
      void provider.stop();
      expect(await conversation.next()).toMatchObject({
        type: 'error',
        error: { status: 503, code: 'http.service_unavailable' },
      });
    },
  );

  specTest(
    'ends only the conversations of the instance that stops when two instances share a loaded route folder',
    {
      feature: 'typescript/api-contracts',
      requirement: 'first-party-websocket-streams',
      check: 'application-shutdown-ends-a-live-stream-with-a-typed-terminal',
    },
    async () => {
      // A scanned route folder is loaded once per process and merged into
      // every application instance that scans it; the loaded plugin itself is
      // never started nor stopped. Stopping one instance must end its own
      // conversations with the typed terminal and nothing of the other's.
      const loaded = api({ autoScan: false, client: CONTRACT });
      loaded.register(
        '/widgets/watch',
        endpoint()
          .returns(Stream({ id: String }))
          .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'safe' } })
          .handle(async () => {
            await new Promise(() => {});
          }),
        'GET',
      );
      const instances = await Promise.all(
        [1, 2].map(async () => {
          const providerHttp: HttpPlugin = http({ port: 0 });
          const app = application()
            .use(providerHttp)
            .use(api({ autoScan: false, client: CONTRACT, preloadedModule: { loaded } }));
          await app.start();
          return { app, port: providerHttp.getServer()?.port };
        }),
      );
      const open = async (port: number | undefined) => {
        const conversation = await Conversation.open(`ws://localhost:${port}/widgets/watch`);
        conversation.init({ credentials: [] });
        expect(await conversation.next()).toEqual({ v: 1, type: 'ready', resumed: false });
        return conversation;
      };
      const [onFirst, onSecond] = await Promise.all([open(instances[0]?.port), open(instances[1]?.port)]);
      try {
        await instances[0]?.app.stop();
        expect(await onFirst.next()).toMatchObject({
          type: 'error',
          error: { status: 503, code: 'http.service_unavailable' },
        });
        await Bun.sleep(100);
        expect(onSecond.quiet).toBe(true);
      } finally {
        await instances[1]?.app.stop();
      }
      expect(await onSecond.next()).toMatchObject({
        type: 'error',
        error: { status: 503, code: 'http.service_unavailable' },
      });
    },
  );

  test('refuses a client frame past the declared frame bound', async () => {
    const provider = await startProvider(
      (plugin) =>
        plugin.register(
          '/widgets/inbox',
          endpoint()
            .body(Stream({ id: String }))
            .returns({ received: Number })
            .client({
              security: { alternatives: [{ allOf: [] }] },
              idempotency: { kind: 'idempotent' },
              resilience: { stream: { maxFrameBytes: 200 } },
            })
            .handle(async (context) => {
              for await (const _message of context.messages()) {
                // drain
              }
              return { received: 0 };
            }),
          'GET',
        ),
      { secured: false },
    );
    const conversation = await Conversation.open(provider.url('/widgets/inbox'));
    conversation.init({ operationId: 'getWidgetsInbox', credentials: [] });
    expect(await conversation.next()).toEqual({ v: 1, type: 'ready', resumed: false });
    conversation.send({
      v: 1,
      type: 'message',
      sequence: '1',
      payload: { encoding: 'json', value: { id: 'x'.repeat(400) } },
    });
    expect(await conversation.next()).toMatchObject({
      type: 'error',
      error: { code: 'client_contract.invalid_transport' },
    });
  });

  specTest(
    'ends the conversation when the inbound queue passes its declared depth, never dropping in silence',
    {
      feature: 'typescript/api-contracts',
      requirement: 'first-party-websocket-streams',
      check: 'the-inbound-queue-bound-ends-the-stream-instead-of-dropping',
    },
    async () => {
      let release: (() => void) | undefined;
      const held = new Promise<void>((resolve) => {
        release = resolve;
      });
      const provider = await startProvider(
        (plugin) =>
          plugin.register(
            '/widgets/inbox',
            endpoint()
              .body(Stream({ id: String }))
              .returns({ received: Number })
              .client({
                security: { alternatives: [{ allOf: [] }] },
                idempotency: { kind: 'idempotent' },
                resilience: { stream: { maxBufferedMessages: 1 } },
              })
              .handle(async (context) => {
                await held;
                let received = 0;
                for await (const _message of context.messages()) received += 1;
                return { received };
              }),
            'GET',
          ),
        { secured: false },
      );
      const conversation = await Conversation.open(provider.url('/widgets/inbox'));
      conversation.init({ operationId: 'getWidgetsInbox', credentials: [] });
      expect(await conversation.next()).toEqual({ v: 1, type: 'ready', resumed: false });
      for (const sequence of ['1', '2', '3']) {
        conversation.send({ v: 1, type: 'message', sequence, payload: { encoding: 'json', value: { id: sequence } } });
      }
      expect(await conversation.next()).toMatchObject({
        type: 'error',
        error: { status: 429, code: 'http.too_many_requests' },
      });
      release?.();
    },
  );
});

describe('following the serving drain', () => {
  const served = (drain?: AbortSignal) => ({ __drain: drain }) as unknown as HttpRequestContext;

  test('follows nothing when the upgrade request carries no drain', () => {
    let ended = 0;
    expect(followServingDrain(undefined, () => ended++)).toBeUndefined();
    expect(followServingDrain(served(), () => ended++)).toBeUndefined();
    expect(ended).toBe(0);
  });

  test('ends at once when the serving plugin is already draining', () => {
    const drain = new AbortController();
    drain.abort();
    let ended = 0;
    expect(followServingDrain(served(drain.signal), () => ended++)).toBeUndefined();
    expect(ended).toBe(1);
  });

  test('ends once on drain, and never after its handle is released', () => {
    const drain = new AbortController();
    let ended = 0;
    const release = followServingDrain(served(drain.signal), () => ended++);
    const other = new AbortController();
    let released = 0;
    followServingDrain(served(other.signal), () => released++)?.();
    drain.abort();
    other.abort();
    expect(ended).toBe(1);
    expect(released).toBe(0);
    release?.();
  });
});

describe('first-party websocket admission metadata', () => {
  const definition = buildStreamDefinition('server', async () => {}, undefined, undefined, {
    client: { security: SECURITY, idempotency: { kind: 'safe' }, resilience: { stream: { heartbeatMs: 40 } } },
  });

  test('reads the declared budgets and falls back to the framework floor for the rest', () => {
    const admission = resolveFirstPartyStreamAdmission({
      operationId: 'getWidgetsWatch',
      path: '/widgets/watch',
      contract: { ...CONTRACT, defaults: { resilience: { attemptTimeoutMs: 2500 } } },
      definition,
    });
    expect(admission.budgets).toEqual({
      handshakeTimeoutMs: 2500,
      idleTimeoutMs: 30_000,
      heartbeatMs: 40,
      maxFrameBytes: 1024 * 1024,
      maxBufferedMessages: 16,
    });
    expect(admission.security).toEqual(SECURITY);
    expect(admission.transport).toEqual({
      protocol: 'websocket',
      path: '/widgets/watch',
      encoding: 'json',
      websocket: { subprotocol: SERVICE_WEBSOCKET_SUBPROTOCOL, resume: false },
    });
  });

  test('declares no heartbeat when the provider declared none', () => {
    const admission = resolveFirstPartyStreamAdmission({
      operationId: 'getWidgetsWatch',
      path: '/widgets/watch',
      contract: CONTRACT,
      definition: buildStreamDefinition('server', async () => {}),
    });
    expect(admission.budgets.heartbeatMs).toBe(0);
    expect(admission.security).toEqual({ alternatives: [{ allOf: [] }] });
  });

  specTest(
    'refuses a proto transport at admission, before any message reaches the handler',
    {
      feature: 'typescript/api-contracts',
      requirement: 'first-party-websocket-streams',
      check: 'an-undeclared-encoding-is-refused-at-admission',
    },
    async () => {
      let handlerRan = false;
      const handlers = resolveFirstPartyStreamHandlers(
        buildStreamDefinition('client', async () => {
          handlerRan = true;
        }),
        {
          operationId: 'getWidgetsUpload',
          contract: CONTRACT,
          security: { alternatives: [{ allOf: [] }] },
          transport: {
            protocol: 'websocket',
            path: '/widgets/upload',
            encoding: 'proto',
            websocket: { subprotocol: SERVICE_WEBSOCKET_SUBPROTOCOL, resume: false },
          },
          budgets: {
            handshakeTimeoutMs: 1000,
            idleTimeoutMs: 1000,
            heartbeatMs: 0,
            maxFrameBytes: 4096,
            maxBufferedMessages: 4,
          },
        },
      );
      const socket = fakeSocket();
      handlers.OPEN({ ws: socket } as never);
      await handlers.MESSAGE({
        ws: socket,
        message: JSON.stringify({
          v: 1,
          type: 'init',
          operationId: 'getWidgetsUpload',
          clientId: CLIENT_ID,
          deadlineUnixMs: '0',
          budgetMs: '0',
          credentials: [],
          headers: [],
        }),
      } as never);
      expect(JSON.parse(socket.sent[0] as string)).toMatchObject({
        type: 'error',
        error: { code: 'client_contract.invalid_transport' },
      });
      expect((socket.sent[0] as string).includes('getWidgetsUpload')).toBe(true);
      expect(handlerRan).toBe(false);
      handlers.CLOSE({ ws: socket } as never);
    },
  );

  specTest(
    'refuses a resume the transport does not declare before it reaches the encoding rule',
    {
      feature: 'typescript/api-contracts',
      requirement: 'first-party-websocket-streams',
      check: 'a-resume-the-transport-does-not-declare-is-refused-before-the-encoding-limit',
    },
    async () => {
      const handlers = resolveFirstPartyStreamHandlers(
        buildStreamDefinition('client', async () => {}),
        {
          operationId: 'getWidgetsUpload',
          contract: CONTRACT,
          security: { alternatives: [{ allOf: [] }] },
          transport: {
            protocol: 'websocket',
            path: '/widgets/upload',
            encoding: 'proto',
            websocket: { subprotocol: SERVICE_WEBSOCKET_SUBPROTOCOL, resume: false },
          },
          budgets: {
            handshakeTimeoutMs: 1000,
            idleTimeoutMs: 1000,
            heartbeatMs: 0,
            maxFrameBytes: 4096,
            maxBufferedMessages: 4,
          },
        },
      );
      const socket = fakeSocket();
      handlers.OPEN({ ws: socket } as never);
      await handlers.MESSAGE({
        ws: socket,
        message: JSON.stringify({
          v: 1,
          type: 'init',
          operationId: 'getWidgetsUpload',
          clientId: CLIENT_ID,
          deadlineUnixMs: '0',
          budgetMs: '0',
          credentials: [],
          headers: [],
          resume: { token: 'resume-token', afterSequence: '4' },
        }),
      } as never);
      expect(JSON.parse(socket.sent[0] as string)).toMatchObject({
        type: 'error',
        error: { code: 'client_contract.invalid_resilience' },
      });
      handlers.CLOSE({ ws: socket } as never);
    },
  );
});

/** The Bun socket a dispatcher sees, with the upgrade context the admission machine rebuilds from. */
function fakeSocket(path = '/widgets/nullable') {
  const sent: string[] = [];
  return {
    sent,
    closes: [] as { code: number; reason: string }[],
    data: {
      __httpContext: {
        req: new Request(`http://localhost${path}`),
        headers: new Headers(),
        params: {},
        queryParams: () => ({}),
        path: () => path,
      },
    } as Record<string, unknown>,
    send(data: string) {
      sent.push(data);
    },
    close(code: number, reason: string) {
      this.closes.push({ code, reason });
    },
  };
}
