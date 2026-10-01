import { describe, expect, test } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { BaseClient } from '../../src/runtime/base-client';
import { circuitBreakerInterceptor } from '../../src/runtime/circuit-breaker';
import {
  ClientCanceledError,
  ClientDeadlineError,
  ClientFrameworkError,
  ClientTimeoutError,
} from '../../src/runtime/errors';
import type { DuplexStream, StreamObserver } from '../../src/runtime/stream.type';
import type { ClientResponse, DisposableInterceptor, Interceptor, Transport } from '../../src/runtime/transport.type';

class StreamTestClient extends BaseClient {
  readonly serviceName = 'stream-test';

  openStream<T>(path: string, body?: unknown): StreamObserver<T> {
    return this.stream<T>(path, { body });
  }

  openDuplex<TIn, TOut>(path: string, body?: unknown): DuplexStream<TIn, TOut> {
    return this.streamDuplex<TIn, TOut>(path, { body });
  }
}

/** Captures the headers argument the BaseClient hands to the WS transport. */
function stubWsTransport(client: BaseClient): {
  streamHeaders: () => Promise<Headers>;
  duplexHeaders: () => Promise<Headers>;
} {
  let streamHeaders: Headers | Promise<Headers> | undefined;
  let duplexHeaders: Headers | Promise<Headers> | undefined;
  (
    client as unknown as {
      wsTransport: {
        stream: (path: string, body?: unknown, headers?: Headers | Promise<Headers>) => unknown;
        streamDuplex: (path: string, body?: unknown, headers?: Headers | Promise<Headers>) => unknown;
      };
    }
  ).wsTransport = {
    stream: (_path, _body, headers) => {
      streamHeaders = headers;
      return { onMessage() {}, onError() {}, onComplete() {}, cancel() {} };
    },
    streamDuplex: (_path, _body, headers) => {
      duplexHeaders = headers;
      return { send() {}, end() {}, onMessage() {}, onError() {}, onComplete() {}, cancel() {} };
    },
  };
  return {
    streamHeaders: async () => (await streamHeaders) ?? new Headers(),
    duplexHeaders: async () => (await duplexHeaders) ?? new Headers(),
  };
}

describe('BaseClient stream wrappers', () => {
  test('delegates stream() to the websocket transport', () => {
    const client = new StreamTestClient({
      baseUrl: 'http://example.com',
      transport: 'http',
    });

    const observer = {
      cancel() {},
      onMessage() {},
      onError() {},
      onComplete() {},
    } as unknown as StreamObserver<{ ok: boolean }>;

    let capturedPath = '';
    let capturedBody: unknown;

    (
      client as unknown as {
        wsTransport: { stream: (path: string, body?: unknown) => StreamObserver<{ ok: boolean }> };
      }
    ).wsTransport = {
      stream: (path, body) => {
        capturedPath = path;
        capturedBody = body;
        return observer;
      },
    };

    const result = client.openStream<{ ok: boolean }>('/events', { topic: 'users' });

    expect(result).toBe(observer);
    expect(capturedPath).toBe('/events');
    expect(capturedBody).toEqual({ topic: 'users' });
  });

  test('delegates streamDuplex() to the websocket transport', () => {
    const client = new StreamTestClient({
      baseUrl: 'http://example.com',
      transport: 'http',
    });

    const duplex = {
      send() {},
      end() {},
      cancel() {},
      onMessage() {},
      onError() {},
      onComplete() {},
    } as unknown as DuplexStream<{ id: string }, { ok: boolean }>;

    let capturedPath = '';
    let capturedBody: unknown;

    (
      client as unknown as {
        wsTransport: {
          streamDuplex: (path: string, body?: unknown) => DuplexStream<{ id: string }, { ok: boolean }>;
        };
      }
    ).wsTransport = {
      streamDuplex: (path, body) => {
        capturedPath = path;
        capturedBody = body;
        return duplex;
      },
    };

    const result = client.openDuplex<{ id: string }, { ok: boolean }>('/chat', { room: 'general' });

    expect(result).toBe(duplex);
    expect(capturedPath).toBe('/chat');
    expect(capturedBody).toEqual({ room: 'general' });
  });
});

