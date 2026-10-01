import { afterEach, beforeEach, describe, expect } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import {
  application,
  type ClientCachePolicy,
  type ClientContractDocument,
  type ClientContractOperation,
  setCollector,
  TelemetryCollector,
} from '@putnami/application';
import { runInContext } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { BaseClient } from '../../src/runtime/base-client';
import { CredentialRegistryClosedError } from '../../src/runtime/credential';
import {
  ClientCanceledError,
  ClientError,
  ClientFrameworkError,
  ClientRetryExhaustedError,
} from '../../src/runtime/errors';
import {
  CLIENT_RUNTIME_CAPABILITIES,
  canonicalCacheKey,
  canonicalizeJsonText,
  invalidationTag,
  type ResponseFieldValue,
  requireClientRuntimeCapabilities,
  responseCacheKey,
  responseTags,
  STALE_SERVED_METRIC,
  ServiceResponseCache,
  serviceResponseCacheInterceptor,
} from '../../src/runtime/response-cache';
import { parseJsonValue } from '../../src/runtime/json-codec';
import { markTransportFailure } from '../../src/runtime/transport-failure';
import type { ClientRequest } from '../../src/runtime/transport.type';
import {
  createServiceClientRegistration,
  type GeneratedServiceDescriptor,
  registerServiceClient,
} from '../../src/runtime/service-binding';

const FEATURE = 'typescript/service-clients';
const REQUIREMENT = 'response-cache';
const CONTROL_REQUIREMENT = 'response-cache-bypass-and-field-invalidation';
const CONTRACT_ROOT = join(import.meta.dir, '../../../../../protocols/clientcontract');

const contract: ClientContractDocument = {
  protocolVersion: 1,
  service: { id: 'identity', audience: 'urn:identity' },
  credentials: {},
};

function accountOperation(
  cache: ClientCachePolicy,
  resilience: ClientContractOperation['resilience'] = {},
): ClientContractOperation {
  return {
    stream: 'unary',
    transports: [{ protocol: 'rest-json', path: '/accounts/{id}', encoding: 'json' }],
    security: { alternatives: [{ allOf: [] }] },
    errors: [{ status: 404, code: 'not_found' }],
    idempotency: { kind: 'safe' },
    resilience: { ...resilience, cache },
  };
}

const FRESH_STALE: ClientCachePolicy = { freshMs: 5000, staleMs: 300_000 };

class AccountsClient extends BaseClient {
  readonly serviceName = 'identity';

  get(id: string, signal?: AbortSignal, withoutResponseCache?: boolean): Promise<{ value: string }> {
    return this.request('GET', '/accounts/{id}', {
      params: { id },
      operationId: 'getAccount',
      signal,
      withoutResponseCache,
      successes: [
        {
          status: 200,
          description: 'Account',
          content: [
            {
              mediaType: 'application/json',
              schema: {
                type: 'object',
                properties: { value: { type: 'string' } },
                required: ['value'],
                additionalProperties: false,
              },
            },
          ],
        },
      ],
    });
  }
}

const PRINCIPAL_SUCCESS = [
  {
    status: 200,
    description: 'Access',
    content: [
      {
        mediaType: 'application/json',
        schema: {
          type: 'object' as const,
          properties: { value: { type: 'string' as const }, principalId: { type: 'string' as const } },
          required: ['value', 'principalId'],
          additionalProperties: false,
        },
      },
    ],
  },
];

/** Two cached reads whose answers name the principal they are about. */
class PrincipalsClient extends BaseClient {
  readonly serviceName = 'identity';

  account(id: string): Promise<{ value: string; principalId: string }> {
    return this.request('GET', '/accounts/{id}', {
      params: { id },
      operationId: 'getAccount',
      successes: PRINCIPAL_SUCCESS,
    });
  }

  profile(id: string): Promise<{ value: string; principalId: string }> {
    return this.request('GET', '/profiles/{id}', {
      params: { id },
      operationId: 'getProfile',
      successes: PRINCIPAL_SUCCESS,
    });
  }
}

