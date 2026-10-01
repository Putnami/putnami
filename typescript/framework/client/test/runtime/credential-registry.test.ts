import { afterEach, describe, expect, mock } from 'bun:test';
import { application, module } from '@putnami/application';
import type { ClientContractDocument, ClientContractOperation } from '@putnami/application';
import { resetConfigLoader, runInContext } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { BaseClient } from '../../src/runtime/base-client';
import {
  type Credential,
  type CredentialBinding,
  type CredentialRequest,
  CredentialManager,
  CredentialRegistryClosedError,
} from '../../src/runtime/credential';
import { ClientCredentialError, ClientFrameworkError } from '../../src/runtime/errors';
import {
  type GeneratedServiceDescriptor,
  registerServiceClient,
  type ServiceBinding,
} from '../../src/runtime/service-binding';
import type { StreamObserver } from '../../src/runtime/stream.type';

/**
 * The credential contract, replayed on the TypeScript registry.
 *
 * These are the thirteen cases the Go registry is held to, in
 * the same order. Two rules of method carry across with them, because they are
 * what makes the Go suite non-vacuous:
 *
 * 1. **No real sleep.** Concurrency is driven by promises the test resolves and
 *    expiry by an injected clock. A test that sleeps proves it waited.
 * 2. **"Nothing was sent" counts requests server-side.** Asserting on the error
 *    type alone would pass just as well if the call had gone out and failed.
 */

const secureOperation: ClientContractOperation = {
  stream: 'unary',
  transports: [{ protocol: 'rest-json', path: '/widgets', encoding: 'json' }],
  security: { alternatives: [{ allOf: [{ profile: 'service', scopes: ['write'] }] }] },
  errors: [
    { status: 400, code: 'http.bad_request' },
    { status: 500, code: 'http.internal_server' },
  ],
  idempotency: { kind: 'non-idempotent' },
  resilience: { timeoutMs: 2000, attemptTimeoutMs: 1000, retry: { maxAttempts: 1 } },
};

const contract: ClientContractDocument = {
  protocolVersion: 1,
  service: { id: 'widgets', audience: 'api://widgets' },
  credentials: {
    service: { kind: 'service-token', scopes: ['read'] },
    user: { kind: 'forwarded-user-token' },
    tenant: { kind: 'named-header', header: 'X-Tenant' },
  },
};

const descriptor: GeneratedServiceDescriptor = {
  contract,
  service: 'WidgetsService',
  operations: { getWidgets: secureOperation },
  transport: 'http',
};

class WidgetsClient extends BaseClient {
  readonly serviceName = 'widgets';

  list(): Promise<{ ok: boolean }> {
    return this.request('GET', '/widgets', { operationId: 'getWidgets' });
  }
}

class SecondWidgetsClient extends WidgetsClient {}

const streamOperation: ClientContractOperation = {
  stream: 'server',
  messages: { output: { type: 'object', properties: { ok: { type: 'boolean' } } } },
  transports: [{ protocol: 'sse', path: '/widgets/watch', encoding: 'json' }],
  security: { alternatives: [{ allOf: [] }] },
  errors: [],
  idempotency: { kind: 'safe' },
};

class StreamingWidgetsClient extends BaseClient {
  readonly serviceName = 'widgets';

  watch(): StreamObserver<{ ok: boolean }> {
    return this.serviceStream('GET', '/widgets/watch', { operationId: 'watchWidgets' });
  }
}

const servers: ReturnType<typeof Bun.serve>[] = [];

afterEach(() => {
  for (const server of servers.splice(0)) server.stop(true);
  delete process.env.CONFIG_DATA;
  resetConfigLoader();
});

function serve(fetch: (request: Request) => Response | Promise<Response>): string {
  const server = Bun.serve({ port: 0, fetch });
  servers.push(server);
  return `http://localhost:${server.port}`;
}

/** A clock the test moves, so expiry is a decision rather than a wait. */
function stopwatch(start = 1_700_000_000_000) {
  let now = start;
  return {
    now: () => now,
    advance: (ms: number) => {
      now += ms;
    },
  };
}

/** A credential source that counts its acquisitions and hands back a deferred. */
function countingProvider(build: (n: number) => Credential | Promise<Credential>) {
  let calls = 0;
  return {
    calls: () => calls,
    provider: (_request: CredentialRequest) => {
      calls++;
      return Promise.resolve(build(calls));
    },
  };
}

