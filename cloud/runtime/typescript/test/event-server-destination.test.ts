import { describe, expect, it } from 'bun:test';
import {
  EventServerRetryablePublishError,
  googlePubSubTransport,
  PUTNAMI_EVENTS_PROTOCOL,
  type Envelope,
} from '@putnami/events';
import {
  cachedEventServerTokenSource,
  eventServerDestination,
  googleEventServerTokenSource,
  type ManagedEventServerEventsConfig,
} from '../src/runtime/event-server-destination';

const AUDIENCE = 'https://events.example.test';
const NOW_MS = Date.UTC(2026, 6, 22, 12, 0, 0);

function managedConfig(): ManagedEventServerEventsConfig {
  return {
    transport: 'eventserver',
    eventServer: {
      contractVersion: 1,
      endpoint: AUDIENCE,
      audience: AUDIENCE,
      protocol: PUTNAMI_EVENTS_PROTOCOL,
      workspaceId: 'workspace-1',
      environment: 'production',
      workload: 'orders-api',
      topologyGenerationId: 'generation-42',
    },
  };
}

function envelope(id = 'event-1'): Envelope {
  return {
    protocol: PUTNAMI_EVENTS_PROTOCOL,
    id,
    topic: 'order.created',
    payload: { orderId: 'order-1' },
    dedupeKey: `dedupe-${id}`,
    topicVersion: '1',
    timestamp: new Date(NOW_MS).toISOString(),
    attributes: { source: 'orders' },
    attempt: 1,
    traceId: 'trace-1',
  };
}

function jwt(audience: string, expiresAtMs: number, extra: Record<string, unknown> = {}): string {
  const header = Buffer.from(JSON.stringify({ alg: 'RS256', typ: 'JWT' })).toString('base64url');
  const payload = Buffer.from(
    JSON.stringify({ aud: audience, exp: Math.floor(expiresAtMs / 1000), ...extra }),
  ).toString('base64url');
  return `${header}.${payload}.signature`;
}

function publishSuccess(id: string, topic = 'order.created'): Response {
  return Response.json({
    protocol: PUTNAMI_EVENTS_PROTOCOL,
    id,
    topic,
    timestamp: new Date(NOW_MS).toISOString(),
  });
}

function signal(): AbortSignal {
  return new AbortController().signal;
}