describe('BaseClient streaming runs interceptors for headers', () => {
  const taggingInterceptor: Interceptor = async (request, next) => {
    request.headers.set('Authorization', 'Bearer stream-token');
    request.headers.set('X-Client-Id', 'orders-service');
    request.headers.set('X-Trace-Id', 'trace-xyz');
    return next(request);
  };

  test('stream() passes interceptor-populated headers to the WS transport', async () => {
    const client = new StreamTestClient({
      baseUrl: 'http://example.com',
      transport: 'http',
      interceptors: [taggingInterceptor],
    });
    const captured = stubWsTransport(client);

    client.openStream('/events', { topic: 'orders' });

    const headers = await captured.streamHeaders();
    expect(headers.get('Authorization')).toBe('Bearer stream-token');
    expect(headers.get('X-Client-Id')).toBe('orders-service');
    expect(headers.get('X-Trace-Id')).toBe('trace-xyz');
  });

  test('streamDuplex() passes interceptor-populated headers to the WS transport', async () => {
    const client = new StreamTestClient({
      baseUrl: 'http://example.com',
      transport: 'http',
      interceptors: [taggingInterceptor],
    });
    const captured = stubWsTransport(client);

    client.openDuplex('/chat', { room: 'general' });

    const headers = await captured.duplexHeaders();
    expect(headers.get('Authorization')).toBe('Bearer stream-token');
    expect(headers.get('X-Client-Id')).toBe('orders-service');
  });

  test('async interceptor (e.g. token fetch) is awaited before headers are used', async () => {
    const asyncAuth: Interceptor = async (request, next) => {
      await new Promise((r) => setTimeout(r, 5));
      request.headers.set('Authorization', 'Bearer async-token');
      return next(request);
    };
    const client = new StreamTestClient({
      baseUrl: 'http://example.com',
      transport: 'http',
      interceptors: [asyncAuth],
    });
    const captured = stubWsTransport(client);

    client.openStream('/events');

    const headers = await captured.streamHeaders();
    expect(headers.get('Authorization')).toBe('Bearer async-token');
  });

  test('no interceptors yields empty headers (no auth)', async () => {
    const client = new StreamTestClient({ baseUrl: 'http://example.com', transport: 'http' });
    const captured = stubWsTransport(client);

    client.openStream('/events');

    const headers = await captured.streamHeaders();
    expect(headers.get('Authorization')).toBeNull();
  });
});

class RequestTestClient extends BaseClient {
  readonly serviceName = 'request-test';

  callGet<T>(path: string, signal?: AbortSignal): Promise<T> {
    return this.request<T>('GET', path, signal ? { signal } : undefined);
  }
}

class FirstPartyRequestTestClient extends BaseClient {
  readonly serviceName = 'catalog.items';

  constructor() {
    super({
      baseUrl: 'https://service.test',
      transport: 'http',
      operationContracts: {
        getWidget: {
          stream: 'unary',
          transports: [{ protocol: 'rest-json', path: '/widgets', encoding: 'json' }],
          security: { alternatives: [{ allOf: [] }] },
          errors: [],
          idempotency: { kind: 'safe' },
          resilience: { retry: { maxAttempts: 1 } },
        },
      },
    });
  }

  call(signal?: AbortSignal): Promise<unknown> {
    return this.request('GET', '/widgets', { operationId: 'getWidget', signal });
  }
}