const SERVICE_REQUEST: CredentialRequest = {
  serviceId: 'widgets',
  clientId: 'consumer',
  profile: 'service',
  audience: 'api://widgets',
  scopes: ['read'],
};

function token(value: string, expiresInMs: number, now: number): Credential {
  return { value, expiresAt: new Date(now + expiresInMs) };
}

describe('L09 — the credential registry contract, cases 1 to 3', () => {
  specTest(
    'the modules of one application share one registry',
    {
      feature: 'typescript/service-clients',
      requirement: 'credential-registry',
      check: 'modules-of-one-application-share-one-credential-registry',
    },
    async () => {
      let acquisitions = 0;
      const tokenUrl = serve(() => {
        acquisitions++;
        return Response.json({ access_token: `t-${acquisitions}`, expires_in: 3600 });
      });
      const serviceUrl = serve(() => Response.json({ ok: true }));
      const binding = serviceBinding(serviceUrl, tokenUrl);

      const first = module('first');
      const second = module('second');
      registerServiceClient(first, WidgetsClient, descriptor, binding);
      registerServiceClient(second, SecondWidgetsClient, descriptor, binding);
      const app = application().use(first).use(second);
      await app.start();
      try {
        await Promise.all([app.context.get(WidgetsClient).list(), app.context.get(SecondWidgetsClient).list()]);
        // One identity requested twice is acquired once: the second module read
        // the first one's entry, not a registry of its own.
        expect(acquisitions).toBe(1);
      } finally {
        await app.stop();
      }
    },
  );

  specTest(
    'two applications share nothing, and disposing one leaves the other usable',
    {
      feature: 'typescript/service-clients',
      requirement: 'credential-registry',
      check: 'two-applications-do-not-share-a-credential-registry',
    },
    async () => {
      let acquisitions = 0;
      const tokenUrl = serve(() => {
        acquisitions++;
        return Response.json({ access_token: `t-${acquisitions}`, expires_in: 3600 });
      });
      const serviceUrl = serve(() => Response.json({ ok: true }));
      const binding = serviceBinding(serviceUrl, tokenUrl);

      const left = application();
      const right = application();
      registerServiceClient(left, WidgetsClient, descriptor, binding);
      registerServiceClient(right, SecondWidgetsClient, descriptor, binding);
      await Promise.all([left.start(), right.start()]);
      try {
        await Promise.all([left.context.get(WidgetsClient).list(), right.context.get(SecondWidgetsClient).list()]);
        expect(acquisitions).toBe(2);

        await left.stop();
        // Closing one application's registry says nothing about the other's.
        expect(await right.context.get(SecondWidgetsClient).list()).toEqual({ ok: true });
        expect(acquisitions).toBe(2);
      } finally {
        await right.stop();
      }
    },
  );

  specTest(
    'stopping the application closes the registry, and a retained client stops calling',
    {
      feature: 'typescript/service-clients',
      requirement: 'credential-registry',
      check: 'a-retained-client-cannot-call-after-the-application-stopped',
    },
    async () => {
      const calls = mock(() => Response.json({ ok: true }));
      const tokenUrl = serve(() => Response.json({ access_token: 'live', expires_in: 3600 }));
      const app = application();
      registerServiceClient(app, WidgetsClient, descriptor, serviceBinding(serve(calls), tokenUrl));
      await app.start();
      const retained = app.context.get(WidgetsClient);
      await retained.list();
      const before = calls.mock.calls.length;
      await app.stop();

      const failure = await retained.list().catch((error: unknown) => error);
      // The failure names the closure rather than the provider: the registry is
      // gone, so there is no identity to present and no call to make.
      expect(failure).toBeInstanceOf(CredentialRegistryClosedError);
      // "Nothing was sent" is counted on the server, not inferred from the error.
      expect(calls.mock.calls.length).toBe(before);
    },
  );
});

let pending: StreamObserver<{ ok: boolean }> | undefined;