describe('eventServerDestination', () => {
  it('sends the canonical request with an exact audience-pinned bearer', async () => {
    const token = jwt(AUDIENCE, NOW_MS + 60 * 60 * 1000);
    let tokenAudience: string | undefined;
    let requestUrl: string | undefined;
    let requestInit: RequestInit | undefined;

    const destination = eventServerDestination(managedConfig(), {
      tokenSource: async (request) => {
        tokenAudience = request.audience;
        return token;
      },
      credentialCache: { now: () => NOW_MS },
      transport: {
        fetch: async (input, init) => {
          requestUrl = input.toString();
          requestInit = init;
          return publishSuccess('event-1');
        },
      },
    });

    await destination.publish('order.created', envelope());

    expect(tokenAudience).toBe(AUDIENCE);
    expect(requestUrl).toBe(`${AUDIENCE}/events/publish`);
    expect(new Headers(requestInit?.headers).get('authorization')).toBe(`Bearer ${token}`);
    expect(new Headers(requestInit?.headers).get('content-type')).toBe('application/json');
    expect(JSON.parse(String(requestInit?.body))).toEqual({
      protocol: PUTNAMI_EVENTS_PROTOCOL,
      type: 'publish',
      id: 'event-1',
      dedupeKey: 'dedupe-event-1',
      topic: 'order.created',
      topicVersion: '1',
      payload: { orderId: 'order-1' },
      attributes: { source: 'orders' },
      traceId: 'trace-1',
    });
  });

  it('validates the complete managed binding before acquiring credentials', () => {
    const cases: [string, (config: ManagedEventServerEventsConfig) => void][] = [
      ['transport', (config) => (config.transport = 'pubsub')],
      ['contract', (config) => (config.eventServer.contractVersion = 2)],
      ['protocol', (config) => (config.eventServer.protocol = 'putnami.events.v2')],
      ['endpoint scheme', (config) => (config.eventServer.endpoint = 'http://events.test')],
      ['endpoint path', (config) => (config.eventServer.endpoint = `${AUDIENCE}/publish`)],
      ['audience', (config) => (config.eventServer.audience = ' audience ')],
      ['workspace', (config) => (config.eventServer.workspaceId = '')],
      ['environment', (config) => (config.eventServer.environment = ' ')],
      ['workload', (config) => (config.eventServer.workload = 'orders\napi')],
      ['generation', (config) => (config.eventServer.topologyGenerationId = '')],
      ['legacy Pub/Sub', (config) => (config.pubsub = { projectId: 'legacy' })],
      ['persistent credential', (config) => Object.assign(config.eventServer, { token: 'must-not-be-persisted' })],
    ];
    let credentialCalls = 0;

    for (const [, mutate] of cases) {
      const config = managedConfig();
      mutate(config);
      expect(() =>
        eventServerDestination(config, {
          tokenSource: async () => {
            credentialCalls += 1;
            return 'not-used';
          },
        }),
      ).toThrow();
    }
    expect(credentialCalls).toBe(0);
  });

  it('captures an immutable transport and topology generation', async () => {
    const config = managedConfig();
    const token = jwt(AUDIENCE, NOW_MS + 60 * 60 * 1000);
    let requestUrl = '';
    const destination = eventServerDestination(config, {
      tokenSource: async () => token,
      credentialCache: { now: () => NOW_MS },
      route: { transport: 'eventserver', topologyGenerationId: 'generation-42' },
      transport: {
        fetch: async (input) => {
          requestUrl = input.toString();
          return publishSuccess('event-1');
        },
      },
    });

    config.transport = 'pubsub';
    config.eventServer.endpoint = 'https://replacement.example.test';
    config.eventServer.topologyGenerationId = 'generation-43';
    await destination.publish('order.created', envelope());

    expect(requestUrl).toBe(`${AUDIENCE}/events/publish`);
  });

  it('refuses a stale persisted generation before token or network access', () => {
    let tokenCalls = 0;
    let networkCalls = 0;

    expect(() =>
      eventServerDestination(managedConfig(), {
        route: { transport: 'eventserver', topologyGenerationId: 'generation-41' },
        tokenSource: async () => {
          tokenCalls += 1;
          return 'not-used';
        },
        transport: {
          fetch: async () => {
            networkCalls += 1;
            return publishSuccess('event-1');
          },
        },
      }),
    ).toThrow(/stale/);

    expect(tokenCalls).toBe(0);
    expect(networkCalls).toBe(0);
  });

  it('keeps retry bytes, identity, and generation pinned without Pub/Sub fallback', async () => {
    const token = jwt(AUDIENCE, NOW_MS + 60 * 60 * 1000);
    const bodies: string[] = [];
    let credentialCalls = 0;
    let requests = 0;
    const destination = eventServerDestination(managedConfig(), {
      route: { transport: 'eventserver', topologyGenerationId: 'generation-42' },
      tokenSource: async () => {
        credentialCalls += 1;
        return token;
      },
      credentialCache: { now: () => NOW_MS },
      transport: {
        fetch: async (_input, init) => {
          requests += 1;
          bodies.push(String(init?.body));
          if (requests === 1) {
            return Response.json(
              {
                protocol: PUTNAMI_EVENTS_PROTOCOL,
                code: 'upstream_unavailable',
                message: 'retry',
                retryable: true,
              },
              { status: 503 },
            );
          }
          return publishSuccess('event-retry');
        },
        sleep: async () => {},
        random: () => 0,
      },
    });

    await destination.publish('order.created', envelope('event-retry'));

    expect(requests).toBe(2);
    expect(credentialCalls).toBe(1);
    expect(bodies[0]).toBe(bodies[1]);
    expect(JSON.parse(bodies[0] as string).id).toBe('event-retry');
  });

  it('fails closed on credential cancellation without sending', async () => {
    const controller = new AbortController();
    let networkCalls = 0;
    const destination = eventServerDestination(managedConfig(), {
      tokenSource: async ({ signal: credentialSignal }) =>
        new Promise<string>((_resolve, reject) => {
          credentialSignal.addEventListener('abort', () => reject(new Error('aborted')), {
            once: true,
          });
        }),
      transport: {
        fetch: async () => {
          networkCalls += 1;
          return publishSuccess('event-1');
        },
      },
    });
    controller.abort();

    await expect(
      destination.publish('order.created', envelope(), { signal: controller.signal }),
    ).rejects.toBeInstanceOf(EventServerRetryablePublishError);
    expect(networkCalls).toBe(0);
  });

  it('redacts credential failures, tokens, and provider response text', async () => {
    const secret = 'secret-token-material';
    const destination = eventServerDestination(managedConfig(), {
      tokenSource: async () => {
        throw new Error(`provider returned ${secret}`);
      },
      transport: {
        fetch: async () => new Response(`Authorization: Bearer ${secret}`, { status: 500 }),
      },
    });

    let caught: unknown;
    try {
      await destination.publish('order.created', envelope());
    } catch (error) {
      caught = error;
    }
    expect(caught).toBeInstanceOf(EventServerRetryablePublishError);
    expect(String(caught)).not.toContain(secret);
    expect(JSON.stringify(caught)).not.toContain(secret);
  });
});

