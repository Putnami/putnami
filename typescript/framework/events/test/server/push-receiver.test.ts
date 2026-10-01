import { describe, expect, it } from 'bun:test';
import type { HttpRequestContext } from '@putnami/application';
import type { HandlerDefinition } from '../../src/handler/handler';
import { resolveHandlerOptions } from '../../src/handler/handler.type';
import { type PushConfig, createPushReceiver, decodePushEnvelope } from '../../src/server/push-receiver';
import type { Envelope } from '../../src/transport/transport';

const ALLOWED = 'pusher@sa.example';

function baseConfig(overrides: Partial<PushConfig> = {}): PushConfig {
  return {
    issuer: 'https://accounts.google.com',
    audience: 'https://svc.example/_putnami/events/orders',
    allowedServiceAccounts: [ALLOWED],
    verify: async (token) => (token === 'good' ? { email: ALLOWED, email_verified: true } : undefined),
    ...overrides,
  };
}

function makeHandler(
  topic: string,
  onCall: (msg: unknown) => void | Promise<void>,
  distribution: 'competing' | 'broadcast' = 'competing',
): HandlerDefinition {
  return {
    __handler: 'putnami:event-handler',
    topic: { __topic: 'putnami:topic', name: topic, schema: {} },
    options: resolveHandlerOptions({ distribution }),
    handler: async (msg) => {
      await onCall(msg);
    },
  } as HandlerDefinition;
}

function pushBody(envelope: Partial<Envelope> = {}): unknown {
  const full = {
    topic: 'order.created',
    id: 'evt-1',
    payload: { ok: true },
    timestamp: '2026-05-01T12:00:00.000Z',
    attempt: 1,
    attributes: {},
    ...envelope,
  };
  return { message: { data: Buffer.from(JSON.stringify(full)).toString('base64') }, subscription: 'sub' };
}

function ctx(
  opts: { token?: string; body?: unknown; contentLength?: number; bodyThrows?: boolean } = {},
): HttpRequestContext {
  const headers = new Headers();
  if (opts.token) headers.set('authorization', `Bearer ${opts.token}`);
  if (opts.contentLength !== undefined) headers.set('content-length', String(opts.contentLength));
  return {
    headers,
    body: async () => {
      if (opts.bodyThrows) throw new SyntaxError('Unexpected token in JSON');
      return opts.body;
    },
  } as unknown as HttpRequestContext;
}

describe('push receiver auth', () => {
  it('mounts disabled bootstrap mode fail-closed without invoking verification or handlers', async () => {
    let verified = false;
    let called = false;
    const receiver = createPushReceiver(
      {
        enabled: false,
        verify: async () => {
          verified = true;
          return { email: ALLOWED };
        },
      },
      [makeHandler('order.created', () => (called = true))],
    );

    const res = await receiver(ctx({ token: 'good', body: pushBody() }));

    expect(res.status).toBe(503);
    expect(verified).toBe(false);
    expect(called).toBe(false);
  });

  it('rejects a request with no bearer token (401)', async () => {
    const receiver = createPushReceiver(baseConfig(), []);
    const res = await receiver(ctx({ body: pushBody() }));
    expect(res.status).toBe(401);
  });

  it('rejects an unverifiable token (401)', async () => {
    const receiver = createPushReceiver(baseConfig(), []);
    const res = await receiver(ctx({ token: 'bad', body: pushBody() }));
    expect(res.status).toBe(401);
  });

  it('rejects a verified but non-allowlisted pusher (403)', async () => {
    const config = baseConfig({
      verify: async () => ({ email: 'intruder@evil.example', email_verified: true }),
    });
    let called = false;
    const receiver = createPushReceiver(config, [makeHandler('order.created', () => (called = true))]);
    const res = await receiver(ctx({ token: 'good', body: pushBody() }));
    expect(res.status).toBe(403);
    expect(called).toBe(false);
  });

  it('rejects an unverified email (403)', async () => {
    const config = baseConfig({ verify: async () => ({ email: ALLOWED, email_verified: false }) });
    const receiver = createPushReceiver(config, []);
    const res = await receiver(ctx({ token: 'good', body: pushBody() }));
    expect(res.status).toBe(403);
  });
});