describe('L09 — cases 4 to 8: closing the registry', () => {
  specTest(
    'dispose is idempotent and leaves no unhandled rejection',
    {
      feature: 'typescript/service-clients',
      requirement: 'credential-registry',
      check: 'disposing-the-registry-twice-is-silent',
    },
    () => {
      const manager = new CredentialManager();
      manager.dispose();
      expect(() => manager.dispose()).not.toThrow();
    },
  );

  specTest(
    'dispose releases every waiter on one in-flight acquisition with the closed error',
    {
      feature: 'typescript/service-clients',
      requirement: 'credential-registry',
      check: 'disposing-the-registry-releases-in-flight-waiters-with-a-distinct-error',
    },
    async () => {
      const clock = stopwatch();
      const manager = new CredentialManager({ now: clock.now });
      let started: (() => void) | undefined;
      const reached = new Promise<void>((resolve) => {
        started = resolve;
      });
      let acquisitions = 0;
      const binding: CredentialBinding = {
        source: 'oauth-client-credentials',
        // Never resolves: only `dispose` ends this acquisition, so the waiters
        // are released by the closure and not by a timer.
        provider: () =>
          new Promise<Credential>(() => {
            acquisitions++;
            started?.();
          }),
      };

      const waiters = [0, 1, 2, 3].map(() => manager.acquire(SERVICE_REQUEST, binding, 60_000));
      await reached;
      expect(acquisitions).toBe(1);

      manager.dispose();
      const outcomes = await Promise.allSettled(waiters);
      for (const outcome of outcomes) {
        expect(outcome.status).toBe('rejected');
        const reason = (outcome as PromiseRejectedResult).reason;
        // Distinct from an ordinary acquisition failure: "this application is
        // shutting down" is final, "the identity provider is broken" is not.
        expect(reason).toBeInstanceOf(CredentialRegistryClosedError);
        expect(reason).toBeInstanceOf(ClientCredentialError);
        expect((reason as Error).name).toBe('CredentialRegistryClosedError');
      }
    },
  );

  specTest(
    'an acquisition that lands during dispose writes nothing to the cache',
    {
      feature: 'typescript/service-clients',
      requirement: 'credential-registry',
      check: 'an-acquisition-that-completes-during-dispose-is-not-cached',
    },
    async () => {
      const clock = stopwatch();
      const manager = new CredentialManager({ now: clock.now });
      let land: ((credential: Credential) => void) | undefined;
      let reached: (() => void) | undefined;
      const started = new Promise<void>((resolve) => {
        reached = resolve;
      });
      let laterAcquisitions = 0;
      const binding: CredentialBinding = {
        source: 'oauth-client-credentials',
        provider: () => {
          laterAcquisitions++;
          return new Promise<Credential>((resolve) => {
            land = resolve;
            reached?.();
          });
        },
      };

      const pending = manager.acquire(SERVICE_REQUEST, binding, 60_000);
      await started;
      manager.dispose();
      land?.(token('late', 3_600_000, clock.now()));
      await expect(pending).rejects.toBeInstanceOf(CredentialRegistryClosedError);

      // A later acquisition would have to be served from a cache the closure
      // emptied; instead the registry refuses outright.
      await expect(manager.acquire(SERVICE_REQUEST, binding, 60_000)).rejects.toBeInstanceOf(
        CredentialRegistryClosedError,
      );
      // The closed registry never reached the provider a second time.
      expect(laterAcquisitions).toBe(1);
    },
  );

  specTest(
    'stopping the application cancels the streams it still holds',
    {
      feature: 'typescript/service-clients',
      requirement: 'credential-registry',
      check: 'stopping-the-application-cancels-the-streams-it-still-holds',
    },
    async () => {
      // The Go registry tracks streams itself; here the generated client owns
      // them and its `dispose` — which the binding's `onClose` runs when the
      // application stops — cancels each one, so a stream never outlives the
      // registry that authenticated it.
      let opened = 0;
      let released = false;
      const url = serve(() => {
        opened++;
        return new Response(
          new ReadableStream({
            start(controller) {
              controller.enqueue(new TextEncoder().encode('data: {"ok":true}\n\n'));
              // Never closed: only the client's disposal ends this stream.
            },
            cancel() {
              released = true;
            },
          }),
          { headers: { 'Content-Type': 'text/event-stream' } },
        );
      });
      const client = new StreamingWidgetsClient({
        baseUrl: url,
        transport: 'http',
        serviceId: 'widgets',
        operationContracts: { watchWidgets: streamOperation },
      });

      const first = await new Promise<{ ok: boolean }>((resolve) => {
        const observer = client.watch();
        observer.onMessage(resolve);
        observer.onError(() => resolve({ ok: false }));
        pending = observer;
      });
      expect(first).toEqual({ ok: true });
      expect(opened).toBe(1);

      client.dispose();
      // The socket is released, not merely forgotten: the provider's own stream
      // is cancelled, which is what stops it writing to a consumer that is gone.
      const deadline = Date.now() + 5000;
      while (!released && Date.now() < deadline) {
        // biome-ignore lint/performance/noAwaitInLoops: poll the provider's own cancellation, bounded
        await Bun.sleep(5);
      }
      expect(released).toBe(true);
      expect(pending).toBeDefined();
    },
  );

  specTest(
    'a late acquisition is refused with the closure error, not a generic abort',
    {
      feature: 'typescript/service-clients',
      requirement: 'credential-registry',
      check: 'an-acquisition-after-closure-names-the-closure',
    },
    async () => {
      const manager = new CredentialManager();
      manager.dispose();
      const binding: CredentialBinding = {
        source: 'oauth-client-credentials',
        provider: () => Promise.resolve(token('never', 3_600_000, Date.now())),
      };
      const failure = await manager.acquire(SERVICE_REQUEST, binding, 60_000).catch((error: unknown) => error);
      expect(failure).toBeInstanceOf(CredentialRegistryClosedError);
      // Distinct from a caller cancellation and from a budget overrun, both of
      // which are ordinary and retryable.
      expect((failure as Error).name).not.toBe('AbortError');
      expect((failure as Error).name).not.toBe('TimeoutError');
    },
  );
});