/** The injectable cache clock: a test moves it instead of sleeping. */
class Clock {
  now = Date.UTC(2026, 8, 18, 12);
  advance(ms: number): void {
    this.now += ms;
  }
}

/** A provider answering every call with the path it was asked, counting calls. */
class Provider {
  calls = 0;
  status = 0;
  /** When set, /accounts/gone answers the declared 404 refusal. */
  gone = false;
  /** When set, every answer also names the principal its path belongs to. */
  principals: Record<string, string> | undefined;
  private hold: { promise: Promise<void>; release: () => void } | undefined;
  private server: ReturnType<typeof Bun.serve>;

  constructor() {
    this.server = Bun.serve({
      port: 0,
      fetch: async (request) => {
        this.calls++;
        await this.hold?.promise;
        if (this.status) {
          return Response.json({ code: 'unavailable', error: 'Unavailable', message: 'down' }, { status: this.status });
        }
        if (this.gone && new URL(request.url).pathname === '/accounts/gone') {
          return Response.json({ code: 'not_found', error: 'Not Found', message: 'gone' }, { status: 404 });
        }
        const pathname = new URL(request.url).pathname;
        if (this.principals) return Response.json({ value: pathname, principalId: this.principals[pathname] ?? '' });
        return Response.json({ value: pathname });
      },
    });
  }

  get url(): string {
    return `http://localhost:${this.server.port}`;
  }

  holdAnswers(): void {
    let release = () => {};
    const promise = new Promise<void>((resolve) => {
      release = resolve;
    });
    this.hold = { promise, release };
  }

  release(): void {
    this.hold?.release();
    this.hold = undefined;
  }

  stop(): void {
    this.release();
    this.server.stop(true);
  }
}

const providers: Provider[] = [];
const apps: ReturnType<typeof application>[] = [];
/** The caches bind() injects: the low-level registration leaves their end to the caller. */
const caches: ServiceResponseCache[] = [];
let collector: TelemetryCollector;

beforeEach(() => {
  collector = new TelemetryCollector();
  setCollector(collector);
});

afterEach(async () => {
  setCollector(undefined);
  // End the shared calls a test left running before their provider goes away:
  // otherwise one retries against the closed port and records its failure in
  // whatever collector the next test file has installed.
  for (const cache of caches.splice(0)) cache.dispose();
  for (const provider of providers.splice(0)) provider.stop();
  await Promise.all(apps.splice(0).map((app) => app.stop()));
});

function provider(): Provider {
  const created = new Provider();
  providers.push(created);
  return created;
}

async function bind(
  url: string,
  operation: ClientContractOperation,
  clock: Clock,
): Promise<{ client: AccountsClient; cache: ServiceResponseCache }> {
  const cache = new ServiceResponseCache({ now: () => clock.now });
  caches.push(cache);
  const descriptor: GeneratedServiceDescriptor = {
    contract,
    service: 'IdentityService',
    operations: { getAccount: operation },
    transport: 'http',
  };
  const app = application();
  app.register(
    createServiceClientRegistration(
      AccountsClient,
      descriptor,
      { url, clientId: 'consumer', allowInsecure: true },
      undefined,
      cache,
    ),
  );
  await app.start();
  apps.push(app);
  return { client: app.context.get(AccountsClient), cache };
}

async function bindPrincipals(url: string): Promise<PrincipalsClient> {
  const declared: ClientCachePolicy = { freshMs: 5000, invalidationFields: ['principalId'] };
  const profile = accountOperation(declared);
  const descriptor: GeneratedServiceDescriptor = {
    contract,
    service: 'IdentityService',
    operations: {
      getAccount: accountOperation(declared),
      getProfile: { ...profile, transports: [{ protocol: 'rest-json', path: '/profiles/{id}', encoding: 'json' }] },
    },
    transport: 'http',
  };
  const app = application();
  app.register(
    createServiceClientRegistration(PrincipalsClient, descriptor, { url, clientId: 'consumer', allowInsecure: true }),
  );
  await app.start();
  apps.push(app);
  return app.context.get(PrincipalsClient);
}

