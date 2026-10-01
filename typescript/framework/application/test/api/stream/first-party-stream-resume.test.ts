import { afterEach, describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { resetConfigLoader, Stream } from '@putnami/runtime';
import { api, type ApiPlugin } from '../../../src/api/api.plugin';
import { endpoint } from '../../../src/api/route';
import { application } from '../../../src/application';
import { http, type HttpPlugin } from '../../../src/http/http.plugin';
import { authenticate } from '../../../src/security/identity-resolver.middleware';
import { apiKeyStrategy } from '../../../src/security/strategies/api-key.strategy';
import { RESUME_BOUNDS, ResumeGrantStore } from '../../../src/api/stream/stream-resume';
import { SERVICE_WEBSOCKET_SUBPROTOCOL, type WebSocketServiceFrame } from '../../../src/api/stream/websocket-protocol';

const CLIENT_ID = 'fixtures-consumer';
const API_KEY = 'runtime-only-api-key';
const SECURITY = { alternatives: [{ allOf: [{ profile: 'service-key' }] }] } as const;
const CONTRACT = {
  service: { id: 'catalog.items', audience: 'api://widgets' },
  credentials: { 'service-key': { kind: 'api-key' as const, header: 'X-Api-Key' } },
};

/** One live conversation on a real socket, driven frame by frame. */
class Conversation {
  private readonly inbox: WebSocketServiceFrame[] = [];
  private readonly waiting: ((frame: WebSocketServiceFrame) => void)[] = [];

  private constructor(private readonly socket: WebSocket) {}

  static async open(url: string): Promise<Conversation> {
    const socket = new WebSocket(url, SERVICE_WEBSOCKET_SUBPROTOCOL);
    const conversation = new Conversation(socket);
    socket.addEventListener('message', (event) => conversation.receive(String(event.data)));
    await new Promise<void>((resolve, reject) => {
      socket.addEventListener('open', () => resolve(), { once: true });
      socket.addEventListener('error', () => reject(new Error('websocket upgrade failed')), { once: true });
      socket.addEventListener('close', () => reject(new Error('websocket upgrade was refused')), { once: true });
    });
    return conversation;
  }

  init(overrides: Record<string, unknown> = {}): void {
    this.socket.send(
      JSON.stringify({
        v: 1,
        type: 'init',
        operationId: 'getWidgetsFeed',
        clientId: CLIENT_ID,
        deadlineUnixMs: '0',
        budgetMs: '0',
        credentials: [{ profile: 'service-key', value: API_KEY }],
        headers: [],
        ...overrides,
      }),
    );
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

  /** Read the admission frame and return its rotated grant. */
  async ready(): Promise<{ resumed: boolean; token: string }> {
    const frame = (await this.next()) as { type: string; resumed?: boolean; resumeToken?: string };
    if (frame.type !== 'ready') throw new Error(`the provider answered ${frame.type} instead of admitting`);
    return { resumed: frame.resumed === true, token: frame.resumeToken ?? '' };
  }

  /** Read `count` messages and return their sequences and values. */
  async messages(count: number): Promise<{ sequences: string[]; values: string[] }> {
    const sequences: string[] = [];
    const values: string[] = [];
    for (let index = 0; index < count; index += 1) {
      // biome-ignore lint/performance/noAwaitInLoops: the frames arrive in order
      const frame = (await this.next()) as {
        type: string;
        sequence?: string;
        payload?: { value?: { id?: string } };
      };
      if (frame.type !== 'message') throw new Error(`the provider sent ${frame.type} instead of a message`);
      sequences.push(frame.sequence ?? '');
      values.push(frame.payload?.value?.id ?? '');
    }
    return { sequences, values };
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
}

interface Provider {
  readonly url: string;
}

const running: (() => Promise<void>)[] = [];

afterEach(async () => {
  for (const stop of running.splice(0)) await stop();
  resetConfigLoader();
});

/**
 * A provider whose feed sends two events numbered from the position the stream
 * continues after, so a gap and a duplicate are both visible in the sequences.
 */
async function startFeedProvider(options: { accepted?: () => string[] } = {}): Promise<Provider> {
  const providerHttp: HttpPlugin = http({ port: 0 });
  const key = apiKeyStrategy({ keys: [API_KEY], header: 'X-Api-Key' });
  providerHttp.prepend(
    authenticate({
      anyOf: [
        async (context) => {
          const allowed = options.accepted?.() ?? [API_KEY];
          if (!allowed.includes(context.req.headers.get('X-Api-Key') ?? '')) return undefined;
          return key(context);
        },
      ],
    }),
  );
  const providerApi = api({ autoScan: false, client: CONTRACT });
  register(providerApi);
  const app = application().use(providerHttp).use(providerApi);
  await app.start();
  const port = providerHttp.getServer()?.port;
  running.push(async () => {
    await app.stop();
  });
  return { url: `ws://localhost:${port}/widgets/feed` };
}

function register(plugin: ApiPlugin): void {
  plugin.register(
    '/widgets/feed',
    endpoint()
      .returns(Stream({ id: String }))
      .secure({ principalKind: 'apikey' })
      .client({ security: SECURITY, idempotency: { kind: 'safe' }, resume: true })
      .handle(async (context) => {
        const from = context.resumeFrom ?? 0n;
        for (let offset = 1n; offset <= 2n; offset += 1n) {
          context.send({ id: `event-${from + offset}` });
        }
      }),
    'GET',
  );
}

describe('a declared resumable server stream', () => {
  specTest(
    'continues the sequence it left, with no gap and no duplicate',
    {
      feature: 'typescript/api-contracts',
      requirement: 'declared-stream-resume',
      check: 'a-resumed-server-stream-continues-the-sequence-it-left',
    },
    async () => {
      const provider = await startFeedProvider();
      const first = await Conversation.open(provider.url);
      first.init();
      const opening = await first.ready();
      expect(opening.resumed).toBe(false);
      expect(opening.token).not.toBe('');
      expect(await first.messages(2)).toEqual({ sequences: ['1', '2'], values: ['event-1', 'event-2'] });
      first.close();

      const second = await Conversation.open(provider.url);
      second.init({ resume: { token: opening.token, afterSequence: '2' } });
      const continued = await second.ready();
      expect(continued.resumed).toBe(true);
      expect(continued.token).not.toBe('');
      expect(continued.token).not.toBe(opening.token);
      expect(await second.messages(2)).toEqual({ sequences: ['3', '4'], values: ['event-3', 'event-4'] });
      second.close();
    },
  );

  specTest(
    'spends a grant on its single redemption',
    {
      feature: 'typescript/api-contracts',
      requirement: 'declared-stream-resume',
      check: 'a-resume-grant-is-spent-by-its-single-redemption',
    },
    async () => {
      const provider = await startFeedProvider();
      const first = await Conversation.open(provider.url);
      first.init();
      const { token } = await first.ready();
      await first.messages(2);
      first.close();

      const second = await Conversation.open(provider.url);
      second.init({ resume: { token, afterSequence: '2' } });
      await second.ready();
      second.close();

      const replay = await Conversation.open(provider.url);
      replay.init({ resume: { token, afterSequence: '2' } });
      const failure = (await replay.next()) as { type: string; error?: { code?: string; message?: string } };
      expect(failure.type).toBe('error');
      expect(failure.error?.code).toBe('client_contract.invalid_resilience');
      expect(failure.error?.message).toContain('already spent');
      replay.close();
    },
  );

  specTest(
    'refuses a grant presented by another client, and a position ahead of what it delivered',
    {
      feature: 'typescript/api-contracts',
      requirement: 'declared-stream-resume',
      check: 'a-resume-grant-is-bound-to-the-operation-and-identity-that-earned-it',
    },
    async () => {
      const cases = [
        { name: 'another client', overrides: { clientId: 'another-consumer' }, after: '1' },
        { name: 'a position ahead', overrides: {}, after: '9' },
      ];
      for (const testCase of cases) {
        // biome-ignore lint/performance/noAwaitInLoops: each case owns a provider
        const provider = await startFeedProvider();
        // biome-ignore lint/performance/noAwaitInLoops: the conversation is sequential
        const first = await Conversation.open(provider.url);
        first.init();
        // biome-ignore lint/performance/noAwaitInLoops: the conversation is sequential
        const { token } = await first.ready();
        // biome-ignore lint/performance/noAwaitInLoops: the conversation is sequential
        await first.messages(2);
        first.close();

        // biome-ignore lint/performance/noAwaitInLoops: the conversation is sequential
        const stolen = await Conversation.open(provider.url);
        stolen.init({ ...testCase.overrides, resume: { token, afterSequence: testCase.after } });
        // biome-ignore lint/performance/noAwaitInLoops: the conversation is sequential
        const failure = (await stolen.next()) as { type: string; error?: { code?: string } };
        expect({ name: testCase.name, type: failure.type, code: failure.error?.code }).toEqual({
          name: testCase.name,
          type: 'error',
          code: 'client_contract.invalid_resilience',
        });
        stolen.close();
      }
    },
  );

  specTest(
    'runs the endpoint security chain again on every continuation',
    {
      feature: 'typescript/api-contracts',
      requirement: 'declared-stream-resume',
      check: 'a-continuation-runs-the-endpoint-security-chain-again',
    },
    async () => {
      let accepted = [API_KEY];
      const provider = await startFeedProvider({ accepted: () => accepted });
      const first = await Conversation.open(provider.url);
      first.init();
      const { token } = await first.ready();
      await first.messages(2);
      first.close();

      // The credential the first socket carried is no longer accepted.
      accepted = [];
      const second = await Conversation.open(provider.url);
      second.init({ resume: { token, afterSequence: '2' } });
      const failure = (await second.next()) as { type: string; error?: { status?: number } };
      expect(failure.type).toBe('error');
      expect(failure.error?.status).toBe(401);
      second.close();
    },
  );
});

describe('the resume grant store bounds what it holds', () => {
  specTest(
    'expires a grant at its bound',
    {
      feature: 'typescript/api-contracts',
      requirement: 'declared-stream-resume',
      check: 'a-resume-grant-expires-at-its-bound',
    },
    () => {
      let instant = 1_000_000;
      const store = new ResumeGrantStore({ now: () => instant });
      const issued = store.issue('getWidgetsFeed', CLIENT_ID, 4n, RESUME_BOUNDS.budget);
      expect(issued).toBeDefined();
      instant += RESUME_BOUNDS.ttlMs + 1;
      expect(store.redeem(issued?.token ?? '', 'getWidgetsFeed', CLIENT_ID, 4n)).toHaveProperty('refusal');
    },
  );

  specTest(
    'stops issuing once the budget of one stream is spent',
    {
      feature: 'typescript/api-contracts',
      requirement: 'declared-stream-resume',
      check: 'a-stream-stops-being-resumable-once-its-budget-is-spent',
    },
    () => {
      const store = new ResumeGrantStore();
      let issued = store.issue('getWidgetsFeed', CLIENT_ID, 0n, RESUME_BOUNDS.budget);
      let budget = RESUME_BOUNDS.budget;
      for (let continuation = 0; continuation < RESUME_BOUNDS.budget; continuation += 1) {
        const redemption = store.redeem(issued?.token ?? '', 'getWidgetsFeed', CLIENT_ID, 0n);
        expect(redemption).toHaveProperty('grant');
        budget = ('grant' in redemption ? redemption.grant.budget : 0) - 1;
        issued = store.issue('getWidgetsFeed', CLIENT_ID, 0n, budget);
      }
      expect(budget).toBe(0);
      expect(issued).toBeUndefined();
    },
  );

  specTest(
    'refuses a grant presented by another client, and a position ahead of what it delivered',
    {
      feature: 'typescript/api-contracts',
      requirement: 'declared-stream-resume',
      check: 'a-resume-position-ahead-of-what-the-provider-delivered-is-refused',
    },
    () => {
      const store = new ResumeGrantStore();
      const issued = store.issue('getWidgetsFeed', CLIENT_ID, 2n, RESUME_BOUNDS.budget);
      const token = issued?.token ?? '';
      // A consumer cannot claim to have received more than this provider sent.
      expect(store.redeem(token, 'getWidgetsFeed', CLIENT_ID, 9n)).toHaveProperty('refusal');
      const other = store.issue('getWidgetsFeed', CLIENT_ID, 2n, RESUME_BOUNDS.budget);
      expect(store.redeem(other?.token ?? '', 'getWidgetsFeed', 'another-consumer', 2n)).toHaveProperty('refusal');
      const foreign = store.issue('getWidgetsFeed', CLIENT_ID, 2n, RESUME_BOUNDS.budget);
      expect(store.redeem(foreign?.token ?? '', 'getWidgetsOther', CLIENT_ID, 2n)).toHaveProperty('refusal');
    },
  );

  specTest(
    'bounds the live grants of one endpoint and mints an unguessable token each time',
    {
      feature: 'typescript/api-contracts',
      requirement: 'declared-stream-resume',
      check: 'the-live-grants-of-one-endpoint-are-bounded',
    },
    () => {
      const store = new ResumeGrantStore();
      const first = store.issue('getWidgetsFeed', CLIENT_ID, 0n, RESUME_BOUNDS.budget);
      const seen = new Set<string>();
      for (let index = 0; index < RESUME_BOUNDS.grants; index += 1) {
        const issued = store.issue('getWidgetsFeed', CLIENT_ID, 0n, RESUME_BOUNDS.budget);
        expect(issued?.token.length).toBeGreaterThanOrEqual(40);
        expect(seen.has(issued?.token ?? '')).toBe(false);
        seen.add(issued?.token ?? '');
      }
      expect(store.size).toBeLessThanOrEqual(RESUME_BOUNDS.grants);
      expect(store.redeem(first?.token ?? '', 'getWidgetsFeed', CLIENT_ID, 0n)).toHaveProperty('refusal');
    },
  );
});