describe('BaseClient request cancellation', () => {
  test('forwards the caller signal to the transport', async () => {
    const client = new RequestTestClient({
      baseUrl: 'http://example.com',
      transport: 'http',
      retry: { maxRetries: 0 },
    });

    let seen: AbortSignal | undefined;
    (client as unknown as { transport: Transport }).transport = {
      execute: async (request): Promise<ClientResponse> => {
        seen = request.signal;
        return { data: undefined, status: 200, headers: new Headers() };
      },
    };

    const controller = new AbortController();
    await client.callGet('/widgets', controller.signal);

    // The per-attempt timeout is combined with the caller signal, so the
    // transport sees a derived signal rather than the caller's own object.
    // Aborting the caller must still abort what the transport was handed.
    expect(seen).toBeDefined();
    expect(seen?.aborted).toBe(false);
    controller.abort();
    expect(seen?.aborted).toBe(true);
  });

  test('aborting the caller signal ends the retry sequence instead of retrying', async () => {
    const client = new RequestTestClient({
      baseUrl: 'http://example.com',
      transport: 'http',
      // A retryable failure would normally be retried three times.
      retry: { maxRetries: 3, baseDelayMs: 1, jitter: false },
    });

    const controller = new AbortController();
    let attempts = 0;
    (client as unknown as { transport: Transport }).transport = {
      execute: async (): Promise<ClientResponse> => {
        attempts++;
        controller.abort();
        // A fetch aborted mid-flight rejects with an AbortError.
        throw new DOMException('The operation was aborted', 'AbortError');
      },
    };

    await expect(client.callGet('/widgets', controller.signal)).rejects.toThrow(/aborted/i);
    expect(attempts).toBe(1);
  });

  test('a request with no caller signal still carries the per-attempt timeout', async () => {
    const client = new RequestTestClient({
      baseUrl: 'http://example.com',
      transport: 'http',
      timeoutMs: 50,
      retry: { maxRetries: 0 },
    });

    let seen: AbortSignal | undefined;
    (client as unknown as { transport: Transport }).transport = {
      execute: async (request): Promise<ClientResponse> => {
        seen = request.signal;
        return { data: undefined, status: 200, headers: new Headers() };
      },
    };

    await client.callGet('/widgets');
    expect(seen).toBeDefined();
  });
});

describe('BaseClient request error mapping', () => {
  test('wraps a transport TimeoutError as ClientTimeoutError', async () => {
    const client = new RequestTestClient({
      baseUrl: 'http://example.com',
      transport: 'http',
      timeoutMs: 1234,
      retry: { maxRetries: 0 },
    });

    // Inject a transport that fails like AbortSignal.timeout() would.
    (client as unknown as { transport: Transport }).transport = {
      execute: (): Promise<ClientResponse> => {
        throw new DOMException('Signal timed out', 'TimeoutError');
      },
    };

    const promise = client.callGet('/widgets');
    await expect(promise).rejects.toBeInstanceOf(ClientTimeoutError);

    const error = (await promise.catch((e: unknown) => e)) as ClientTimeoutError;
    expect(error.timeoutMs).toBe(1234);
    expect(error.service).toBe('request-test');
    expect(error.method).toBe('GET /widgets');
  });

  test('normalizes an untrusted network failure without preserving its message or cause', async () => {
    const client = new FirstPartyRequestTestClient();
    (client as unknown as { transport: Transport }).transport = {
      execute: (): Promise<ClientResponse> => {
        throw new TypeError('fetch https://service.test/?token=secret-token failed');
      },
    };

    const error = (await client.call().catch((caught: unknown) => caught)) as ClientFrameworkError;
    expect(error).toBeInstanceOf(ClientFrameworkError);
    expect(error.code).toBe('client.remote');
    expect(error.message).not.toContain('secret-token');
    expect(error.cause).toBeUndefined();
  });

  test('normalizes a caller abort reason without exposing it', async () => {
    const client = new FirstPartyRequestTestClient();
    const controller = new AbortController();
    (client as unknown as { transport: Transport }).transport = {
      execute: async (request): Promise<ClientResponse> => {
        controller.abort('secret abort reason');
        throw request.signal?.reason;
      },
    };

    const error = (await client.call(controller.signal).catch((caught: unknown) => caught)) as ClientCanceledError;
    expect(error).toBeInstanceOf(ClientCanceledError);
    expect(error.code).toBe('client.canceled');
    expect(error.message).not.toContain('secret abort reason');
    expect(error.cause).toBeUndefined();
  });

  test('normalizes a first-party attempt timeout as client.deadline', async () => {
    const client = new FirstPartyRequestTestClient();
    (client as unknown as { transport: Transport }).transport = {
      execute: (): Promise<ClientResponse> => {
        throw new DOMException('fetch secret endpoint timed out', 'TimeoutError');
      },
    };

    const error = (await client.call().catch((caught: unknown) => caught)) as ClientDeadlineError;
    expect(error).toBeInstanceOf(ClientDeadlineError);
    expect(error.code).toBe('client.deadline');
    expect(error.message).not.toContain('secret endpoint');
  });
});

class DisposeTestClient extends BaseClient {
  readonly serviceName = 'dispose-test';
}

