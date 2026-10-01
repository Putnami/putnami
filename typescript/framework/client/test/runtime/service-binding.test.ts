import { afterEach, describe, expect, mock, test } from 'bun:test';
import { application, module } from '@putnami/application';
import type { ClientContractDocument, ClientContractOperation } from '@putnami/application';
import { resetConfigLoader, type Registration, runInContext } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { BaseClient } from '../../src/runtime/base-client';
import { CircuitOpenError } from '../../src/runtime/circuit-breaker';
import { CredentialRegistryClosedError } from '../../src/runtime/credential';
import {
  ClientCanceledError,
  ClientCredentialError,
  ClientFrameworkError,
  ClientResponseContractError,
  ClientServiceConfigError,
} from '../../src/runtime/errors';
import {
  type GeneratedServiceDescriptor,
  registerServiceClient,
  type ServiceBinding,
} from '../../src/runtime/service-binding';
import type { StreamObserver } from '../../src/runtime/stream.type';

const secureOperation: ClientContractOperation = {
  stream: 'unary',
  transports: [{ protocol: 'rest-json', path: '/widgets', encoding: 'json' }],
  security: { alternatives: [{ allOf: [{ profile: 'service', scopes: ['write'] }] }] },
  errors: [
    { status: 400, code: 'http.bad_request' },
    { status: 409, code: 'conflict', schema: { type: 'object', properties: { reason: { type: 'string' } } } },
    { status: 500, code: 'http.internal_server' },
  ],
  idempotency: { kind: 'idempotent', keyHeader: 'Idempotency-Key' },
  resilience: {
    timeoutMs: 2000,
    attemptTimeoutMs: 1000,
    retry: { maxAttempts: 2, statuses: [503] },
    circuit: { failureThreshold: 5, resetTimeoutMs: 30_000 },
  },
};