describe('push receiver dispatch + status mapping', () => {
  it('acknowledges a valid push (204) and runs the matching handler', async () => {
    let receivedId: string | undefined;
    const receiver = createPushReceiver(baseConfig(), [
      makeHandler('order.created', (msg) => {
        receivedId = (msg as { id: string }).id;
      }),
    ]);
    const res = await receiver(ctx({ token: 'good', body: pushBody({ id: 'evt-9' }) }));
    expect(res.status).toBe(204);
    expect(receivedId).toBe('evt-9');
  });

  it('returns 500 (retry) when a handler throws', async () => {
    const receiver = createPushReceiver(baseConfig(), [
      makeHandler('order.created', () => {
        throw new Error('boom');
      }),
    ]);
    const res = await receiver(ctx({ token: 'good', body: pushBody() }));
    expect(res.status).toBe(500);
  });

  it('acknowledges (204) a push with no matching handler', async () => {
    let called = false;
    const receiver = createPushReceiver(baseConfig(), [makeHandler('billing.charged', () => (called = true))]);
    const res = await receiver(ctx({ token: 'good', body: pushBody({ topic: 'unhandled.topic' }) }));
    expect(res.status).toBe(204);
    expect(called).toBe(false);
  });

  it('dead-letters a malformed push body (400)', async () => {
    const receiver = createPushReceiver(baseConfig(), []);
    const res = await receiver(ctx({ token: 'good', body: { message: { data: 'not-valid-json' } } }));
    expect(res.status).toBe(400);
  });

  it('dead-letters an oversized push (413) before reading the body', async () => {
    const receiver = createPushReceiver(baseConfig(), []);
    const res = await receiver(ctx({ token: 'good', body: pushBody(), contentLength: 2_000_000 }));
    expect(res.status).toBe(413);
  });

  it('dead-letters a body that fails to parse (400, not 5xx/retry)', async () => {
    const receiver = createPushReceiver(baseConfig(), []);
    const res = await receiver(ctx({ token: 'good', bodyThrows: true }));
    expect(res.status).toBe(400);
  });

  it('dead-letters a wrapper missing the subscription field (400)', async () => {
    const receiver = createPushReceiver(baseConfig(), []);
    const wrapper = pushBody() as { message: { data: string }; subscription?: string };
    wrapper.subscription = undefined;
    const res = await receiver(ctx({ token: 'good', body: wrapper }));
    expect(res.status).toBe(400);
  });
});

describe('push receiver distribution semantics', () => {
  it('round-robins competing handlers (one per push, not all)', async () => {
    let a = 0;
    let b = 0;
    const receiver = createPushReceiver(baseConfig(), [
      makeHandler('order.created', () => {
        a += 1;
      }),
      makeHandler('order.created', () => {
        b += 1;
      }),
    ]);
    await receiver(ctx({ token: 'good', body: pushBody() }));
    await receiver(ctx({ token: 'good', body: pushBody() }));
    expect(a).toBe(1);
    expect(b).toBe(1);
  });

  it('fans out to all broadcast handlers', async () => {
    let a = 0;
    let b = 0;
    const receiver = createPushReceiver(baseConfig(), [
      makeHandler(
        'order.created',
        () => {
          a += 1;
        },
        'broadcast',
      ),
      makeHandler(
        'order.created',
        () => {
          b += 1;
        },
        'broadcast',
      ),
    ]);
    await receiver(ctx({ token: 'good', body: pushBody() }));
    expect(a).toBe(1);
    expect(b).toBe(1);
  });
});

describe('decodePushEnvelope', () => {
  it('applies defaults and strips auth.* attributes', () => {
    const wrapper = {
      message: {
        data: Buffer.from(
          JSON.stringify({
            topic: 'order.created',
            payload: { ok: true },
            attributes: { region: 'eu', 'auth.sub': 'spoofed' },
          }),
        ).toString('base64'),
        messageId: 'msg-1',
      },
      subscription: 'sub',
    };
    const env = decodePushEnvelope(wrapper);
    expect(env).toBeDefined();
    expect(env?.id).toBe('msg-1');
    expect(env?.attempt).toBe(1);
    expect(env?.timestamp).toBeTruthy();
    expect(env?.attributes['auth.sub']).toBeUndefined();
    expect(env?.attributes['region']).toBe('eu');
  });

  it('returns undefined for a missing subscription, missing data, or malformed data', () => {
    expect(decodePushEnvelope(undefined)).toBeUndefined();
    // valid data but no subscription (wrapper-contract violation)
    expect(decodePushEnvelope({ message: { data: Buffer.from('{"topic":"t"}').toString('base64') } })).toBeUndefined();
    expect(decodePushEnvelope({ subscription: 'sub', message: { data: '' } })).toBeUndefined();
    expect(decodePushEnvelope({ subscription: 'sub', message: { data: 'not-valid-json' } })).toBeUndefined();
  });
});