/** Minimal disposable interceptor that records dispose() calls. */
function makeDisposableInterceptor(): DisposableInterceptor {
  let disposed = 0;
  const interceptor: Interceptor = async (request, next) => next(request);
  return Object.assign(interceptor, {
    dispose: () => {
      disposed++;
    },
    disposedCount: () => disposed,
  });
}

describe('BaseClient.dispose', () => {
  specTest(
    'dispose() tears down disposable interceptors',
    {
      feature: 'typescript/service-clients',
      requirement: 'resource-release',
      check: 'disposing-the-client-tears-down-disposable-interceptors',
    },
    () => {
      const disposable = makeDisposableInterceptor() as DisposableInterceptor & { disposedCount: () => number };
      const client = new DisposeTestClient({
        baseUrl: 'http://example.com',
        transport: 'http',
        interceptors: [disposable],
      });

      expect(disposable.disposedCount()).toBe(0);
      client.dispose();
      expect(disposable.disposedCount()).toBe(1);
    },
  );

  specTest(
    '[Symbol.dispose] delegates to dispose() (using-statement support)',
    {
      feature: 'typescript/service-clients',
      requirement: 'resource-release',
      check: 'the-using-statement-delegates-to-dispose',
    },
    () => {
      const disposable = makeDisposableInterceptor() as DisposableInterceptor & { disposedCount: () => number };

      {
        using _client = new DisposeTestClient({
          baseUrl: 'http://example.com',
          transport: 'http',
          interceptors: [disposable],
        });
        expect(disposable.disposedCount()).toBe(0);
      }

      // Leaving the block disposes the client via Symbol.dispose.
      expect(disposable.disposedCount()).toBe(1);
    },
  );

  specTest(
    'dispose() is a no-op when there are no disposable interceptors',
    {
      feature: 'typescript/service-clients',
      requirement: 'resource-release',
      check: 'disposing-with-no-disposable-interceptor-is-a-no-op',
    },
    () => {
      const passthrough: Interceptor = async (request, next) => next(request);
      const client = new DisposeTestClient({
        baseUrl: 'http://example.com',
        transport: 'http',
        interceptors: [passthrough],
      });

      expect(() => client.dispose()).not.toThrow();
    },
  );

  specTest(
    'dispose() stops a circuit breaker health probe wired through the client',
    {
      feature: 'typescript/service-clients',
      requirement: 'resource-release',
      check: 'disposing-the-client-stops-a-wired-circuit-health-probe',
    },
    async () => {
      let probeCount = 0;
      const server = Bun.serve({
        port: 0,
        fetch() {
          probeCount++;
          return new Response('unavailable', { status: 503 });
        },
      });

      const breaker = circuitBreakerInterceptor({
        failureThreshold: 1,
        failureStatuses: [503],
        healthCheckUrl: `http://localhost:${server.port}/health`,
        healthCheckIntervalMs: 5,
      });

      const client = new DisposeTestClient({
        baseUrl: 'http://example.com',
        transport: 'http',
        interceptors: [breaker],
        retry: { maxRetries: 0 },
      });

      // Transport that always 503s so the breaker opens and starts probing.
      (client as unknown as { transport: Transport }).transport = {
        execute: async (): Promise<ClientResponse> => ({ data: undefined, status: 503, headers: new Headers() }),
      };

      try {
        // Drive one request through the chain to trip the breaker open.
        await (client as unknown as { request: (m: string, p: string) => Promise<unknown> })
          .request('GET', '/widgets')
          .catch(() => {});

        await waitFor(() => probeCount >= 1);

        // dispose() on the client must reach the breaker and stop the probe.
        client.dispose();
        await wait(20);
        const countAfterDispose = probeCount;

        await wait(40);
        expect(probeCount).toBe(countAfterDispose);
      } finally {
        client.dispose();
        server.stop(true);
      }
    },
  );
});

function wait(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function waitFor(fn: () => boolean, timeoutMs = 500, intervalMs = 5): Promise<void> {
  const start = Date.now();
  while (!fn()) {
    if (Date.now() - start > timeoutMs) throw new Error('waitFor timed out');
    // biome-ignore lint/performance/noAwaitInLoops: bounded test polling is intentionally sequential
    await wait(intervalMs);
  }
}