const contract: ClientContractDocument = {
  protocolVersion: 1,
  service: { id: 'widgets', audience: 'api://widgets' },
  credentials: {
    service: { kind: 'service-token', scopes: ['read'] },
    user: { kind: 'forwarded-user-token' },
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

  list(signal?: AbortSignal): Promise<{ ok: boolean }> {
    return this.request('GET', '/widgets', { operationId: 'getWidgets', signal });
  }

  async listAt(endpoint?: string, signal?: AbortSignal): Promise<{ ok: boolean }> {
    if (endpoint !== undefined) return this.forEndpoint(endpoint).list(signal);
    return this.list(signal);
  }

  listWithClientId(value: string): Promise<{ ok: boolean }> {
    return this.request('GET', '/widgets', {
      operationId: 'getWidgets',
      headers: { 'X-Client-Id': value },
    });
  }
}

class ProfiledClient extends BaseClient {
  readonly serviceName = 'widgets';

  first(): Promise<{ ok: boolean }> {
    return this.request('POST', '/first', { operationId: 'first' });
  }

  second(): Promise<{ ok: boolean }> {
    return this.request('GET', '/second', { operationId: 'second' });
  }
}

class GroupedWidgetsClient extends WidgetsClient {}

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

function bind(overrides: Partial<ServiceBinding>): ServiceBinding {
  return { url: 'https://widgets.example.test', ...overrides };
}

/** An unsigned JWT-shaped ID token expiring in one hour, the shape the metadata path accepts. */
function futureIdToken(): string {
  const payload = Buffer.from(JSON.stringify({ exp: Math.floor(Date.now() / 1000) + 3600 })).toString('base64url');
  return `header.${payload}.signature`;
}

/** A fake GCP metadata server that records the audience of every identity request. */
function serveGcpMetadata(audiences: string[]): string {
  return serve((request) => {
    expect(request.headers.get('metadata-flavor')).toBe('Google');
    audiences.push(new URL(request.url).searchParams.get('audience') ?? '');
    return new Response(futureIdToken());
  });
}

/** A contract that declares an audience on the service and on the profile, so a test can prove neither is used. */
const audienceDescriptor: GeneratedServiceDescriptor = {
  ...descriptor,
  contract: {
    ...contract,
    service: { id: 'widgets', audience: 'api://widgets/contract' },
    credentials: { service: { kind: 'service-token', audience: 'api://widgets/profile' } },
  },
};

describe('generated service binding', () => {
  specTest(
    'endpoint fan-out isolates audiences, cache flights and circuits',
    {
      feature: 'typescript/service-clients',
      requirement: 'per-call-endpoint',
      check: 'concurrent-targets-isolate-credentials-circuits-and-cached-responses',
    },
    async () => {
      const acquisitions: string[] = [];
      const calls = [0, 0];
      const urls = [true, false].map((ok, index) =>
        serve((request) => {
          calls[index]++;
          expect(request.headers.get('Authorization')).toBe(`Bearer token-${new URL(request.url).origin}`);
          return Response.json({ ok });
        }),
      );
      let failedCalls = 0;
      const failed = serve(() => {
        failedCalls++;
        return Response.json({ code: 'unavailable' }, { status: 503 });
      });
      const app = application();
      registerServiceClient(
        app,
        WidgetsClient,
        {
          ...descriptor,
          operations: {
            getWidgets: {
              ...secureOperation,
              resilience: {
                ...secureOperation.resilience,
                cache: { freshMs: 60_000 },
                circuit: { failureThreshold: 1, resetTimeoutMs: 60_000 },
              },
            },
          },
        },
        {
          url: urls[0] ?? '',
          clientId: 'consumer',
          credentials: {
            service: {
              source: 'gcp-id-token',
              provider: async (request) => {
                acquisitions.push(request.audience);
                return { value: `token-${request.audience}`, expiresAt: new Date(Date.now() + 3_600_000) };
              },
            },
          },
        },
      );
      await app.start();
      const client = app.context.get(WidgetsClient);
      try {
        const results = await Promise.all(
          Array.from({ length: 40 }, (_, index) => client.listAt(`${urls[index % 2]}/`)),
        );
        for (const [index, result] of results.entries()) expect(result.ok).toBe(index % 2 === 0);
        expect(calls).toEqual([1, 1]);
        expect(acquisitions).toHaveLength(2);
        expect(acquisitions).toEqual(expect.arrayContaining(urls));
        expect(await client.listAt(urls[0]?.replace('http://localhost', 'HTTP://LOCALHOST') ?? '')).toEqual({
          ok: true,
        });
        expect(calls).toEqual([1, 1]);
        expect(acquisitions).toHaveLength(2);
        expect(await client.list()).toEqual({ ok: true });
        await expect(client.listAt(failed)).rejects.toBeInstanceOf(ClientFrameworkError);
        await expect(client.listAt(`${failed}/`)).rejects.toBeInstanceOf(CircuitOpenError);
        expect(failedCalls).toBe(2);
        expect(await client.list()).toEqual({ ok: true });
        expect(client.invalidateResponses('getWidgets?')).toBe(2);
      } finally {
        await app.stop();
      }
      await expect(client.listAt(urls[1])).rejects.toBeInstanceOf(CredentialRegistryClosedError);
    },
  );

  specTest(
    'invalid endpoint overrides fail before credentials or dispatch',
    {
      feature: 'typescript/service-clients',
      requirement: 'per-call-endpoint',
      check: 'an-invalid-override-fails-before-credentials-or-network',
    },
    async () => {
      let acquisitions = 0;
      const app = application();
      registerServiceClient(app, WidgetsClient, descriptor, {
        url: 'https://default.example',
        clientId: 'consumer',
        credentials: {
          service: {
            source: 'gcp-id-token',
            provider: async () => {
              acquisitions++;
              return { value: 'secret', expiresAt: new Date(Date.now() + 3_600_000) };
            },
          },
        },
      });
      await app.start();
      try {
        const client = app.context.get(WidgetsClient);
        for (const endpoint of [
          '',
          '/relative',
          'https://user:secret@target.example',
          'https://target.example?secret=x',
          'https://target.example#fragment',
          'http://target.example',
          'file:///tmp/secret',
        ]) {
          await expect(client.listAt(endpoint)).rejects.toBeInstanceOf(ClientServiceConfigError);
        }
        expect(acquisitions).toBe(0);
        const unbound = new WidgetsClient({ baseUrl: 'https://default.example', transport: 'http' });
        await expect(unbound.listAt('https://target.example')).rejects.toBeInstanceOf(ClientServiceConfigError);
        unbound.dispose();
      } finally {
        await app.stop();
      }
    },
  );

  specTest(
    'endpoint overrides retain explicit audiences and response validation',
    {
      feature: 'typescript/service-clients',
      requirement: 'per-call-endpoint',
      check: 'an-override-preserves-explicit-audiences-and-declared-schemas',
    },
    async () => {
      const audiences: string[] = [];
      const target = serve(() => Response.json({ ok: 'invalid' }));
      class ValidatedClient extends WidgetsClient {
        override list(signal?: AbortSignal): Promise<{ ok: boolean }> {
          return this.request('GET', '/widgets', {
            operationId: 'getWidgets',
            signal,
            successes: [
              {
                status: 200,
                description: '',
                content: [
                  {
                    mediaType: 'application/json',
                    schema: {
                      type: 'object',
                      properties: { ok: { type: 'boolean' } },
                      required: ['ok'],
                    },
                  },
                ],
              },
            ],
          });
        }
      }
      const app = application();
      registerServiceClient(app, ValidatedClient, descriptor, {
        url: 'https://default.example',
        clientId: 'consumer',
        credentials: {
          service: {
            source: 'gcp-id-token',
            audience: 'explicit-audience',
            provider: async (request) => {
              audiences.push(request.audience);
              return { value: 'secret', expiresAt: new Date(Date.now() + 3_600_000) };
            },
          },
        },
      });
      await app.start();
      try {
        await expect(app.context.get(ValidatedClient).listAt(target)).rejects.toBeInstanceOf(
          ClientResponseContractError,
        );
        expect(audiences).toEqual(['explicit-audience']);
      } finally {
        await app.stop();
      }
    },
  );

  test('canceling one endpoint does not cancel its siblings', async () => {
    const fast = serve(() => Response.json({ ok: true }));
    let arrived!: () => void;
    const arrival = new Promise<void>((resolve) => {
      arrived = resolve;
    });
    const slow = serve(
      (request) =>
        new Promise<Response>((resolve) => {
          arrived();
          request.signal.addEventListener('abort', () => resolve(new Response(null, { status: 499 })), { once: true });
        }),
    );
    const app = application();
    registerServiceClient(app, WidgetsClient, descriptor, {
      url: fast,
      clientId: 'consumer',
      credentials: {
        service: {
          source: 'gcp-id-token',
          provider: async () => ({ value: 'token', expiresAt: new Date(Date.now() + 3_600_000) }),
        },
      },
    });
    await app.start();
    try {
      const client = app.context.get(WidgetsClient);
      const controller = new AbortController();
      const pending = client.listAt(slow, controller.signal).catch((error: unknown) => error);
      await arrival;
      expect(await client.listAt(fast)).toEqual({ ok: true });
      controller.abort();
      expect(await pending).toBeInstanceOf(ClientCanceledError);
      expect(await client.list()).toEqual({ ok: true });
    } finally {
      await app.stop();
    }
  });
  test.each([
    'https://user:secret@widgets.example.test',
    'https://widgets.example.test?token=secret',
    'https://widgets.example.test#fragment',
  ])('rejects authority-confusing service URL %s', (url) => {
    const app = application();
    expect(() => registerServiceClient(app, WidgetsClient, descriptor, bind({ url }))).toThrow(
      ClientServiceConfigError,
    );
  });

  test('accepts numeric loopback authorities and rejects DNS names with a 127 prefix', () => {
    expect(() =>
      registerServiceClient(application(), WidgetsClient, descriptor, bind({ url: 'http://127.attacker.example' })),
    ).toThrow(ClientServiceConfigError);
    expect(() =>
      registerServiceClient(application(), WidgetsClient, descriptor, {
        url: 'http://[::1]:8080',
        clientId: 'consumer',
      }),
    ).not.toThrow();
  });

  test('applies the same loopback rule to credential endpoints', async () => {
    const app = application();
    registerServiceClient(app, WidgetsClient, descriptor, {
      url: 'https://widgets.example.test',
      clientId: 'consumer',
      credentials: {
        service: {
          source: 'oauth-client-credentials',
          tokenUrl: 'http://127.attacker.example/token',
          clientSecret: 'secret',
        },
      },
    });
    await app.start();
    try {
      await expect(app.context.get(WidgetsClient).list()).rejects.toBeInstanceOf(ClientCredentialError);
    } finally {
      await app.stop();
    }
  });

  test('registers a singleton in application DI and acquires one token for concurrent calls', async () => {
    let tokenRequests = 0;
    const tokenUrl = serve(async (request) => {
      tokenRequests++;
      expect(request.headers.get('authorization')).toBe(`Basic ${btoa('consumer:client-secret')}`);
      const form = new URLSearchParams(await request.text());
      expect(form.get('audience')).toBe('api://widgets');
      expect(form.get('scope')).toBe('read write');
      return Response.json({ access_token: 'service-token', expires_in: 3600 });
    });
    const serviceUrl = serve((request) => {
      expect(request.headers.get('authorization')).toBe('Bearer service-token');
      expect(request.headers.get('x-client-id')).toBe('consumer');
      return Response.json({ ok: true });
    });

    const app = application();
    registerServiceClient(app, WidgetsClient, descriptor, {
      url: serviceUrl,
      clientId: 'consumer',
      credentials: {
        service: {
          source: 'oauth-client-credentials',
          tokenUrl,
          clientId: 'consumer',
          clientSecret: 'client-secret',
          allowInsecure: true,
        },
      },
      allowInsecure: true,
    });
    await app.start();
    try {
      const client = app.context.get(WidgetsClient);
      expect(app.context.get(WidgetsClient)).toBe(client);
      expect(await Promise.all([client.list(), client.list(), client.list()])).toEqual([
        { ok: true },
        { ok: true },
        { ok: true },
      ]);
      expect(tokenRequests).toBe(1);
    } finally {
      await app.stop();
    }
  });

  test('shares one credential cache and singleflight across clients registered in sibling modules', async () => {
    let tokenRequests = 0;
    const tokenUrl = serve(async () => {
      tokenRequests++;
      await new Promise((resolve) => setTimeout(resolve, 20));
      return Response.json({ access_token: 'module-shared-token', expires_in: 3600 });
    });
    const serviceUrl = serve((request) => {
      expect(request.headers.get('authorization')).toBe('Bearer module-shared-token');
      return Response.json({ ok: true });
    });
    const binding: ServiceBinding = {
      url: serviceUrl,
      clientId: 'consumer',
      credentials: {
        service: {
          source: 'oauth-client-credentials',
          tokenUrl,
          clientId: 'consumer',
          clientSecret: 'secret',
          allowInsecure: true,
        },
      },
      allowInsecure: true,
    };
    const firstModule = module('first-client');
    const secondModule = module('second-client');
    let firstRegistration: Registration<WidgetsClient> | undefined;
    const firstTarget = {
      register<T>(registration: Registration<T>) {
        firstRegistration = registration as unknown as Registration<WidgetsClient>;
        return firstModule.register(registration);
      },
      getRoot: () => firstModule.getRoot(),
    };
    const mutableDescriptor = structuredClone(descriptor);
    registerServiceClient(firstTarget, WidgetsClient, mutableDescriptor, binding);
    (mutableDescriptor.contract.service as { id: string }).id = 'mutated.after.registration';
    registerServiceClient(secondModule, GroupedWidgetsClient, descriptor, binding);
    const app = application().use(firstModule).use(secondModule);

    await app.start();
    try {
      expect(
        await Promise.all([app.context.get(WidgetsClient).list(), app.context.get(GroupedWidgetsClient).list()]),
      ).toEqual([{ ok: true }, { ok: true }]);
      expect(tokenRequests).toBe(1);
      await firstRegistration?.provider.onClose?.(app.context.get(WidgetsClient));
      expect(await app.context.get(GroupedWidgetsClient).list()).toEqual({ ok: true });
      expect(tokenRequests).toBe(1);
    } finally {
      await app.stop();
    }
  });

  test('isolates credential registries between applications', async () => {
    let tokenRequests = 0;
    const tokenUrl = serve(() => {
      tokenRequests++;
      return Response.json({ access_token: `app-token-${tokenRequests}`, expires_in: 3600 });
    });
    const serviceUrl = serve((request) => {
      expect(request.headers.get('authorization')).toMatch(/^Bearer app-token-[12]$/);
      return Response.json({ ok: true });
    });
    const binding: ServiceBinding = {
      url: serviceUrl,
      clientId: 'consumer',
      credentials: {
        service: {
          source: 'oauth-client-credentials',
          tokenUrl,
          clientId: 'consumer',
          clientSecret: 'secret',
          allowInsecure: true,
        },
      },
      allowInsecure: true,
    };
    const first = application();
    const second = application();
    registerServiceClient(first, WidgetsClient, descriptor, binding);
    registerServiceClient(second, GroupedWidgetsClient, descriptor, binding);
    await Promise.all([first.start(), second.start()]);
    try {
      expect(
        await Promise.all([first.context.get(WidgetsClient).list(), second.context.get(GroupedWidgetsClient).list()]),
      ).toEqual([{ ok: true }, { ok: true }]);
      expect(tokenRequests).toBe(2);
    } finally {
      await Promise.all([first.stop(), second.stop()]);
    }
  });

  test('aborts an in-flight credential acquisition and rejects retained clients after application shutdown', async () => {
    let markStarted: (() => void) | undefined;
    const started = new Promise<void>((resolve) => {
      markStarted = resolve;
    });
    let acquisitionAborted = false;
    let resolveLateCredential: ((credential: { value: string; expiresAt: Date }) => void) | undefined;
    const serviceCalls = mock(() => Response.json({ ok: true }));
    const app = application();
    registerServiceClient(app, WidgetsClient, descriptor, {
      url: serve(serviceCalls),
      clientId: 'consumer',
      credentials: {
        service: {
          source: 'oauth-client-credentials',
          provider: (request) =>
            new Promise((resolve) => {
              resolveLateCredential = resolve;
              markStarted?.();
              request.signal?.addEventListener(
                'abort',
                () => {
                  acquisitionAborted = true;
                },
                { once: true },
              );
            }),
        },
      },
      allowInsecure: true,
    });
    await app.start();
    const client = app.context.get(WidgetsClient);
    const pending = client.list().catch((error) => error);
    await started;
    await app.stop();

    expect(await pending).toBeInstanceOf(ClientCredentialError);
    expect(acquisitionAborted).toBe(true);
    resolveLateCredential?.({ value: 'arrived-after-shutdown', expiresAt: new Date(Date.now() + 60_000) });
    await Promise.resolve();
    await expect(client.list()).rejects.toBeInstanceOf(ClientCredentialError);
    expect(serviceCalls).not.toHaveBeenCalled();
  });

  test('refuses an anonymous call through a client retained past its application stop', async () => {
    // No credential is ever acquired for an anonymous operation, so the closed
    // credential registry alone would not stop it: the disposed client must.
    const serviceCalls = mock(() => Response.json({ ok: true }));
    const anonymousDescriptor: GeneratedServiceDescriptor = {
      ...descriptor,
      operations: { getWidgets: { ...secureOperation, security: { alternatives: [{ allOf: [] }] } } },
    };
    const app = application();
    registerServiceClient(app, WidgetsClient, anonymousDescriptor, {
      url: serve(serviceCalls),
      clientId: 'consumer',
      credentials: {},
      allowInsecure: true,
    });
    await app.start();
    const client = app.context.get(WidgetsClient);
    expect(await client.list()).toEqual({ ok: true });
    await app.stop();

    await expect(client.list()).rejects.toBeInstanceOf(CredentialRegistryClosedError);
    expect(serviceCalls).toHaveBeenCalledTimes(1);
  });

  test('cancels an open generated SSE stream when its application stops', async () => {
    let markClosed: (() => void) | undefined;
    const closed = new Promise<void>((resolve) => {
      markClosed = resolve;
    });
    let serviceRequests = 0;
    const serviceUrl = serve((request) => {
      serviceRequests++;
      const body = new ReadableStream<Uint8Array>({
        start(controller) {
          request.signal.addEventListener('abort', () => markClosed?.(), { once: true });
          controller.enqueue(new TextEncoder().encode('event: message\ndata: {"ok":true}\n\n'));
        },
        cancel() {
          markClosed?.();
        },
      });
      return new Response(body, { headers: { 'Content-Type': 'text/event-stream' } });
    });
    const streamDescriptor: GeneratedServiceDescriptor = {
      ...descriptor,
      contract: { ...contract, credentials: {} },
      operations: {
        watchWidgets: {
          stream: 'server',
          transports: [{ protocol: 'sse', path: '/widgets/watch', encoding: 'json' }],
          security: { alternatives: [{ allOf: [] }] },
          errors: secureOperation.errors,
          idempotency: { kind: 'safe' },
          messages: { output: { type: 'object', properties: { ok: { type: 'boolean' } }, required: ['ok'] } },
        },
      },
    };
    const app = application();
    registerServiceClient(app, StreamingWidgetsClient, streamDescriptor, {
      url: serviceUrl,
      clientId: 'consumer',
      allowInsecure: true,
    });
    await app.start();
    const client = app.context.get(StreamingWidgetsClient);
    const stream = client.watch();
    const message = new Promise<{ ok: boolean }>((resolve, reject) => {
      stream.onMessage(resolve);
      stream.onError(reject);
    });
    expect(await message).toEqual({ ok: true });

    await app.stop();

    expect(
      await Promise.race([
        closed.then(() => true),
        new Promise<false>((resolve) => setTimeout(() => resolve(false), 1000)),
      ]),
    ).toBe(true);
    const reopened = client.watch();
    const reopenedError = new Promise<Error>((resolve) => reopened.onError(resolve));
    expect(await reopenedError).toBeInstanceOf(ClientCanceledError);
    expect(serviceRequests).toBe(1);
  });

  test('always imposes the framework binding identity over a caller-supplied header', async () => {
    const serviceUrl = serve((request) => {
      expect(request.headers.get('x-client-id')).toBe('consumer');
      return Response.json({ ok: true });
    });
    const anonymousDescriptor: GeneratedServiceDescriptor = {
      ...descriptor,
      contract: { ...contract, credentials: {} },
      operations: {
        getWidgets: { ...secureOperation, security: { alternatives: [{ allOf: [] }] } },
      },
    };
    const app = application();
    registerServiceClient(app, WidgetsClient, anonymousDescriptor, {
      url: serviceUrl,
      clientId: 'consumer',
      allowInsecure: true,
    });
    await app.start();
    try {
      expect(await app.context.get(WidgetsClient).listWithClientId('attacker')).toEqual({ ok: true });
    } finally {
      await app.stop();
    }
  });

  test('resolves the normal binding from typed framework config and DI', async () => {
    const serviceUrl = serve((request) => {
      expect(request.headers.get('x-client-id')).toBe('configured-consumer');
      return Response.json({ ok: true });
    });
    process.env.CONFIG_DATA = JSON.stringify({
      clients: {
        clientId: 'configured-consumer',
        services: { widgets: { url: serviceUrl, allowInsecure: true } },
      },
    });
    resetConfigLoader();
    const anonymousDescriptor: GeneratedServiceDescriptor = {
      ...descriptor,
      contract: { ...contract, credentials: {} },
      operations: {
        getWidgets: { ...secureOperation, security: { alternatives: [{ allOf: [] }] } },
      },
    };
    const app = application();
    registerServiceClient(app, WidgetsClient, anonymousDescriptor);
    await app.start();
    try {
      expect(await app.context.get(WidgetsClient).list()).toEqual({ ok: true });
    } finally {
      await app.stop();
    }
  });

  // The provider's free-text `message` reaches the typed error only for a
  // binding that asked for it, and the flag has to survive the whole path —
  // binding, client config, request, transport decode. The Go half runs the
  // same provider answer through the same two bindings in
  // TestServiceBindingCarriesTheProviderMessageOnlyWhenItOptsIn
  // (go/framework/client/service_operation_test.go).
  for (const scene of [
    { name: 'keeps the synthetic message on a default binding', expected: 'widgets request failed with not_found' },
    {
      name: 'carries the provider message when the binding opts in',
      carryRemoteMessage: true,
      expected: 'widget 4f0c8f4e does not exist',
    },
  ]) {
    test(scene.name, async () => {
      const serviceUrl = serve(() =>
        Response.json(
          { code: 'not_found', error: 'Not Found', message: 'widget 4f0c8f4e does not exist' },
          { status: 404 },
        ),
      );
      const notFoundDescriptor: GeneratedServiceDescriptor = {
        ...descriptor,
        contract: { ...contract, credentials: {} },
        operations: {
          getWidgets: {
            ...secureOperation,
            security: { alternatives: [{ allOf: [] }] },
            errors: [{ status: 404, code: 'not_found' }],
          },
        },
      };
      const app = application();
      registerServiceClient(app, WidgetsClient, notFoundDescriptor, {
        url: serviceUrl,
        clientId: 'consumer',
        allowInsecure: true,
        ...(scene.carryRemoteMessage ? { carryRemoteMessage: true } : {}),
      });
      await app.start();
      try {
        const error = await app.context
          .get(WidgetsClient)
          .list()
          .then(
            () => undefined,
            (thrown: unknown) => thrown,
          );
        expect(error).toBeInstanceOf(ClientFrameworkError);
        expect((error as ClientFrameworkError).code).toBe('not_found');
        expect((error as ClientFrameworkError).message).toBe(scene.expected);
      } finally {
        await app.stop();
      }
    });
  }

  test('acquires and caches a provider OAuth extension grant without application HTTP code', async () => {
    let tokenRequests = 0;
    const tokenUrl = serve(async (request) => {
      tokenRequests++;
      expect(request.headers.get('content-type')).toBe('application/json');
      expect(await request.json()).toEqual({
        api_key: 'cloud-api-key',
        workspace_id: 'workspace-1',
        grant_type: 'urn:putnami:params:oauth:grant-type:api-key',
        client_id: 'review-worker',
        audience: 'api://widgets',
        scope: 'events write',
      });
      return Response.json({ access_token: 'machine-token', expires_in: 300 });
    });
    const serviceUrl = serve((request) => {
      expect(request.headers.get('authorization')).toBe('Bearer machine-token');
      return Response.json({ ok: true });
    });
    const scopedDescriptor: GeneratedServiceDescriptor = {
      ...descriptor,
      contract: {
        ...contract,
        credentials: { service: { kind: 'service-token', scopes: ['events'] } },
      },
    };
    const app = application();
    registerServiceClient(app, WidgetsClient, scopedDescriptor, {
      url: serviceUrl,
      clientId: 'review-worker',
      credentials: {
        service: {
          source: 'oauth-extension-grant',
          tokenUrl,
          grantType: 'urn:putnami:params:oauth:grant-type:api-key',
          tokenRequestFormat: 'json',
          parameters: { api_key: 'cloud-api-key', workspace_id: 'workspace-1' },
          allowInsecure: true,
        },
      },
      allowInsecure: true,
    });
    await app.start();
    try {
      const client = app.context.get(WidgetsClient);
      expect(await Promise.all([client.list(), client.list()])).toEqual([{ ok: true }, { ok: true }]);
      expect(tokenRequests).toBe(1);
    } finally {
      await app.stop();
    }
  });

  test('rejects extension-grant parameters that collide with framework-owned identity fields', async () => {
    let tokenRequests = 0;
    const tokenUrl = serve(() => {
      tokenRequests++;
      return Response.json({ access_token: 'must-not-be-used', expires_in: 300 });
    });
    const app = application();
    registerServiceClient(app, WidgetsClient, descriptor, {
      url: 'https://widgets.example.test',
      clientId: 'consumer',
      credentials: {
        service: {
          source: 'oauth-extension-grant',
          tokenUrl,
          grantType: 'urn:example:grant',
          parameters: { client_id: 'attacker-controlled' },
          allowInsecure: true,
        },
      },
    });
    await app.start();
    try {
      await expect(app.context.get(WidgetsClient).list()).rejects.toBeInstanceOf(ClientCredentialError);
      expect(tokenRequests).toBe(0);
    } finally {
      await app.stop();
    }
  });

  test('form-encodes OAuth client credentials before constructing HTTP Basic auth', async () => {
    const clientId = 'oauth:id +%é';
    const clientSecret = 'a:b +%é';
    const tokenUrl = serve((request) => {
      const authorization = request.headers.get('authorization');
      expect(authorization?.startsWith('Basic ')).toBe(true);
      const encodedPair = atob(authorization?.slice('Basic '.length) ?? '');
      const separator = encodedPair.indexOf(':');
      expect(separator).toBeGreaterThan(0);
      const decodeComponent = (value: string) => new URLSearchParams(`value=${value}`).get('value');
      expect(decodeComponent(encodedPair.slice(0, separator))).toBe(clientId);
      expect(decodeComponent(encodedPair.slice(separator + 1))).toBe(clientSecret);
      return Response.json({ access_token: 'encoded-basic-token', expires_in: 3600 });
    });
    const serviceUrl = serve((request) => {
      expect(request.headers.get('authorization')).toBe('Bearer encoded-basic-token');
      return Response.json({ ok: true });
    });
    const app = application();
    registerServiceClient(app, WidgetsClient, descriptor, {
      url: serviceUrl,
      clientId: 'consumer',
      credentials: {
        service: {
          source: 'oauth-client-credentials',
          tokenUrl,
          clientId,
          clientSecret,
          allowInsecure: true,
        },
      },
      allowInsecure: true,
    });
    await app.start();
    try {
      expect(await app.context.get(WidgetsClient).list()).toEqual({ ok: true });
    } finally {
      await app.stop();
    }
  });

  test('rejects an oversized credential response while it is streamed and never dispatches the service call', async () => {
    const tokenUrl = serve(
      () =>
        new Response(
          new ReadableStream({
            start(controller) {
              controller.enqueue(new Uint8Array(32 * 1024).fill(97));
              controller.enqueue(new Uint8Array(32 * 1024 + 1).fill(98));
              controller.close();
            },
          }),
        ),
    );
    const serviceCalls = mock(() => Response.json({ ok: true }));
    const app = application();
    registerServiceClient(app, WidgetsClient, descriptor, {
      url: serve(serviceCalls),
      clientId: 'consumer',
      credentials: {
        service: {
          source: 'oauth-client-credentials',
          tokenUrl,
          clientId: 'consumer',
          clientSecret: 'secret',
          allowInsecure: true,
        },
      },
      allowInsecure: true,
    });
    await app.start();
    try {
      await expect(app.context.get(WidgetsClient).list()).rejects.toBeInstanceOf(ClientCredentialError);
      expect(serviceCalls).not.toHaveBeenCalled();
    } finally {
      await app.stop();
    }
  });

  test('rejects a credential that expires within one second before dispatch', async () => {
    const serviceCalls = mock(() => Response.json({ ok: true }));
    const app = application();
    registerServiceClient(app, WidgetsClient, descriptor, {
      url: serve(serviceCalls),
      clientId: 'consumer',
      credentials: {
        service: {
          source: 'oauth-client-credentials',
          provider: async () => ({ value: 'nearly-expired', expiresAt: new Date(Date.now() + 500) }),
        },
      },
      allowInsecure: true,
    });
    await app.start();
    try {
      await expect(app.context.get(WidgetsClient).list()).rejects.toBeInstanceOf(ClientCredentialError);
      expect(serviceCalls).not.toHaveBeenCalled();
    } finally {
      await app.stop();
    }
  });

  test('snapshots nested credential sources when the generated binding is registered', async () => {
    const serviceUrl = serve((request) => {
      expect(request.headers.get('x-api-key')).toBe('registered-value');
      return Response.json({ ok: true });
    });
    const apiKeyDescriptor: GeneratedServiceDescriptor = {
      ...descriptor,
      contract: { ...contract, credentials: { service: { kind: 'api-key', header: 'X-Api-Key' } } },
    };
    const credential = { source: 'static' as const, value: 'registered-value' };
    const app = application();
    registerServiceClient(app, WidgetsClient, apiKeyDescriptor, {
      url: serviceUrl,
      clientId: 'consumer',
      credentials: { service: credential },
      allowInsecure: true,
    });
    credential.value = 'mutated-after-registration';
    await app.start();
    try {
      expect(await app.context.get(WidgetsClient).list()).toEqual({ ok: true });
    } finally {
      await app.stop();
    }
  });

  test('selects the first fully available alternative and never downgrades after acquisition fails', async () => {
    const userOperation: ClientContractOperation = {
      ...secureOperation,
      security: {
        alternatives: [{ allOf: [{ profile: 'service' }] }, { allOf: [{ profile: 'user' }] }, { allOf: [] }],
      },
    };
    const failingDescriptor: GeneratedServiceDescriptor = {
      ...descriptor,
      operations: { getWidgets: userOperation },
    };
    const tokenUrl = serve(() => new Response('credential secret must not escape', { status: 401 }));
    const serviceCalls = mock(() => Response.json({ ok: true }));
    const serviceUrl = serve(serviceCalls);
    const app = application();
    registerServiceClient(app, WidgetsClient, failingDescriptor, {
      url: serviceUrl,
      clientId: 'consumer',
      credentials: {
        service: {
          source: 'oauth-client-credentials',
          tokenUrl,
          clientId: 'consumer',
          clientSecret: 'top-secret',
          allowInsecure: true,
        },
      },
      allowInsecure: true,
    });
    await app.start();
    try {
      await runInContext({ __authorizationHeader: 'Bearer user-token' }, async () => {
        const error = await app.context
          .get(WidgetsClient)
          .list()
          .catch((caught) => caught);
        expect(error).toBeInstanceOf(ClientCredentialError);
        expect(String(error)).not.toContain('top-secret');
        expect(String(error)).not.toContain('credential secret');
      });
      expect(serviceCalls).not.toHaveBeenCalled();
    } finally {
      await app.stop();
    }
  });

  test('uses a forwarded user token per call without putting it in the service-token cache', async () => {
    const forwardedDescriptor: GeneratedServiceDescriptor = {
      ...descriptor,
      operations: {
        getWidgets: {
          ...secureOperation,
          security: { alternatives: [{ allOf: [{ profile: 'user' }] }] },
        },
      },
    };
    const authorizations: string[] = [];
    const serviceUrl = serve((request) => {
      authorizations.push(request.headers.get('authorization') ?? '');
      return Response.json({ ok: true });
    });
    const app = application();
    registerServiceClient(app, WidgetsClient, forwardedDescriptor, {
      url: serviceUrl,
      clientId: 'consumer',
      credentials: { user: { source: 'forwarded-user' } },
      allowInsecure: true,
    });
    await app.start();
    try {
      const client = app.context.get(WidgetsClient);
      await runInContext({ __authorizationHeader: 'Bearer first-user' }, () => client.list());
      await runInContext({ __authorizationHeader: 'Bearer second-user' }, () => client.list());
      expect(authorizations).toEqual(['Bearer first-user', 'Bearer second-user']);
    } finally {
      await app.stop();
    }
  });

  test('resolves canonical forwarded-user opt-in from framework config after bootstrap', async () => {
    const forwardedDescriptor: GeneratedServiceDescriptor = {
      ...descriptor,
      operations: {
        getWidgets: {
          ...secureOperation,
          security: { alternatives: [{ allOf: [{ profile: 'user' }] }] },
        },
      },
    };
    const serviceUrl = serve((request) => {
      expect(request.headers.get('authorization')).toBe('Bearer configured-user');
      return Response.json({ ok: true });
    });
    process.env.CONFIG_DATA = JSON.stringify({
      clients: {
        clientId: 'configured-consumer',
        services: {
          widgets: {
            url: serviceUrl,
            allowInsecure: true,
            credentials: { user: { source: 'forwarded-user' } },
          },
        },
      },
    });
    resetConfigLoader();
    const app = application();
    registerServiceClient(app, WidgetsClient, forwardedDescriptor);
    await app.start();
    try {
      await runInContext({ __authorizationHeader: 'Bearer configured-user' }, () =>
        app.context.get(WidgetsClient).list(),
      );
    } finally {
      await app.stop();
    }
  });

  test('cancellation covers token acquisition and prevents the service attempt', async () => {
    const tokenUrl = serve(async () => {
      await new Promise((resolve) => setTimeout(resolve, 250));
      return Response.json({ access_token: 'too-late', expires_in: 3600 });
    });
    const serviceCalls = mock(() => Response.json({ ok: true }));
    const serviceUrl = serve(serviceCalls);
    const app = application();
    registerServiceClient(app, WidgetsClient, descriptor, {
      url: serviceUrl,
      clientId: 'consumer',
      credentials: {
        service: {
          source: 'oauth-client-credentials',
          tokenUrl,
          clientId: 'consumer',
          clientSecret: 'secret',
          allowInsecure: true,
        },
      },
      allowInsecure: true,
    });
    await app.start();
    try {
      const controller = new AbortController();
      const promise = app.context.get(WidgetsClient).list(controller.signal);
      controller.abort(new DOMException('cancelled', 'AbortError'));
      const error = await promise.catch((caught) => caught);
      expect(error).toBeInstanceOf(ClientCanceledError);
      expect(error).toMatchObject({ code: 'client.canceled' });
      expect(error.cause).toBeUndefined();
      expect(serviceCalls).not.toHaveBeenCalled();
    } finally {
      await app.stop();
    }
  });

  test('retries only an idempotent declared operation and decodes a declared error without raw response leakage', async () => {
    let calls = 0;
    const serviceUrl = serve(() => {
      calls++;
      // Both responses are the first-party envelope `{code, error, message,
      // details?}`: the stable code stands outside the declared detail body.
      if (calls === 1)
        return Response.json(
          {
            code: 'http.service_unavailable',
            error: 'Service Unavailable',
            message: 'retry later',
            details: { token: 'remote-secret' },
          },
          { status: 503 },
        );
      return Response.json(
        {
          code: 'conflict',
          error: 'Conflict',
          message: 'remote message with remote-secret',
          details: { reason: 'already exists', token: 'remote-secret' },
        },
        { status: 409 },
      );
    });
    const errorDescriptor: GeneratedServiceDescriptor = {
      ...descriptor,
      contract: { ...contract, credentials: {} },
      operations: {
        getWidgets: {
          ...secureOperation,
          security: { alternatives: [{ allOf: [] }] },
        },
      },
    };
    const app = application();
    registerServiceClient(app, WidgetsClient, errorDescriptor, {
      url: serviceUrl,
      clientId: 'consumer',
      allowInsecure: true,
    });
    await app.start();
    try {
      const error = await app.context
        .get(WidgetsClient)
        .list()
        .catch((caught) => caught);
      expect(calls).toBe(2);
      expect(error).toBeInstanceOf(ClientFrameworkError);
      expect(error).toMatchObject({ status: 409, code: 'conflict', details: { reason: 'already exists' } });
      // This binding did not set `carryRemoteMessage`, so the provider's
      // envelope message stays off the error and the local synthetic line is
      // what the caller sees. The structured `details.token` sibling is
      // redacted by the credential-shaped-name mechanism either way, and
      // "remote-secret" never reaches the error through the message the
      // binding declined.
      expect(error.message).toBe('widgets request failed with conflict');
      expect(JSON.stringify(error)).not.toContain('remote-secret');
      expect('responseBody' in error).toBe(false);
    } finally {
      await app.stop();
    }
  });

  test('evicts a rejected service token and refreshes only on the next unsafe call', async () => {
    let tokenRequests = 0;
    const tokenUrl = serve(() => {
      tokenRequests++;
      return Response.json({ access_token: `token-${tokenRequests}`, expires_in: 3600 });
    });
    let serviceCalls = 0;
    const serviceUrl = serve((request) => {
      serviceCalls++;
      const authorization = request.headers.get('authorization');
      if (authorization === 'Bearer token-1') return Response.json({ code: 'Unauthorized' }, { status: 401 });
      return Response.json({ ok: true });
    });
    const unsafeDescriptor: GeneratedServiceDescriptor = {
      ...descriptor,
      operations: {
        getWidgets: { ...secureOperation, idempotency: { kind: 'non-idempotent' } },
      },
    };
    const app = application();
    registerServiceClient(app, WidgetsClient, unsafeDescriptor, {
      url: serviceUrl,
      clientId: 'consumer',
      credentials: {
        service: {
          source: 'oauth-client-credentials',
          tokenUrl,
          clientId: 'consumer',
          clientSecret: 'secret',
          allowInsecure: true,
        },
      },
      allowInsecure: true,
    });
    await app.start();
    try {
      const client = app.context.get(WidgetsClient);
      await expect(client.list()).rejects.toMatchObject({ status: 401 });
      expect(serviceCalls).toBe(1);
      expect(await client.list()).toEqual({ ok: true });
      expect(serviceCalls).toBe(2);
      expect(tokenRequests).toBe(2);
    } finally {
      await app.stop();
    }
  });

  test('evicts only the exact acquisition key when distinct profiles return the same token value', async () => {
    const acquisitions = { first: 0, second: 0 };
    const sameToken = async (profile: keyof typeof acquisitions) => {
      acquisitions[profile]++;
      return { value: 'shared-token-value', expiresAt: new Date(Date.now() + 60_000) };
    };
    const serviceUrl = serve((request) => {
      expect(request.headers.get('authorization')).toBe('Bearer shared-token-value');
      if (new URL(request.url).pathname === '/first') {
        return Response.json({ code: 'Unauthorized' }, { status: 401 });
      }
      return Response.json({ ok: true });
    });
    const profiledDescriptor: GeneratedServiceDescriptor = {
      contract: {
        ...contract,
        credentials: {
          first: { kind: 'service-token' },
          second: { kind: 'service-token' },
        },
      },
      service: 'ProfiledService',
      operations: {
        first: {
          ...secureOperation,
          transports: [{ protocol: 'rest-json', path: '/first', encoding: 'json' }],
          security: { alternatives: [{ allOf: [{ profile: 'first' }] }] },
          idempotency: { kind: 'non-idempotent' },
        },
        second: {
          ...secureOperation,
          transports: [{ protocol: 'rest-json', path: '/second', encoding: 'json' }],
          security: { alternatives: [{ allOf: [{ profile: 'second' }] }] },
        },
      },
      transport: 'http',
    };
    const app = application();
    registerServiceClient(app, ProfiledClient, profiledDescriptor, {
      url: serviceUrl,
      clientId: 'consumer',
      credentials: {
        first: { source: 'oauth-client-credentials', provider: () => sameToken('first') },
        second: { source: 'oauth-client-credentials', provider: () => sameToken('second') },
      },
      allowInsecure: true,
    });
    await app.start();
    try {
      const client = app.context.get(ProfiledClient);
      expect(await client.second()).toEqual({ ok: true });
      await expect(client.first()).rejects.toMatchObject({ status: 401 });
      expect(await client.second()).toEqual({ ok: true });
      expect(acquisitions).toEqual({ first: 1, second: 1 });
    } finally {
      await app.stop();
    }
  });

  specTest(
    'requests a gcp-id-token for the binding URL whatever the contract says',
    {
      feature: 'typescript/service-clients',
      requirement: 'credential-audience',
      check: 'a-gcp-id-token-is-requested-for-the-binding-url-whatever-the-contract-says',
    },
    async () => {
      const audiences: string[] = [];
      const metadataUrl = serveGcpMetadata(audiences);
      const serviceUrl = serve((request) => {
        expect(request.headers.get('authorization')?.startsWith('Bearer header.')).toBe(true);
        return Response.json({ ok: true });
      });
      const app = application();
      // A trailing slash in the configured URL is canonicalized away: the
      // audience is the base URL the client itself calls, byte for byte.
      registerServiceClient(app, WidgetsClient, audienceDescriptor, {
        url: `${serviceUrl}/`,
        clientId: 'consumer',
        credentials: { service: { source: 'gcp-id-token', metadataUrl } },
        allowInsecure: true,
      });
      await app.start();
      try {
        expect(await app.context.get(WidgetsClient).list()).toEqual({ ok: true });
        expect(audiences).toEqual([serviceUrl]);
      } finally {
        await app.stop();
      }
    },
  );

  specTest(
    'a binding audience wins over the profile and contract audience for every source',
    {
      feature: 'typescript/service-clients',
      requirement: 'credential-audience',
      check: 'a-binding-audience-wins-over-the-profile-and-contract-audience',
    },
    async () => {
      const idTokenAudiences: string[] = [];
      const metadataUrl = serveGcpMetadata(idTokenAudiences);
      const oauthAudiences: string[] = [];
      const tokenUrl = serve(async (request) => {
        oauthAudiences.push(new URLSearchParams(await request.text()).get('audience') ?? '');
        return Response.json({ access_token: 'oauth-token', expires_in: 3600 });
      });
      const serviceUrl = serve(() => Response.json({ ok: true }));
      const bindings = {
        'gcp-id-token': { source: 'gcp-id-token', audience: 'foo', metadataUrl },
        'oauth-client-credentials': {
          source: 'oauth-client-credentials',
          audience: 'foo',
          tokenUrl,
          clientId: 'consumer',
          clientSecret: 'secret',
          allowInsecure: true,
        },
      } as const;
      for (const service of Object.values(bindings)) {
        const app = application();
        registerServiceClient(app, WidgetsClient, audienceDescriptor, {
          url: serviceUrl,
          clientId: 'consumer',
          credentials: { service },
          allowInsecure: true,
        });
        await app.start();
        try {
          expect(await app.context.get(WidgetsClient).list()).toEqual({ ok: true });
        } finally {
          await app.stop();
        }
      }
      expect(idTokenAudiences).toEqual(['foo']);
      expect(oauthAudiences).toEqual(['foo']);
    },
  );

  test('an OAuth audience still falls back to the profile, then the contract', async () => {
    const audiences: string[] = [];
    const tokenUrl = serve(async (request) => {
      audiences.push(new URLSearchParams(await request.text()).get('audience') ?? '');
      return Response.json({ access_token: 'oauth-token', expires_in: 3600 });
    });
    const serviceUrl = serve(() => Response.json({ ok: true }));
    const withoutProfileAudience: GeneratedServiceDescriptor = {
      ...audienceDescriptor,
      contract: { ...audienceDescriptor.contract, credentials: { service: { kind: 'service-token' } } },
    };
    for (const contractDescriptor of [audienceDescriptor, withoutProfileAudience]) {
      const app = application();
      registerServiceClient(app, WidgetsClient, contractDescriptor, {
        url: serviceUrl,
        clientId: 'consumer',
        credentials: {
          service: {
            source: 'oauth-client-credentials',
            tokenUrl,
            clientId: 'consumer',
            clientSecret: 'secret',
            allowInsecure: true,
          },
        },
        allowInsecure: true,
      });
      await app.start();
      try {
        expect(await app.context.get(WidgetsClient).list()).toEqual({ ok: true });
      } finally {
        await app.stop();
      }
    }
    expect(audiences).toEqual(['api://widgets/profile', 'api://widgets/contract']);
  });

  test('loads the credential audience from typed framework config', async () => {
    const audiences: string[] = [];
    const metadataUrl = serveGcpMetadata(audiences);
    const serviceUrl = serve(() => Response.json({ ok: true }));
    process.env.CONFIG_DATA = JSON.stringify({
      clients: {
        clientId: 'configured-consumer',
        services: {
          widgets: {
            url: serviceUrl,
            allowInsecure: true,
            credentials: {
              service: { source: 'gcp-id-token', audience: 'https://widgets-abc.a.run.app', metadataUrl },
            },
          },
        },
      },
    });
    resetConfigLoader();
    const app = application();
    registerServiceClient(app, WidgetsClient, audienceDescriptor);
    await app.start();
    try {
      expect(await app.context.get(WidgetsClient).list()).toEqual({ ok: true });
      expect(audiences).toEqual(['https://widgets-abc.a.run.app']);
    } finally {
      await app.stop();
    }
  });
});
