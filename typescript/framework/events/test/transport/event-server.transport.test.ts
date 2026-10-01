import { describe, expect, it } from 'bun:test';
import { runInContext } from '@putnami/runtime';
import { PUTNAMI_EVENTS_PROTOCOL } from '../../src/protocol';
import {
  EventServerAmbiguousPublishError,
  EventServerPermanentPublishError,
  EventServerRetryablePublishError,
  eventServerTransport,
  type EventServerTransportConfig,
} from '../../src/transport/event-server.transport';
import type { Envelope } from '../../src/transport/transport';

const TOKEN = 'header.payload.signature';

function envelope(overrides: Partial<Envelope> = {}): Envelope {
  return {
    protocol: PUTNAMI_EVENTS_PROTOCOL,
    id: 'evt-123',
    topic: 'orders.created',
    topicVersion: '1',
    payload: { orderId: 'order-1' },
    timestamp: '2026-07-22T00:00:00.000Z',
    attributes: { region: 'eu' },
    attempt: 1,
    traceId: 'trace-123',
    ...overrides,
  };
}

function accepted(id = 'evt-123', topic = 'orders.created', status = 200): Response {
  return Response.json(
    {
      protocol: PUTNAMI_EVENTS_PROTOCOL,
      id,
      topic,
      timestamp: '2026-07-22T00:00:01.000Z',
    },
    { status },
  );
}

function rejected(status: number, retryable?: boolean, code = 'upstream_unavailable', token = TOKEN): Response {
  return Response.json(
    {
      protocol: PUTNAMI_EVENTS_PROTOCOL,
      code,
      message: `server text must not leak ${token}`,
      retryable,
    },
    { status },
  );
}

function config(overrides: Partial<EventServerTransportConfig> = {}): EventServerTransportConfig {
  return {
    contractVersion: 1,
    endpoint: 'https://events.example',
    audience: 'https://events.example',
    protocol: PUTNAMI_EVENTS_PROTOCOL,
    tokenSource: async () => TOKEN,
    retry: { maxAttempts: 1, baseDelayMs: 0, maxDelayMs: 0 },
    ...overrides,
  };
}

describe('EventServerPublisherTransport request contract', () => {
  it('posts the canonical managed frame with credentials and W3C trace headers', async () => {
    const calls: Array<{ url: string; init: RequestInit }> = [];
    const audiences: string[] = [];
    const transport = eventServerTransport(
      config({
        workspaceId: 'caller-workspace-hint',
        environment: 'production',
        workload: 'orders-api',
        topologyGenerationId: 'generation-1',
        tokenSource: async ({ audience }) => {
          audiences.push(audience);
          return TOKEN;
        },
        fetch: async (input, init) => {
          calls.push({ url: String(input), init: init ?? {} });
          return accepted();
        },
      }),
    );

    const headers = new Headers({
      traceparent: '00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01',
      tracestate: 'vendor=value',
    });
    await runInContext({ headers }, () =>
      transport.publish(
        'orders.created',
        envelope({
          key: 'order-1',
          attributes: { region: 'eu' },
        }),
      ),
    );

    expect(audiences).toEqual(['https://events.example']);
    expect(calls).toHaveLength(1);
    expect(calls[0]?.url).toBe('https://events.example/events/publish');
    expect(calls[0]?.init.method).toBe('POST');
    const requestHeaders = new Headers(calls[0]?.init.headers);
    expect(requestHeaders.get('authorization')).toBe(`Bearer ${TOKEN}`);
    expect(requestHeaders.get('content-type')).toBe('application/json');
    expect(requestHeaders.get('traceparent')).toBe(headers.get('traceparent'));
    expect(requestHeaders.get('tracestate')).toBe(headers.get('tracestate'));

    const body = JSON.parse(String(calls[0]?.init.body)) as Record<string, unknown>;
    expect(body).toEqual({
      protocol: PUTNAMI_EVENTS_PROTOCOL,
      type: 'publish',
      id: 'evt-123',
      dedupeKey: 'evt-123',
      topic: 'orders.created',
      topicVersion: '1',
      payload: { orderId: 'order-1' },
      key: 'order-1',
      attributes: { region: 'eu' },
      traceId: 'trace-123',
    });
    expect(body['channel']).toBeUndefined();
    expect(body['workspaceId']).toBeUndefined();
    expect(body['topologyGenerationId']).toBeUndefined();
  });

  it('preserves exact request bytes and identity across retryable and ambiguous attempts', async () => {
    const bodies: string[] = [];
    let attempt = 0;
    const transport = eventServerTransport(
      config({
        retry: { maxAttempts: 3, baseDelayMs: 1, maxDelayMs: 1 },
        random: () => 0,
        sleep: async () => {},
        fetch: async (_input, init) => {
          bodies.push(String(init?.body));
          attempt += 1;
          if (attempt === 1) return rejected(503, true);
          if (attempt === 2) throw new Error('connection reset');
          return accepted();
        },
      }),
    );

    await transport.publish('orders.created', envelope({ dedupeKey: 'outbox-row-7' }));

    expect(bodies).toHaveLength(3);
    expect(new Set(bodies).size).toBe(1);
    const body = JSON.parse(bodies[0] ?? '{}') as Record<string, unknown>;
    expect(body['id']).toBe('evt-123');
    expect(body['dedupeKey']).toBe('outbox-row-7');
  });

  it('rejects oversized frames before acquiring credentials or sending', async () => {
    let tokenCalls = 0;
    let fetchCalls = 0;
    const transport = eventServerTransport(
      config({
        maxRequestBytes: 128,
        tokenSource: async () => {
          tokenCalls += 1;
          return TOKEN;
        },
        fetch: async () => {
          fetchCalls += 1;
          return accepted();
        },
      }),
    );

    const publish = transport.publish('orders.created', envelope({ payload: { value: 'x'.repeat(512) } }));
    await expect(publish).rejects.toBeInstanceOf(EventServerPermanentPublishError);
    expect(tokenCalls).toBe(0);
    expect(fetchCalls).toBe(0);
  });

  it('rejects missing topic versions and reserved authority attributes before sending', async () => {
    let fetchCalls = 0;
    const transport = eventServerTransport(
      config({
        fetch: async () => {
          fetchCalls += 1;
          return accepted();
        },
      }),
    );

    await expect(transport.publish('orders.created', envelope({ topicVersion: undefined }))).rejects.toBeInstanceOf(
      EventServerPermanentPublishError,
    );
    await expect(
      transport.publish('orders.created', envelope({ attributes: { workspace_id: 'caller-owned' } })),
    ).rejects.toBeInstanceOf(EventServerPermanentPublishError);
    await expect(
      transport.publish('orders.created', envelope({ attributes: { 'putnami.route': 'fallback' } })),
    ).rejects.toBeInstanceOf(EventServerPermanentPublishError);
    await expect(
      transport.publish('orders.created', envelope({ attributes: { 'auth.subject': 'caller-owned' } })),
    ).rejects.toBeInstanceOf(EventServerPermanentPublishError);
    await expect(transport.publish('orders.created', envelope({ channel: 'caller-channel' }))).rejects.toBeInstanceOf(
      EventServerPermanentPublishError,
    );
    expect(fetchCalls).toBe(0);
  });
});