/** A vector's JSON text read into the value a consumer holds. */
function nativeVectorValue(text: string): ResponseFieldValue {
  if (/^-?\d+$/.test(text)) {
    const number = Number(text);
    return Number.isSafeInteger(number) ? number : BigInt(text);
  }
  return JSON.parse(text) as ResponseFieldValue;
}

function staleServed(): number {
  let total = 0;
  for (const bucket of collector.drainAll())
    for (const series of bucket.counterSeries ?? []) if (series.name === STALE_SERVED_METRIC) total += series.value;
  return total;
}

async function until(condition: () => boolean): Promise<void> {
  const deadline = Date.now() + 10_000;
  while (!condition()) {
    if (Date.now() > deadline) throw new Error('condition was not reached');
    // biome-ignore lint/performance/noAwaitInLoops: each turn yields to the event loop until the provider has seen the call
    await new Promise<void>((resolve) => setImmediate(resolve));
  }
}

describe('generated client response cache', () => {
  specTest(
    'makes one upstream call per key and identity per fresh window under concurrent load',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'one-upstream-call-per-key-and-identity-per-fresh-window-under-concurrent-load',
    },
    async () => {
      const upstream = provider();
      upstream.holdAnswers();
      const clock = new Clock();
      const { client, cache } = await bind(upstream.url, accountOperation(FRESH_STALE), clock);

      const callers = 50;
      const answers = Array.from({ length: callers }, () => client.get('a1'));
      // Every caller is in flight at once before the provider answers: one
      // leads the call, the other 49 wait on it.
      await until(() => cache.waiting() === callers - 1);
      upstream.release();
      for (const answer of await Promise.all(answers)) expect(answer).toEqual({ value: '/accounts/a1' });
      expect(upstream.calls).toBe(1);

      clock.advance(4999);
      await client.get('a1');
      expect(upstream.calls).toBe(1);
      await client.get('a2');
      expect(upstream.calls).toBe(2);
      clock.advance(1);
      await client.get('a1');
      expect(upstream.calls).toBe(3);

      // Two forwarded identities never share an answer; one identity does.
      await runInContext({ __authorizationHeader: 'Bearer alice' }, () => client.get('shared'));
      await runInContext({ __authorizationHeader: 'Bearer bob' }, () => client.get('shared'));
      await runInContext({ __authorizationHeader: 'Bearer alice' }, () => client.get('shared'));
      expect(upstream.calls).toBe(5);
    },
  );

  specTest(
    'returns the stored answer while the provider is down inside the stale window and counts it',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'a-provider-failure-inside-the-stale-window-returns-the-stored-answer-and-is-counted',
    },
    async () => {
      const upstream = provider();
      const clock = new Clock();
      const { client } = await bind(upstream.url, accountOperation(FRESH_STALE, { retry: { maxAttempts: 1 } }), clock);
      expect(await client.get('a1')).toEqual({ value: '/accounts/a1' });

      upstream.stop();
      clock.advance(6000);
      expect(await client.get('a1')).toEqual({ value: '/accounts/a1' });
      expect(staleServed()).toBe(1);
      clock.advance(300_000 - 6000 - 1);
      expect(await client.get('a1')).toEqual({ value: '/accounts/a1' });
      expect(staleServed()).toBe(1);

      // At the stale bound the transport failure reaches the caller.
      clock.advance(1);
      const error = await client.get('a1').catch((caught: unknown) => caught);
      expect(error).toBeInstanceOf(ClientError);
      expect(staleServed()).toBe(0);
    },
  );

  specTest(
    'masks retry exhaustion and an open breaker but never a declared refusal or a canceled caller',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'a-failure-after-the-stale-window-or-outside-the-provider-failure-classes-reaches-the-caller',
    },
    async () => {
      const upstream = provider();
      const clock = new Clock();
      const { client } = await bind(
        upstream.url,
        accountOperation(FRESH_STALE, {
          retry: { maxAttempts: 1, statuses: [503] },
          circuit: { failureThreshold: 1, resetTimeoutMs: 60_000 },
        }),
        clock,
      );
      await client.get('a1');
      clock.advance(6000);

      upstream.status = 503;
      expect(await client.get('a1')).toEqual({ value: '/accounts/a1' });
      const calls = upstream.calls;
      expect(await client.get('a1')).toEqual({ value: '/accounts/a1' });
      expect(upstream.calls).toBe(calls);
      expect(staleServed()).toBe(2);

      const refusing = provider();
      const second = await bind(refusing.url, accountOperation(FRESH_STALE), clock);
      await second.client.get('gone');
      refusing.gone = true;
      clock.advance(6000);
      // A declared refusal is an answer, not an outage: the stored answer stays put.
      const refusal = await second.client.get('gone').catch((caught: unknown) => caught);
      expect(refusal).toBeInstanceOf(ClientFrameworkError);
      expect((refusal as ClientFrameworkError).status).toBe(404);

      const canceled = new AbortController();
      canceled.abort();
      await expect(client.get('a1', canceled.signal)).rejects.toBeInstanceOf(ClientCanceledError);
    },
  );

  specTest(
    'sends the next call upstream after an invalidation, even one that overtakes a call in flight',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'an-invalidation-sends-the-next-call-upstream' },
    async () => {
      const upstream = provider();
      const clock = new Clock();
      const { client } = await bind(upstream.url, accountOperation({ freshMs: 5000, keyFields: ['path.id'] }), clock);
      await client.get('a1');
      await client.get('a2');
      expect(client.invalidateResponses('getAccount?path.id=%22a1')).toBe(1);
      await client.get('a1');
      expect(upstream.calls).toBe(3);
      await client.get('a2');
      expect(upstream.calls).toBe(3);
      expect(client.invalidateResponses('')).toBe(2);

      upstream.holdAnswers();
      const inFlight = client.get('a3');
      await until(() => upstream.calls === 4);
      client.invalidateResponses('getAccount?path.id=%22a3');
      upstream.release();
      await inFlight;
      await client.get('a3');
      expect(upstream.calls).toBe(5);
    },
  );

  specTest(
    'drops every entry with the registry and refuses a late call',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'entries-never-cross-a-forwarded-identity-or-outlive-the-registry',
    },
    async () => {
      const upstream = provider();
      const clock = new Clock();
      const { client, cache } = await bind(upstream.url, accountOperation({ freshMs: 5000, maxEntries: 2 }), clock);
      await client.get('a1');
      await client.get('a2');
      await client.get('a3');
      await client.get('a1');
      expect(upstream.calls).toBe(4);

      cache.dispose();
      expect(cache.isDisposed).toBe(true);
      await expect(client.get('a1')).rejects.toBeInstanceOf(CredentialRegistryClosedError);

      // The application registry owns one cache per service and ends it with
      // the application.
      const app = application();
      registerServiceClient(
        app,
        AccountsClient,
        {
          contract,
          service: 'IdentityService',
          operations: { getAccount: accountOperation(FRESH_STALE) },
          transport: 'http',
        },
        { url: upstream.url, clientId: 'consumer', allowInsecure: true },
      );
      await app.start();
      const registered = app.context.get(AccountsClient);
      await registered.get('a1');
      await registered.get('a1');
      expect(upstream.calls).toBe(5);
      await app.stop();
      await expect(registered.get('a1')).rejects.toBeInstanceOf(CredentialRegistryClosedError);
    },
  );

  specTest(
    'aborts the shared call when the registry ends and tells its callers the registry closed',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'entries-never-cross-a-forwarded-identity-or-outlive-the-registry',
    },
    async () => {
      const upstream = provider();
      upstream.holdAnswers();
      const { client, cache } = await bind(upstream.url, accountOperation(FRESH_STALE), new Clock());
      const inFlight = client.get('a1');
      await until(() => upstream.calls === 1);
      // The provider never answers: only an aborted call can settle.
      cache.dispose();
      await expect(inFlight).rejects.toBeInstanceOf(CredentialRegistryClosedError);
    },
  );

  specTest(
    'returns the stored answer to a caller whose deadline passes while the provider hangs, never to a canceled one',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'a-caller-deadline-that-passes-while-the-provider-hangs-returns-the-stored-answer',
    },
    async () => {
      const upstream = provider();
      const clock = new Clock();
      const { client, cache } = await bind(upstream.url, accountOperation(FRESH_STALE), clock);
      await client.get('a1');
      staleServed();

      // A deadline carried by the caller's signal.
      clock.advance(6000);
      upstream.holdAnswers();
      const deadline = new AbortController();
      const late = client.get('a1', deadline.signal);
      await until(() => upstream.calls === 2);
      deadline.abort(new DOMException('caller deadline', 'TimeoutError'));
      expect(await late).toEqual({ value: '/accounts/a1' });
      expect(staleServed()).toBe(1);

      // The shared call kept running: a second caller joins it, and its answer
      // lands in the cache without another provider call.
      const joined = client.get('a1');
      await until(() => cache.waiting() === 1);
      upstream.release();
      expect(await joined).toEqual({ value: '/accounts/a1' });
      await client.get('a1');
      expect(upstream.calls).toBe(2);

      // A deadline carried by the ambient context.
      clock.advance(6000);
      upstream.holdAnswers();
      const ambient = await runInContext({ deadlineAt: Date.now() + 150 }, () => client.get('a1'));
      expect(ambient).toEqual({ value: '/accounts/a1' });
      expect(staleServed()).toBe(1);
      upstream.release();
      await client.get('a1');
      expect(upstream.calls).toBe(3);

      // An explicit cancellation is the caller's answer, never a stored one.
      clock.advance(6000);
      upstream.holdAnswers();
      const cancel = new AbortController();
      const canceled = client.get('a1', cancel.signal);
      await until(() => upstream.calls === 4);
      cancel.abort();
      await expect(canceled).rejects.toBeInstanceOf(ClientCanceledError);
      expect(staleServed()).toBe(0);
    },
  );

  specTest(
    'masks a transport failure but never a defect that happens to be a TypeError',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'a-failure-after-the-stale-window-or-outside-the-provider-failure-classes-reaches-the-caller',
    },
    async () => {
      const clock = new Clock();
      const interceptor = serviceResponseCacheInterceptor(
        'identity',
        new ServiceResponseCache({ now: () => clock.now }),
      );
      const request = (): ClientRequest => ({
        method: 'GET',
        path: '/accounts/{id}',
        params: { id: 'a1' },
        headers: new Headers(),
        operationId: 'getAccount',
        clientOperation: accountOperation(FRESH_STALE),
      });
      await interceptor(request(), async () => ({ data: { value: 'stored' }, status: 200, headers: new Headers() }));
      clock.advance(6000);

      const defect = new TypeError("Cannot read properties of undefined (reading 'value')");
      await expect(
        interceptor(request(), () => {
          throw defect;
        }),
      ).rejects.toBe(defect);
      const exhaustedByDefect = new ClientRetryExhaustedError({
        service: 'identity',
        method: 'getAccount',
        attempts: 2,
        lastError: new TypeError('decode is not a function'),
      });
      await expect(
        interceptor(request(), () => {
          throw exhaustedByDefect;
        }),
      ).rejects.toBe(exhaustedByDefect);
      expect(staleServed()).toBe(0);

      const outage = markTransportFailure(new TypeError('fetch failed'));
      const answer = await interceptor(request(), () => {
        throw outage;
      });
      expect(answer.data).toEqual({ value: 'stored' });
      expect(staleServed()).toBe(1);
    },
  );

  specTest(
    'renders a repeated header as one JSON array, as the Go runtime does',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'the-cache-key-matches-the-shared-vectors' },
    () => {
      const headers = new Headers();
      headers.append('X-Region', 'eu');
      headers.append('X-Region', 'us');
      const key = responseCacheKey('listRegions', {
        method: 'GET',
        path: '/regions',
        headers,
        operationId: 'listRegions',
        clientOperation: accountOperation({ freshMs: 5000 }),
      });
      expect(key).toBe('listRegions?header.x-region=%5B%22eu%22%2C%22us%22%5D');
    },
  );

  specTest(
    'renders every shared cache key vector byte for byte',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'the-cache-key-matches-the-shared-vectors' },
    () => {
      const vectors = JSON.parse(readFileSync(join(CONTRACT_ROOT, 'fixtures/cache/keys.json'), 'utf8')) as {
        cases: {
          name: string;
          operationId: string;
          keyFields?: string[];
          idempotencyKeyHeader?: string;
          request: {
            path?: Record<string, string>;
            query?: Record<string, string[]>;
            headers?: Record<string, string[]>;
            body?: string;
            binaryBody?: string;
          };
          key: string;
        }[];
      };
      expect(vectors.cases.length).toBeGreaterThan(0);
      for (const vector of vectors.cases) {
        const body =
          vector.request.binaryBody !== undefined
            ? JSON.stringify(vector.request.binaryBody)
            : vector.request.body
              ? canonicalizeJsonText(vector.request.body)
              : undefined;
        const key = canonicalCacheKey(vector.operationId, vector.keyFields, {
          keyHeader: vector.idempotencyKeyHeader,
          path: vector.request.path ?? {},
          query: vector.request.query ?? {},
          headers: vector.request.headers ?? {},
          body,
        });
        expect(key, vector.name).toBe(vector.key);
      }
    },
  );

  specTest(
    'sends a bypassed call to the provider without reading, storing or joining the cache',
    {
      feature: FEATURE,
      requirement: CONTROL_REQUIREMENT,
      check: 'a-bypassed-call-neither-reads-nor-stores-nor-joins-a-call-in-flight',
    },
    async () => {
      const upstream = provider();
      const clock = new Clock();
      const { client, cache } = await bind(
        upstream.url,
        accountOperation(FRESH_STALE, { retry: { maxAttempts: 1, statuses: [503] } }),
        clock,
      );

      // No read: a fresh stored answer does not stand in for the bypassed call.
      await client.get('a1');
      expect(await client.get('a1', undefined, true)).toEqual({ value: '/accounts/a1' });
      expect(upstream.calls).toBe(2);

      // No store: the bypassed answer is not kept for the next caller.
      await client.get('b1', undefined, true);
      expect(upstream.calls).toBe(3);
      await client.get('b1');
      expect(upstream.calls).toBe(4);

      // No stale answer: a bypassed call is told about the outage.
      clock.advance(6000);
      upstream.status = 503;
      await expect(client.get('a1', undefined, true)).rejects.toBeInstanceOf(ClientError);
      expect(await client.get('a1')).toEqual({ value: '/accounts/a1' });
      upstream.status = 0;

      // No join: a bypassed call does not wait on the shared call in flight.
      clock.advance(3_600_000);
      upstream.holdAnswers();
      const calls = upstream.calls;
      const shared = client.get('c1');
      await until(() => upstream.calls === calls + 1);
      const own = client.get('c1', undefined, true);
      await until(() => upstream.calls === calls + 2);
      expect(cache.waiting()).toBe(0);
      upstream.release();
      await Promise.all([shared, own]);
    },
  );

  specTest(
    'drops every answer carrying a declared response field value, across operations and identities',
    {
      feature: FEATURE,
      requirement: CONTROL_REQUIREMENT,
      check: 'an-invalidation-by-a-response-field-drops-every-answer-carrying-the-value',
    },
    async () => {
      const upstream = provider();
      upstream.principals = {
        '/accounts/a1': 'p1',
        '/accounts/a2': 'p1',
        '/accounts/a3': 'p2',
        '/profiles/p1': 'p1',
        '/accounts/a4': 'p1',
      };
      const client = await bindPrincipals(upstream.url);
      const store = async () => {
        for (const authorization of [undefined, 'Bearer alice']) {
          const calls = async () => {
            for (const id of ['a1', 'a2', 'a3']) await client.account(id);
            await client.profile('p1');
          };
          if (authorization) await runInContext({ __authorizationHeader: authorization }, calls);
          else await calls();
        }
      };
      await store();
      expect(upstream.calls).toBe(8);

      // Values compare in canonical form: the integer 1 and the string 'p1 '
      // are other values, and a field no operation declares tags nothing.
      expect(client.invalidateResponsesByField('principalId', 1)).toBe(0);
      expect(client.invalidateResponsesByField('principalId', 'p1 ')).toBe(0);
      expect(client.invalidateResponsesByField('value', '/accounts/a1')).toBe(0);

      // Across the service's operations and for every identity.
      expect(client.invalidateResponsesByField('principalId', 'p1')).toBe(6);
      await store();
      expect(upstream.calls).toBe(14);
      expect(client.invalidateResponsesByField('principalId', 'p2')).toBe(2);

      // An invalidation that overtakes a call in flight keeps its answer out of
      // the cache, whatever that answer turns out to carry.
      upstream.holdAnswers();
      const inFlight = client.account('a4');
      await until(() => upstream.calls === 15);
      client.invalidateResponsesByField('principalId', 'nobody');
      upstream.release();
      await inFlight;
      await client.account('a4');
      expect(upstream.calls).toBe(16);

      // A value no runtime can compare is refused, never matched against nothing.
      for (const refused of [1.5, 2 ** 53, Number.NaN, null, ['p1'], { id: 'p1' }]) {
        expect(() => client.invalidateResponsesByField('principalId', refused as never)).toThrow(TypeError);
      }
      for (const field of ['', ' principalId', 'principal\u0000Id']) {
        expect(() => client.invalidateResponsesByField(field, 'p1')).toThrow(TypeError);
      }
    },
  );

  specTest(
    'renders every shared response-field invalidation vector byte for byte',
    { feature: FEATURE, requirement: CONTROL_REQUIREMENT, check: 'the-response-field-tags-match-the-shared-vectors' },
    () => {
      const vectors = JSON.parse(readFileSync(join(CONTRACT_ROOT, 'fixtures/cache/invalidation.json'), 'utf8')) as {
        values: { name: string; value: string; tag: string }[];
        refused: { name: string; value: string }[];
        responses: { name: string; invalidationFields: string[]; body: string; tags: Record<string, string> }[];
      };
      expect(vectors.values.length).toBeGreaterThan(0);
      expect(vectors.refused.length).toBeGreaterThan(0);
      expect(vectors.responses.length).toBeGreaterThan(0);
      for (const vector of vectors.values) {
        const value = nativeVectorValue(vector.value);
        expect(invalidationTag('field', value), vector.name).toBe(vector.tag);
        // An integer held as a bigint renders the same tag as the number.
        if (typeof value === 'number')
          expect(invalidationTag('field', BigInt(vector.value)), vector.name).toBe(vector.tag);
      }
      for (const vector of vectors.refused) {
        expect(() => invalidationTag('field', nativeVectorValue(vector.value)), vector.name).toThrow(TypeError);
      }
      for (const vector of vectors.responses) {
        // The raw parse keeps every integer digit, as the Go runtime's decoder does.
        const tags = responseTags(parseJsonValue(vector.body), vector.invalidationFields);
        expect(Object.fromEntries(tags), vector.name).toEqual(vector.tags);
      }
      // A schema-decoded answer holds a wide integer as a bigint and a narrow
      // one as a number: both tag as their digits.
      expect(
        Object.fromEntries(responseTags({ version: 12345678901234567890n, zero: -0 }, ['version', 'zero'])),
      ).toEqual({
        version: '12345678901234567890',
        zero: '0',
      });
    },
  );

  specTest(
    'implements exactly the capabilities the contract publishes',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'the-runtime-implements-exactly-the-published-capabilities' },
    () => {
      const schema = JSON.parse(
        readFileSync(join(CONTRACT_ROOT, 'schemas/generated-client-manifest-v1.json'), 'utf8'),
      ) as { properties: { runtimeCapabilities: { items: { enum: string[] } } } };
      expect([...CLIENT_RUNTIME_CAPABILITIES]).toEqual(schema.properties.runtimeCapabilities.items.enum);
      expect(() => requireClientRuntimeCapabilities(['response-cache'])).not.toThrow();
      expect(() => requireClientRuntimeCapabilities(['response-cache-v2'])).toThrow(/response-cache-v2/);
    },
  );
});