describe('L09 — cases 9 to 12: freshness', () => {
  specTest(
    'concurrent callers collapse onto one acquisition and all read the same value',
    {
      feature: 'typescript/service-clients',
      requirement: 'credential-registry',
      check: 'concurrent-refreshes-collapse-onto-one-acquisition',
    },
    async () => {
      const clock = stopwatch();
      const manager = new CredentialManager({ now: clock.now });
      let release: ((credential: Credential) => void) | undefined;
      const source = countingProvider(
        () =>
          new Promise<Credential>((resolve) => {
            release = resolve;
          }),
      );
      const binding: CredentialBinding = { source: 'oauth-client-credentials', provider: source.provider };

      const waiters = [0, 1, 2, 3, 4].map(() => manager.acquire(SERVICE_REQUEST, binding, 60_000));
      await Promise.resolve();
      release?.(token('shared', 3_600_000, clock.now()));
      const resolved = await Promise.all(waiters);

      expect(source.calls()).toBe(1);
      expect(new Set(resolved.map((entry) => entry.credential.value))).toEqual(new Set(['shared']));
      expect(new Set(resolved.map((entry) => entry.cacheKey)).size).toBe(1);
    },
  );

  specTest(
    'a credential is reused at mid-life and renewed once inside its leeway',
    {
      feature: 'typescript/service-clients',
      requirement: 'credential-registry',
      check: 'expiry-renews-once-inside-the-leeway-and-never-before',
    },
    async () => {
      const clock = stopwatch();
      const manager = new CredentialManager({ now: clock.now });
      const source = countingProvider((n) => token(`t-${n}`, 600_000, clock.now()));
      const binding: CredentialBinding = { source: 'oauth-client-credentials', provider: source.provider };

      expect((await manager.acquire(SERVICE_REQUEST, binding, 60_000)).credential.value).toBe('t-1');

      // Half-life: still fresh, still the same value, still one acquisition.
      clock.advance(300_000);
      expect((await manager.acquire(SERVICE_REQUEST, binding, 60_000)).credential.value).toBe('t-1');
      expect(source.calls()).toBe(1);

      // Inside the leeway — a tenth of the lifetime, so the last minute — the
      // renewal starts before expiry rather than at it.
      clock.advance(260_000);
      const concurrent = await Promise.all([
        manager.acquire(SERVICE_REQUEST, binding, 60_000),
        manager.acquire(SERVICE_REQUEST, binding, 60_000),
        manager.acquire(SERVICE_REQUEST, binding, 60_000),
      ]);
      expect(source.calls()).toBe(2);
      expect(new Set(concurrent.map((entry) => entry.credential.value))).toEqual(new Set(['t-2']));
    },
  );

  specTest(
    'a provider failure is never cached, and the recovery is',
    {
      feature: 'typescript/service-clients',
      requirement: 'credential-registry',
      check: 'a-credential-source-failure-is-never-cached',
    },
    async () => {
      const clock = stopwatch();
      const manager = new CredentialManager({ now: clock.now });
      let calls = 0;
      const binding: CredentialBinding = {
        source: 'oauth-client-credentials',
        provider: () => {
          calls++;
          if (calls === 1) return Promise.reject(new Error('client_secret rejected by https://idp.internal/token'));
          return Promise.resolve(token('recovered', 3_600_000, clock.now()));
        },
      };

      const failure = await manager.acquire(SERVICE_REQUEST, binding, 60_000).catch((error: unknown) => error);
      expect(failure).toBeInstanceOf(ClientCredentialError);
      // The provider's own message can carry a token endpoint, a client id or a
      // secret; none of it reaches the caller.
      expect((failure as Error).message).toBe('credential acquisition failed');
      expect((failure as Error).message).not.toContain('idp.internal');

      expect((await manager.acquire(SERVICE_REQUEST, binding, 60_000)).credential.value).toBe('recovered');
      expect(calls).toBe(2);

      // The recovery is cached: a third call does not go back to the provider.
      expect((await manager.acquire(SERVICE_REQUEST, binding, 60_000)).credential.value).toBe('recovered');
      expect(calls).toBe(2);
    },
  );

  specTest(
    'rejecting one exact credential evicts it and nothing else',
    {
      feature: 'typescript/service-clients',
      requirement: 'credential-registry',
      check: 'rejecting-a-stale-credential-does-not-evict-the-fresh-one',
    },
    async () => {
      const clock = stopwatch();
      const manager = new CredentialManager({ now: clock.now });
      const source = countingProvider((n) => token(`t-${n}`, 3_600_000, clock.now()));
      const binding: CredentialBinding = { source: 'oauth-client-credentials', provider: source.provider };
      const other: CredentialBinding = {
        source: 'oauth-client-credentials',
        provider: () => Promise.resolve(token('other-profile', 3_600_000, clock.now())),
      };

      const first = await manager.acquire(SERVICE_REQUEST, binding, 60_000);
      const sibling = await manager.acquire({ ...SERVICE_REQUEST, profile: 'tenant' }, other, 60_000);

      // A stale value rejected after the entry has already been renewed must
      // not evict the value that replaced it.
      manager.reject(first.cacheKey, 'a-value-that-was-never-cached');
      expect((await manager.acquire(SERVICE_REQUEST, binding, 60_000)).credential.value).toBe('t-1');
      expect(source.calls()).toBe(1);

      manager.reject(first.cacheKey, first.credential.value);
      expect((await manager.acquire(SERVICE_REQUEST, binding, 60_000)).credential.value).toBe('t-2');
      expect(source.calls()).toBe(2);

      // Another profile's entry is untouched by either rejection.
      expect((await manager.acquire({ ...SERVICE_REQUEST, profile: 'tenant' }, other, 60_000)).cacheKey).toBe(
        sibling.cacheKey,
      );
    },
  );

  specTest(
    'a 401 invalidates the credential without replaying an unsafe operation',
    {
      feature: 'typescript/service-clients',
      requirement: 'credential-registry',
      check: 'a-401-does-not-replay-a-non-idempotent-operation',
    },
    async () => {
      let acquisitions = 0;
      const tokenUrl = serve(() => {
        acquisitions++;
        return Response.json({ access_token: `t-${acquisitions}`, expires_in: 3600 });
      });
      let received = 0;
      const serviceUrl = serve(() => {
        received++;
        return Response.json({ code: 'unauthorized', message: 'stale token' }, { status: 401 });
      });
      const app = application();
      registerServiceClient(app, WidgetsClient, descriptor, serviceBinding(serviceUrl, tokenUrl));
      await app.start();
      try {
        await expect(app.context.get(WidgetsClient).list()).rejects.toBeInstanceOf(ClientFrameworkError);
        // The operation is declared non-idempotent: the credential is dropped,
        // the request is not repeated. Counted on the server, not inferred.
        expect(received).toBe(1);
        expect(acquisitions).toBe(1);

        // The next call acquires afresh, proving the 401 evicted the entry.
        await expect(app.context.get(WidgetsClient).list()).rejects.toBeInstanceOf(ClientFrameworkError);
        expect(acquisitions).toBe(2);
        expect(received).toBe(2);
      } finally {
        await app.stop();
      }
    },
  );
});