describe('EventServerPublisherTransport outcomes', () => {
  it('accepts a matching structured 200 response', async () => {
    let calls = 0;
    const transport = eventServerTransport(
      config({
        retry: { maxAttempts: 3, baseDelayMs: 0, maxDelayMs: 0 },
        fetch: async () => {
          calls += 1;
          return accepted('evt-123', 'orders.created', 200);
        },
      }),
    );

    await transport.publish('orders.created', envelope());

    expect(calls).toBe(1);
  });

  for (const status of [201, 202]) {
    it(`classifies a matching structured ${status} response as ambiguous`, async () => {
      let calls = 0;
      const transport = eventServerTransport(
        config({
          retry: { maxAttempts: 1, baseDelayMs: 0, maxDelayMs: 0 },
          fetch: async () => {
            calls += 1;
            return accepted('evt-123', 'orders.created', status);
          },
        }),
      );

      await expect(transport.publish('orders.created', envelope())).rejects.toBeInstanceOf(
        EventServerAmbiguousPublishError,
      );
      expect(calls).toBe(1);
    });
  }

  for (const status of [400, 401, 403]) {
    it(`classifies structured ${status} as permanent without leaking tokens`, async () => {
      let calls = 0;
      const transport = eventServerTransport(
        config({
          retry: { maxAttempts: 3, baseDelayMs: 0, maxDelayMs: 0 },
          fetch: async () => {
            calls += 1;
            return rejected(
              status,
              false,
              status === 400 ? 'invalid_protocol' : status === 401 ? 'unauthorized' : 'forbidden',
            );
          },
        }),
      );

      try {
        await transport.publish('orders.created', envelope());
        throw new Error('expected publish to fail');
      } catch (error) {
        expect(error).toBeInstanceOf(EventServerPermanentPublishError);
        expect((error as Error).message).not.toContain(TOKEN);
        expect((error as EventServerPermanentPublishError).outcome).toBe('permanent');
        expect((error as EventServerPermanentPublishError).status).toBe(status);
      }
      expect(calls).toBe(1);
    });
  }

  for (const status of [400, 401, 403]) {
    it(`classifies structured ${status} without a retryable hint as ambiguous`, async () => {
      let calls = 0;
      const transport = eventServerTransport(
        config({
          retry: { maxAttempts: 1, baseDelayMs: 0, maxDelayMs: 0 },
          fetch: async () => {
            calls += 1;
            return rejected(status, undefined, status === 400 ? 'invalid_frame' : 'unauthorized');
          },
        }),
      );

      await expect(transport.publish('orders.created', envelope())).rejects.toBeInstanceOf(
        EventServerAmbiguousPublishError,
      );
      expect(calls).toBe(1);
    });
  }

  for (const status of [429, 503]) {
    it(`classifies exhausted structured ${status} as retryable`, async () => {
      let calls = 0;
      const transport = eventServerTransport(
        config({
          retry: { maxAttempts: 2, baseDelayMs: 0, maxDelayMs: 0 },
          sleep: async () => {},
          fetch: async () => {
            calls += 1;
            return rejected(status, true, status === 429 ? 'rate_limited' : 'upstream_unavailable');
          },
        }),
      );

      try {
        await transport.publish('orders.created', envelope());
        throw new Error('expected publish to fail');
      } catch (error) {
        expect(error).toBeInstanceOf(EventServerRetryablePublishError);
        expect((error as EventServerRetryablePublishError).outcome).toBe('retryable');
        expect((error as EventServerRetryablePublishError).attempts).toBe(2);
      }
      expect(calls).toBe(2);
    });
  }

  for (const status of [429, 503]) {
    it(`honors Retry-After for structured retryable ${status}`, async () => {
      const delays: number[] = [];
      let calls = 0;
      const transport = eventServerTransport(
        config({
          retry: { maxAttempts: 2, baseDelayMs: 0, maxDelayMs: 0 },
          sleep: async (delayMs) => {
            delays.push(delayMs);
          },
          fetch: async () => {
            calls += 1;
            if (calls === 2) return accepted();
            const response = rejected(status, true, status === 429 ? 'rate_limited' : 'upstream_unavailable');
            response.headers.set('Retry-After', '1');
            return response;
          },
        }),
      );

      await transport.publish('orders.created', envelope(), { deadline: Date.now() + 2000 });

      expect(calls).toBe(2);
      expect(delays).toEqual([1000]);
    });
  }

  for (const status of [429, 503]) {
    it(`classifies structured ${status} without a retryable hint as ambiguous`, async () => {
      let calls = 0;
      const transport = eventServerTransport(
        config({
          retry: { maxAttempts: 1, baseDelayMs: 0, maxDelayMs: 0 },
          fetch: async () => {
            calls += 1;
            return rejected(status, undefined, status === 429 ? 'rate_limited' : 'upstream_unavailable');
          },
        }),
      );

      await expect(transport.publish('orders.created', envelope())).rejects.toBeInstanceOf(
        EventServerAmbiguousPublishError,
      );
      expect(calls).toBe(1);
    });
  }

  it('classifies a reset, malformed success, and unstructured 5xx as ambiguous', async () => {
    const responses: Array<Response | Error> = [
      new Error('reset'),
      Response.json({ protocol: PUTNAMI_EVENTS_PROTOCOL, id: 'different', topic: 'orders.created' }),
      new Response('gateway failure', { status: 502 }),
    ];

    // biome-ignore lint/performance/noAwaitInLoops: each independent transport must settle before the next case
    for (const result of responses) {
      const transport = eventServerTransport(
        config({
          fetch: async () => {
            if (result instanceof Error) throw result;
            return result;
          },
        }),
      );
      await expect(transport.publish('orders.created', envelope())).rejects.toBeInstanceOf(
        EventServerAmbiguousPublishError,
      );
    }
  });

  it('bounds a stalled request with the publish deadline', async () => {
    const transport = eventServerTransport(
      config({
        requestTimeoutMs: 5,
        fetch: async (_input, init) =>
          new Promise<Response>((_resolve, reject) => {
            init?.signal?.addEventListener('abort', () => reject(new Error('aborted')), { once: true });
          }),
      }),
    );

    await expect(transport.publish('orders.created', envelope())).rejects.toBeInstanceOf(
      EventServerAmbiguousPublishError,
    );
  });

  it('fails credential acquisition before sending and does not expose its error', async () => {
    let fetchCalls = 0;
    const transport = eventServerTransport(
      config({
        tokenSource: async () => {
          throw new Error(`credential failed with ${TOKEN}`);
        },
        fetch: async () => {
          fetchCalls += 1;
          return accepted();
        },
      }),
    );

    try {
      await transport.publish('orders.created', envelope());
      throw new Error('expected publish to fail');
    } catch (error) {
      expect(error).toBeInstanceOf(EventServerRetryablePublishError);
      expect((error as Error).message).not.toContain(TOKEN);
      expect((error as EventServerRetryablePublishError).attempts).toBe(0);
    }
    expect(fetchCalls).toBe(0);
  });
});

describe('EventServerPublisherTransport config', () => {
  it('requires the managed contract and canonical protocol', () => {
    expect(() => eventServerTransport(config({ contractVersion: 2 }))).toThrow('contractVersion must be 1');
    expect(() => eventServerTransport(config({ protocol: 'putnami.events.v2' }))).toThrow(
      "protocol must be 'putnami.events.v1'",
    );
  });

  it('requires an HTTP(S) origin and owns the canonical path', () => {
    expect(() => eventServerTransport(config({ endpoint: 'file:///tmp/events' }))).toThrow('must use http');
    expect(() => eventServerTransport(config({ endpoint: 'https://events.example/custom' }))).toThrow(
      'must be an origin',
    );
  });
});