describe('cachedEventServerTokenSource', () => {
  it('caches per exact audience and refreshes inside the expiry safety window', async () => {
    let now = NOW_MS;
    let calls = 0;
    const source = cachedEventServerTokenSource(
      async ({ audience }) => {
        calls += 1;
        return jwt(audience, now + 10 * 60 * 1000, { sequence: calls });
      },
      { now: () => now, refreshSkewMs: 60 * 1000 },
    );

    const first = await source({ audience: AUDIENCE, signal: signal() });
    expect(await source({ audience: AUDIENCE, signal: signal() })).toBe(first);
    expect(calls).toBe(1);

    now += 9 * 60 * 1000 + 1;
    const refreshed = await source({ audience: AUDIENCE, signal: signal() });
    expect(refreshed).not.toBe(first);
    expect(calls).toBe(2);

    await source({ audience: 'https://other.example.test', signal: signal() });
    expect(calls).toBe(3);
  });

  it('coalesces concurrent refresh and lets one waiter cancel safely', async () => {
    let calls = 0;
    let resolveToken: ((token: string) => void) | undefined;
    let sharedSignal: AbortSignal | undefined;
    const source = cachedEventServerTokenSource(
      async ({ signal: credentialSignal }) => {
        calls += 1;
        sharedSignal = credentialSignal;
        return new Promise<string>((resolve) => {
          resolveToken = resolve;
        });
      },
      { now: () => NOW_MS },
    );
    const cancelled = new AbortController();
    const first = source({ audience: AUDIENCE, signal: cancelled.signal });
    const second = source({ audience: AUDIENCE, signal: signal() });
    const rest = Array.from({ length: 8 }, () => source({ audience: AUDIENCE, signal: signal() }));

    cancelled.abort();
    await expect(first).rejects.toThrow(/cancelled/);
    expect(sharedSignal?.aborted).toBe(false);
    resolveToken?.(jwt(AUDIENCE, NOW_MS + 60 * 60 * 1000));

    await expect(second).resolves.toContain('.');
    await Promise.all(rest);
    expect(calls).toBe(1);
  });

  it('times out a stuck provider refresh with a redacted failure', async () => {
    const source = cachedEventServerTokenSource(
      async ({ signal: credentialSignal }) =>
        new Promise<string>((_resolve, reject) => {
          credentialSignal.addEventListener('abort', () => reject(new Error('provider secret response')), {
            once: true,
          });
        }),
      { acquisitionTimeoutMs: 5, now: () => NOW_MS },
    );

    await expect(source({ audience: AUDIENCE, signal: signal() })).rejects.toThrow(
      'Managed Event Server credential acquisition failed',
    );
  });

  it('bounds a non-cooperative source without starting repeated stuck refreshes', async () => {
    let calls = 0;
    let release: (() => void) | undefined;
    const source = cachedEventServerTokenSource(
      async ({ audience }) => {
        calls += 1;
        if (calls === 1) {
          await new Promise<void>((resolve) => {
            release = resolve;
          });
        }
        return jwt(audience, NOW_MS + 60 * 60 * 1000);
      },
      { acquisitionTimeoutMs: 5, now: () => NOW_MS },
    );

    await expect(source({ audience: AUDIENCE, signal: signal() })).rejects.toThrow(
      'Managed Event Server credential acquisition failed',
    );
    await expect(source({ audience: AUDIENCE, signal: signal() })).rejects.toThrow(
      'Managed Event Server credential acquisition failed',
    );
    expect(calls).toBe(1);

    release?.();
    await Bun.sleep(0);
    await expect(source({ audience: AUDIENCE, signal: signal() })).resolves.toContain('.');
    expect(calls).toBe(2);
  });

  it('rejects malformed, expired, near-expiry, and wrong-audience JWTs', async () => {
    const invalidTokens = [
      'not-a-jwt',
      jwt(AUDIENCE, NOW_MS - 1000),
      jwt(AUDIENCE, NOW_MS + 30 * 1000),
      jwt('https://wrong.example.test', NOW_MS + 60 * 60 * 1000),
      jwt(AUDIENCE, NOW_MS + 60 * 60 * 1000, { aud: [AUDIENCE] }),
      jwt(AUDIENCE, NOW_MS + 60 * 60 * 1000, {
        exp: String(Math.floor((NOW_MS + 60 * 60 * 1000) / 1000)),
      }),
      `${Buffer.from('{}').toString('base64url')}.${Buffer.from(JSON.stringify({ aud: AUDIENCE })).toString(
        'base64url',
      )}.signature`,
    ];

    await Promise.all(
      invalidTokens.map(async (token) => {
        const source = cachedEventServerTokenSource(async () => token, { now: () => NOW_MS });
        await expect(source({ audience: AUDIENCE, signal: signal() })).rejects.toThrow(
          'Managed Event Server credential acquisition failed',
        );
      }),
    );
  });
});