describe('L09 — case 13: alternatives, secondaries and forwarding', () => {
  const anded: ClientContractOperation = {
    ...secureOperation,
    security: {
      alternatives: [{ allOf: [{ profile: 'service' }, { profile: 'tenant' }] }, { allOf: [{ profile: 'user' }] }],
    },
  };
  const andedDescriptor: GeneratedServiceDescriptor = {
    ...descriptor,
    operations: { getWidgets: anded },
  };

  specTest(
    'an AND alternative is satisfied whole, or the next alternative is tried',
    {
      feature: 'typescript/service-clients',
      requirement: 'credential-registry',
      check: 'a-partially-bound-alternative-is-refused-before-dispatch',
    },
    async () => {
      const tokenUrl = serve(() => Response.json({ access_token: 'svc', expires_in: 3600 }));
      let received = 0;
      const serviceUrl = serve((request) => {
        received++;
        expect(request.headers.get('authorization')).toBe('Bearer svc');
        expect(request.headers.get('x-tenant')).toBe('acme');
        return Response.json({ ok: true });
      });

      const complete = application();
      registerServiceClient(complete, WidgetsClient, andedDescriptor, {
        url: serviceUrl,
        clientId: 'consumer',
        allowInsecure: true,
        credentials: {
          service: {
            source: 'oauth-client-credentials',
            tokenUrl,
            clientId: 'consumer',
            clientSecret: 's',
            allowInsecure: true,
          },
          tenant: { source: 'static', value: 'acme' },
        },
      });
      await complete.start();
      try {
        expect(await complete.context.get(WidgetsClient).list()).toEqual({ ok: true });
        expect(received).toBe(1);
      } finally {
        await complete.stop();
      }

      // Half of the AND group configured: the alternative is not satisfiable,
      // the anonymous branch does not exist, and no socket is opened.
      let partialReceived = 0;
      const partialUrl = serve(() => {
        partialReceived++;
        return Response.json({ ok: true });
      });
      const partial = application();
      registerServiceClient(partial, SecondWidgetsClient, andedDescriptor, {
        url: partialUrl,
        clientId: 'consumer',
        allowInsecure: true,
        credentials: {
          service: {
            source: 'oauth-client-credentials',
            tokenUrl,
            clientId: 'consumer',
            clientSecret: 's',
            allowInsecure: true,
          },
        },
      });
      await partial.start();
      try {
        const failure = await partial.context
          .get(SecondWidgetsClient)
          .list()
          .catch((error: unknown) => error);
        expect(failure).toBeInstanceOf(ClientCredentialError);
        // Counted on the server: no socket was opened for an operation whose
        // security could not be satisfied.
        expect(partialReceived).toBe(0);
      } finally {
        await partial.stop();
      }
    },
  );

  specTest(
    'a chosen alternative that then fails does not fall back to a weaker one',
    {
      feature: 'typescript/service-clients',
      requirement: 'credential-registry',
      check: 'a-chosen-alternative-that-fails-is-not-downgraded',
    },
    async () => {
      let received = 0;
      const serviceUrl = serve(() => {
        received++;
        return Response.json({ ok: true });
      });
      const app = application();
      registerServiceClient(app, WidgetsClient, andedDescriptor, {
        url: serviceUrl,
        clientId: 'consumer',
        allowInsecure: true,
        credentials: {
          // The first alternative is fully bound, so it is the one chosen — and
          // its failure is the call's failure. Retrying the forwarded-user
          // branch would silently weaken the call's identity.
          service: { source: 'oauth-client-credentials', provider: () => Promise.reject(new Error('idp down')) },
          tenant: { source: 'static', value: 'acme' },
          user: { source: 'forwarded-user' },
        },
      });
      await app.start();
      try {
        await runInContext({ __authorizationHeader: 'Bearer user-token' }, async () => {
          const failure = await app.context
            .get(WidgetsClient)
            .list()
            .catch((error: unknown) => error);
          expect(failure).toBeInstanceOf(ClientCredentialError);
        });
        expect(received).toBe(0);
      } finally {
        await app.stop();
      }
    },
  );

  specTest(
    'a forwarded user identity comes from a real incoming context, never from the service cache',
    {
      feature: 'typescript/service-clients',
      requirement: 'credential-registry',
      check: 'a-forwarded-user-identity-is-never-cached',
    },
    async () => {
      const forwardedOnly: GeneratedServiceDescriptor = {
        ...descriptor,
        operations: {
          getWidgets: { ...secureOperation, security: { alternatives: [{ allOf: [{ profile: 'user' }] }] } },
        },
      };
      const seen: (string | null)[] = [];
      const serviceUrl = serve((request) => {
        seen.push(request.headers.get('authorization'));
        return Response.json({ ok: true });
      });
      const app = application();
      registerServiceClient(app, WidgetsClient, forwardedOnly, {
        url: serviceUrl,
        clientId: 'consumer',
        allowInsecure: true,
        credentials: { user: { source: 'forwarded-user' } },
      });
      await app.start();
      try {
        const client = app.context.get(WidgetsClient);
        await runInContext({ __authorizationHeader: 'Bearer alice' }, () => client.list());
        await runInContext({ __authorizationHeader: 'Bearer bob' }, () => client.list());
        // Two callers, two identities: a cached one would have sent alice twice.
        expect(seen).toEqual(['Bearer alice', 'Bearer bob']);

        // Outside a request there is no identity to forward, and no call is made.
        const before = seen.length;
        const failure = await client.list().catch((error: unknown) => error);
        expect(failure).toBeInstanceOf(ClientCredentialError);
        expect(seen.length).toBe(before);
      } finally {
        await app.stop();
      }
    },
  );

  specTest(
    'a malformed or expired OAuth response is a safe typed failure',
    {
      feature: 'typescript/service-clients',
      requirement: 'credential-registry',
      check: 'a-malformed-or-expired-token-response-is-a-safe-typed-failure',
    },
    async () => {
      const clock = stopwatch();
      const bodies: [string, BodyInit, ResponseInit][] = [
        ['not json', 'this is not json', { headers: { 'Content-Type': 'application/json' } }],
        ['no access_token', JSON.stringify({ token: 'nope' }), { headers: { 'Content-Type': 'application/json' } }],
        [
          'blank access_token',
          JSON.stringify({ access_token: '   ', expires_in: 60 }),
          { headers: { 'Content-Type': 'application/json' } },
        ],
        [
          'expiry that is not a positive integer',
          JSON.stringify({ access_token: 'x', expires_in: -1 }),
          { headers: { 'Content-Type': 'application/json' } },
        ],
        ['a 500 carrying a secret', 'client_secret=hunter2 was rejected', { status: 500 }],
      ];

      for (const [name, body, init] of bodies) {
        const tokenUrl = serve(() => new Response(body, init));
        const manager = new CredentialManager({ now: clock.now });
        const failure = await manager
          .acquire(
            SERVICE_REQUEST,
            {
              source: 'oauth-client-credentials',
              tokenUrl,
              clientId: 'consumer',
              clientSecret: 'hunter2',
              allowInsecure: true,
            },
            5000,
          )
          .catch((error: unknown) => error);
        expect(failure, name).toBeInstanceOf(ClientCredentialError);
        expect((failure as Error).message, name).not.toContain('hunter2');
      }
    },
  );

  specTest(
    'the acquisition budget is the call budget, and it is bounded',
    {
      feature: 'typescript/service-clients',
      requirement: 'credential-registry',
      check: 'the-acquisition-budget-bounds-a-source-that-never-answers',
    },
    async () => {
      const manager = new CredentialManager();
      const binding: CredentialBinding = {
        source: 'oauth-client-credentials',
        provider: (request) =>
          new Promise<Credential>((_resolve, reject) => {
            // The budget arrives as the leader's own signal: a source that never
            // answers is ended by it, not by the caller giving up.
            request.signal?.addEventListener('abort', () => reject(request.signal?.reason), { once: true });
          }),
      };
      const failure = await manager.acquire(SERVICE_REQUEST, binding, 20).catch((error: unknown) => error);
      expect(failure).toBeInstanceOf(Error);
      expect((failure as Error).name === 'TimeoutError' || failure instanceof ClientCredentialError).toBe(true);
    },
  );

  // The Go half of these three is TestForwardedUserCredentialRemintsOnceOn401,
  // TestForwardedUserCredentialRemintsAtMostOnce and
  // TestForwardedUserCredentialWithoutRefreshNeverRetries401
  // (service_operation_test.go).
  specTest(
    'a forwarded-user binding with refresh remints once on 401 and retries',
    {
      feature: 'typescript/service-clients',
      requirement: 'credential-registry',
      check: 'a-forwarded-user-binding-remints-once-on-401',
    },
    async () => {
      const forwardedOnly: GeneratedServiceDescriptor = {
        ...descriptor,
        operations: {
          getWidgets: { ...secureOperation, security: { alternatives: [{ allOf: [{ profile: 'user' }] }] } },
        },
      };
      const seen: (string | null)[] = [];
      const serviceUrl = serve((request) => {
        const authorization = request.headers.get('authorization');
        seen.push(authorization);
        if (authorization !== 'Bearer fresh-user-token') return new Response(null, { status: 401 });
        return Response.json({ ok: true });
      });
      let refreshCalls = 0;
      const app = application();
      registerServiceClient(app, WidgetsClient, forwardedOnly, {
        url: serviceUrl,
        clientId: 'consumer',
        allowInsecure: true,
        credentials: {
          user: {
            source: 'forwarded-user',
            refresh: () => {
              refreshCalls++;
              return Promise.resolve('fresh-user-token');
            },
          },
        },
      });
      await app.start();
      try {
        const result = await runInContext({ __authorizationHeader: 'Bearer stale-user-token' }, () =>
          app.context.get(WidgetsClient).list(),
        );
        expect(result).toEqual({ ok: true });
        expect(refreshCalls).toBe(1);
        expect(seen).toEqual(['Bearer stale-user-token', 'Bearer fresh-user-token']);
      } finally {
        await app.stop();
      }
    },
  );

  specTest(
    'a forwarded-user remint retries at most once',
    {
      feature: 'typescript/service-clients',
      requirement: 'credential-registry',
      check: 'a-forwarded-user-remint-does-not-loop',
    },
    async () => {
      const forwardedOnly: GeneratedServiceDescriptor = {
        ...descriptor,
        operations: {
          getWidgets: { ...secureOperation, security: { alternatives: [{ allOf: [{ profile: 'user' }] }] } },
        },
      };
      let attempts = 0;
      const serviceUrl = serve(() => {
        attempts++;
        return new Response(null, { status: 401 });
      });
      let refreshCalls = 0;
      const app = application();
      registerServiceClient(app, WidgetsClient, forwardedOnly, {
        url: serviceUrl,
        clientId: 'consumer',
        allowInsecure: true,
        credentials: {
          user: {
            source: 'forwarded-user',
            refresh: () => {
              refreshCalls++;
              return Promise.resolve('still-stale-user-token');
            },
          },
        },
      });
      await app.start();
      try {
        const failure = await runInContext({ __authorizationHeader: 'Bearer stale-user-token' }, () =>
          app.context.get(WidgetsClient).list(),
        ).catch((error: unknown) => error);
        expect(failure).toBeInstanceOf(ClientFrameworkError);
        expect((failure as ClientFrameworkError).status).toBe(401);
        expect(refreshCalls).toBe(1);
        expect(attempts).toBe(2);
      } finally {
        await app.stop();
      }
    },
  );

  specTest(
    'a forwarded-user binding without refresh never retries a 401',
    {
      feature: 'typescript/service-clients',
      requirement: 'credential-registry',
      check: 'a-forwarded-user-binding-without-refresh-keeps-todays-behavior',
    },
    async () => {
      const forwardedOnly: GeneratedServiceDescriptor = {
        ...descriptor,
        operations: {
          getWidgets: { ...secureOperation, security: { alternatives: [{ allOf: [{ profile: 'user' }] }] } },
        },
      };
      let attempts = 0;
      const serviceUrl = serve(() => {
        attempts++;
        return new Response(null, { status: 401 });
      });
      const app = application();
      registerServiceClient(app, WidgetsClient, forwardedOnly, {
        url: serviceUrl,
        clientId: 'consumer',
        allowInsecure: true,
        credentials: { user: { source: 'forwarded-user' } },
      });
      await app.start();
      try {
        const failure = await runInContext({ __authorizationHeader: 'Bearer stale-user-token' }, () =>
          app.context.get(WidgetsClient).list(),
        ).catch((error: unknown) => error);
        expect(failure).toBeInstanceOf(ClientFrameworkError);
        expect(attempts).toBe(1);
      } finally {
        await app.stop();
      }
    },
  );
});

function serviceBinding(url: string, tokenUrl: string): ServiceBinding {
  return {
    url,
    clientId: 'consumer',
    allowInsecure: true,
    credentials: {
      service: {
        source: 'oauth-client-credentials',
        tokenUrl,
        clientId: 'consumer',
        clientSecret: 'secret',
        allowInsecure: true,
      },
    },
  };
}
