import { describe, expect, it, beforeEach } from 'bun:test';
import { Uuid, Email, runInContext } from '@putnami/runtime';
import { getPublisher, setTransport, clearTransport } from '../../src/publisher/publisher';
import { topic } from '../../src/topic/topic';
import type { Transport } from '../../src/transport/transport';

const UserCreated = topic('user.created', { id: Uuid, email: Email, name: String });

function createMockTransport(): Transport & { published: unknown[] } {
  const published: unknown[] = [];
  return {
    published,
    async publish(_topic: string, envelope: unknown) {
      published.push(envelope);
    },
    async subscribe() {},
    async start() {},
    async stop() {},
  };
}

describe('getPublisher()', () => {
  beforeEach(() => {
    clearTransport();
  });

  it('should throw if no transport is configured', async () => {
    const publish = getPublisher(UserCreated);

    expect(publish({ id: crypto.randomUUID(), email: 'test@example.com', name: 'Test' })).rejects.toThrow(
      'No transport configured',
    );
  });

  it('should publish a valid payload', async () => {
    const transport = createMockTransport();
    setTransport(transport);

    const publish = getPublisher(UserCreated);
    await publish({ id: crypto.randomUUID(), email: 'test@example.com', name: 'Test' });

    expect(transport.published.length).toBe(1);
  });

  it('should reject invalid payloads', async () => {
    const transport = createMockTransport();
    setTransport(transport);

    const publish = getPublisher(UserCreated);

    // Missing required field
    // @ts-expect-error Testing invalid payload
    expect(publish({ id: crypto.randomUUID(), email: 'test@example.com' })).rejects.toThrow(
      "Invalid payload for topic 'user.created'",
    );
  });

  it('should reject payloads failing constraints', async () => {
    const transport = createMockTransport();
    setTransport(transport);

    const publish = getPublisher(UserCreated);

    expect(publish({ id: 'not-a-uuid', email: 'test@example.com', name: 'Test' })).rejects.toThrow(
      'must be a valid UUID',
    );
  });

  it('should pass publish options through to the envelope', async () => {
    const transport = createMockTransport();
    setTransport(transport);

    const publish = getPublisher(UserCreated);
    await publish(
      { id: crypto.randomUUID(), email: 'test@example.com', name: 'Test' },
      {
        attributes: { region: 'eu' },
        traceId: 'trace-123',
        messageId: 'message-123',
        key: 'user-1',
        dedupeKey: 'dedupe-1',
      },
    );

    expect(transport.published.length).toBe(1);
    const envelope = transport.published[0] as {
      id: string;
      attributes: Record<string, string>;
      traceId: string;
      key: string;
      dedupeKey: string;
    };
    expect(envelope.id).toBe('message-123');
    expect(envelope.attributes).toEqual({ region: 'eu' });
    expect(envelope.traceId).toBe('trace-123');
    expect(envelope.key).toBe('user-1');
    expect(envelope.dedupeKey).toBe('dedupe-1');
  });

  it('should attach topic version to the envelope', async () => {
    const transport = createMockTransport();
    setTransport(transport);

    const VersionedTopic = topic('user.versioned', { id: Uuid }, { version: 'v2' });
    const publish = getPublisher(VersionedTopic);
    await publish({ id: crypto.randomUUID() });

    const envelope = transport.published[0] as { topicVersion?: string };
    expect(envelope.topicVersion).toBe('v2');
  });

  it('should attach topic channel to the envelope', async () => {
    const transport = createMockTransport();
    setTransport(transport);

    const AnalyticsTopic = topic('analytics.page_view', { id: Uuid }, { channel: 'analytics' });
    const publish = getPublisher(AnalyticsTopic);
    await publish({ id: crypto.randomUUID() });

    const envelope = transport.published[0] as { channel?: string; protocol?: string };
    expect(envelope.protocol).toBe('putnami.events.v1');
    expect(envelope.channel).toBe('analytics');
  });

  it('should auto-capture traceId from async context when not provided', async () => {
    const transport = createMockTransport();
    setTransport(transport);

    const publish = getPublisher(UserCreated);

    await runInContext({ traceId: 'ctx-trace-abc' }, () =>
      publish({ id: crypto.randomUUID(), email: 'test@example.com', name: 'Test' }),
    );

    expect(transport.published.length).toBe(1);
    const envelope = transport.published[0] as { traceId: string };
    expect(envelope.traceId).toBe('ctx-trace-abc');
  });

  it('should prefer explicit traceId over context traceId', async () => {
    const transport = createMockTransport();
    setTransport(transport);

    const publish = getPublisher(UserCreated);

    await runInContext({ traceId: 'ctx-trace-abc' }, () =>
      publish({ id: crypto.randomUUID(), email: 'test@example.com', name: 'Test' }, { traceId: 'explicit-trace' }),
    );

    expect(transport.published.length).toBe(1);
    const envelope = transport.published[0] as { traceId: string };
    expect(envelope.traceId).toBe('explicit-trace');
  });

  it('should auto-capture auth claims from context into attributes', async () => {
    const transport = createMockTransport();
    setTransport(transport);

    const publish = getPublisher(UserCreated);

    await runInContext(
      { traceId: 'trace-1', user: { sub: 'user-123', email: 'jane@example.com', azp: 'my-app' } },
      () => publish({ id: crypto.randomUUID(), email: 'test@example.com', name: 'Test' }),
    );

    expect(transport.published.length).toBe(1);
    const envelope = transport.published[0] as { attributes: Record<string, string> };
    expect(envelope.attributes['auth.sub']).toBe('user-123');
    expect(envelope.attributes['auth.email']).toBe('jane@example.com');
    expect(envelope.attributes['auth.azp']).toBe('my-app');
  });

  it('should protect auth attributes from being overridden by callers', async () => {
    const transport = createMockTransport();
    setTransport(transport);

    const publish = getPublisher(UserCreated);

    await runInContext({ traceId: 'trace-1', user: { sub: 'user-123', email: 'jane@example.com' } }, () =>
      publish(
        { id: crypto.randomUUID(), email: 'test@example.com', name: 'Test' },
        { attributes: { 'auth.sub': 'override-sub', region: 'eu' } },
      ),
    );

    expect(transport.published.length).toBe(1);
    const envelope = transport.published[0] as { attributes: Record<string, string> };
    // Auth attributes are protected — caller's auth.sub override is ignored
    expect(envelope.attributes['auth.sub']).toBe('user-123');
    expect(envelope.attributes['auth.email']).toBe('jane@example.com');
    // Non-auth attributes are passed through
    expect(envelope.attributes.region).toBe('eu');
  });

  it('should generate a new traceId when none exists in context or options', async () => {
    const transport = createMockTransport();
    setTransport(transport);

    const publish = getPublisher(UserCreated);
    await publish({ id: crypto.randomUUID(), email: 'test@example.com', name: 'Test' });

    expect(transport.published.length).toBe(1);
    const envelope = transport.published[0] as { traceId: string };
    expect(envelope.traceId).toBeDefined();
    expect(typeof envelope.traceId).toBe('string');
    expect(envelope.traceId.length).toBeGreaterThan(0);
  });
});