describe('Google metadata and direct Pub/Sub compatibility', () => {
  it('requests an exact audience token from the metadata identity endpoint', async () => {
    let metadataUrl = '';
    let metadataHeaders: Headers | undefined;
    const token = jwt(AUDIENCE, NOW_MS + 60 * 60 * 1000);
    const source = googleEventServerTokenSource({
      now: () => NOW_MS,
      fetch: async (input, init) => {
        metadataUrl = input.toString();
        metadataHeaders = new Headers(init?.headers);
        return new Response(token, {
          status: 200,
          headers: { 'Metadata-Flavor': 'Google' },
        });
      },
    });

    expect(await source({ audience: AUDIENCE, signal: signal() })).toBe(token);
    const parsed = new URL(metadataUrl);
    expect(parsed.origin + parsed.pathname).toBe(
      'http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity',
    );
    expect(parsed.searchParams.get('audience')).toBe(AUDIENCE);
    expect(parsed.searchParams.get('format')).toBe('full');
    expect(metadataHeaders?.get('metadata-flavor')).toBe('Google');
  });

  it('rejects an oversized metadata response without buffering the remainder', async () => {
    let pulls = 0;
    const oversized = new ReadableStream<Uint8Array>({
      pull(controller) {
        pulls += 1;
        controller.enqueue(new Uint8Array(32 * 1024));
      },
    });
    const source = googleEventServerTokenSource({
      now: () => NOW_MS,
      fetch: async () =>
        new Response(oversized, {
          status: 200,
          headers: { 'Metadata-Flavor': 'Google' },
        }),
    });

    await expect(source({ audience: AUDIENCE, signal: signal() })).rejects.toThrow(
      'Managed Event Server credential acquisition failed',
    );
    expect(pulls).toBeLessThanOrEqual(4);
  });

  it('uses an explicit credential source off-GCP without contacting metadata', async () => {
    let metadataCalls = 0;
    const token = jwt(AUDIENCE, NOW_MS + 60 * 60 * 1000);
    const destination = eventServerDestination(managedConfig(), {
      tokenSource: async () => token,
      credentialCache: { now: () => NOW_MS },
      metadata: {
        fetch: async () => {
          metadataCalls += 1;
          throw new Error('metadata must not be called');
        },
      },
      transport: { fetch: async () => publishSuccess('event-1') },
    });

    await destination.publish('order.created', envelope());
    expect(metadataCalls).toBe(0);
  });

  it('fails closed off-GCP when ambient metadata identity is unavailable', async () => {
    const secret = 'metadata-provider-secret';
    let eventServerCalls = 0;
    const destination = eventServerDestination(managedConfig(), {
      metadata: {
        fetch: async () => {
          throw new Error(secret);
        },
      },
      transport: {
        fetch: async () => {
          eventServerCalls += 1;
          return publishSuccess('event-1');
        },
      },
    });

    let caught: unknown;
    try {
      await destination.publish('order.created', envelope());
    } catch (error) {
      caught = error;
    }
    expect(caught).toBeInstanceOf(EventServerRetryablePublishError);
    expect(String(caught)).not.toContain(secret);
    expect(eventServerCalls).toBe(0);
  });

  it('keeps the framework direct Pub/Sub transport available for workloads that publish to Pub/Sub directly', async () => {
    let publishedTopic = '';
    let publishedData = '';
    const direct = googlePubSubTransport({
      client: {
        topic(name) {
          publishedTopic = name;
          return {
            publishMessage(message) {
              publishedData = new TextDecoder().decode(message.data);
            },
          };
        },
        subscription() {
          throw new Error('not used by publish compatibility test');
        },
      },
    });

    await direct.publish('legacy.topic', { ...envelope('legacy-1'), topic: 'legacy.topic' });
    expect(publishedTopic).toBe('legacy.topic');
    expect(JSON.parse(publishedData).id).toBe('legacy-1');
  });
});
