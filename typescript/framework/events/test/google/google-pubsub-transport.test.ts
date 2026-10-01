import { describe, expect, it } from 'bun:test';
import { Uuid } from '@putnami/runtime';
import {
  googlePubSubTransport,
  type GooglePubSubClient,
  type GooglePubSubMessage,
  type GooglePubSubSubscription,
  type GooglePubSubTopic,
} from '../../src/google';
import { handler } from '../../src/handler/handler';
import { buildEnvelope } from '../../src/transport/transport';
import { topic } from '../../src/topic/topic';

const TestTopic = topic('google.pubsub.event', { id: Uuid, value: String });

async function waitUntil(condition: () => boolean, timeoutMs = 250): Promise<void> {
  const startedAt = Date.now();
  while (!condition()) {
    if (Date.now() - startedAt > timeoutMs) {
      throw new Error(`Timed out after ${timeoutMs}ms`);
    }
    await Bun.sleep(1);
  }
}

describe('GooglePubSubTransport', () => {
  it('publishes envelopes to mapped Pub/Sub topics', async () => {
    const client = new FakeGooglePubSubClient();
    const transport = googlePubSubTransport({
      client,
      topicName: (topic) => `events-${topic.replaceAll('.', '-')}`,
    });

    await transport.publish(TestTopic.name, buildEnvelope(TestTopic, { id: crypto.randomUUID(), value: 'published' }));

    const published = client.topic('events-google-pubsub-event').published[0];
    const envelope = JSON.parse(new TextDecoder().decode(published.data)) as { topic: string; key?: string };
    expect(envelope.topic).toBe(TestTopic.name);
  });

  it('acks messages when handlers succeed', async () => {
    const client = new FakeGooglePubSubClient();
    const transport = googlePubSubTransport({ client });
    const received: string[] = [];

    const def = handler(TestTopic)
      .options({ group: 'google-subscription', timeout: 0 })
      .handle(async () => {});
    await transport.subscribe(def, async (message) => {
      received.push((message.payload as { value: string }).value);
    });
    await transport.start();

    const message = new FakeGoogleMessage(buildEnvelope(TestTopic, { id: crypto.randomUUID(), value: 'handled' }));
    client.subscription('google-subscription').emit(message);

    await waitUntil(() => received.length === 1);
    await transport.stop();

    expect(received).toEqual(['handled']);
    expect(message.acked).toBe(true);
    expect(message.nacked).toBe(false);
  });

  it('nacks messages when handlers fail', async () => {
    const client = new FakeGooglePubSubClient();
    const transport = googlePubSubTransport({ client });

    const def = handler(TestTopic)
      .options({ group: 'google-subscription', timeout: 0 })
      .handle(async () => {});
    await transport.subscribe(def, async () => {
      throw new Error('boom');
    });
    await transport.start();

    const message = new FakeGoogleMessage(buildEnvelope(TestTopic, { id: crypto.randomUUID(), value: 'handled' }));
    client.subscription('google-subscription').emit(message);

    await waitUntil(() => message.nacked);
    await transport.stop();

    expect(message.acked).toBe(false);
    expect(message.nacked).toBe(true);
  });

  it('acks a malformed (non-JSON) message instead of redelivering forever', async () => {
    const client = new FakeGooglePubSubClient();
    const transport = googlePubSubTransport({ client });

    const def = handler(TestTopic)
      .options({ group: 'google-subscription', timeout: 0 })
      .handle(async () => {});
    await transport.subscribe(def, async () => {});
    await transport.start();

    // A truncated / non-JSON body throws during decode, before the dispatch
    // guard. The poison message must be acked (dropped), never left un-acked to
    // redeliver indefinitely.
    const message = new FakeGoogleMessage('this is not json', { raw: true });
    client.subscription('google-subscription').emit(message);

    await waitUntil(() => message.acked);
    await transport.stop();

    expect(message.acked).toBe(true);
    expect(message.nacked).toBe(false);
  });

  it('surfaces subscription errors without crashing', async () => {
    const client = new FakeGooglePubSubClient();
    const errors: Array<{ error: unknown; subscriptionName: string }> = [];
    const transport = googlePubSubTransport({
      client,
      onError: (error, context) => {
        errors.push({ error, subscriptionName: context.subscriptionName });
      },
    });

    const def = handler(TestTopic)
      .options({ group: 'google-subscription', timeout: 0 })
      .handle(async () => {});
    await transport.subscribe(def, async () => {});
    await transport.start();

    const error = new Error('streaming pull failed');
    client.subscription('google-subscription').emitError(error);

    await waitUntil(() => errors.length === 1);
    await transport.stop();

    expect(errors[0]?.error).toBe(error);
    expect(errors[0]?.subscriptionName).toBe('google-subscription');
  });
});

class FakeGooglePubSubClient implements GooglePubSubClient {
  private readonly topics = new Map<string, FakeGoogleTopic>();
  private readonly subscriptions = new Map<string, FakeGoogleSubscription>();

  topic(name: string): FakeGoogleTopic {
    const existing = this.topics.get(name);
    if (existing) return existing;
    const topic = new FakeGoogleTopic();
    this.topics.set(name, topic);
    return topic;
  }

  subscription(name: string): FakeGoogleSubscription {
    const existing = this.subscriptions.get(name);
    if (existing) return existing;
    const subscription = new FakeGoogleSubscription();
    this.subscriptions.set(name, subscription);
    return subscription;
  }
}

class FakeGoogleTopic implements GooglePubSubTopic {
  readonly published: Array<{ data: Uint8Array; attributes?: Record<string, string>; orderingKey?: string }> = [];

  async publishMessage(message: {
    data: Uint8Array;
    attributes?: Record<string, string>;
    orderingKey?: string;
  }): Promise<void> {
    this.published.push(message);
  }
}

class FakeGoogleSubscription implements GooglePubSubSubscription {
  private handlers = new Set<(message: GooglePubSubMessage) => void>();
  private errorHandlers = new Set<(error: unknown) => void>();

  on(event: 'message', handler: (message: GooglePubSubMessage) => void): void;
  on(event: 'error', handler: (error: unknown) => void): void;
  on(event: 'message' | 'error', handler: ((message: GooglePubSubMessage) => void) | ((error: unknown) => void)): void {
    if (event === 'message') {
      this.handlers.add(handler as (message: GooglePubSubMessage) => void);
    } else {
      this.errorHandlers.add(handler as (error: unknown) => void);
    }
  }

  removeListener(event: 'message', handler: (message: GooglePubSubMessage) => void): void;
  removeListener(event: 'error', handler: (error: unknown) => void): void;
  removeListener(
    event: 'message' | 'error',
    handler: ((message: GooglePubSubMessage) => void) | ((error: unknown) => void),
  ): void {
    if (event === 'message') {
      this.handlers.delete(handler as (message: GooglePubSubMessage) => void);
    } else {
      this.errorHandlers.delete(handler as (error: unknown) => void);
    }
  }

  emit(message: GooglePubSubMessage): void {
    for (const handler of this.handlers) {
      handler(message);
    }
  }

  emitError(error: unknown): void {
    for (const handler of this.errorHandlers) {
      handler(error);
    }
  }
}

class FakeGoogleMessage implements GooglePubSubMessage {
  readonly data: Uint8Array;
  acked = false;
  nacked = false;

  constructor(envelope: unknown, options?: { raw?: boolean }) {
    const body = options?.raw ? (envelope as string) : JSON.stringify(envelope);
    this.data = new TextEncoder().encode(body);
  }

  ack(): void {
    this.acked = true;
  }

  nack(): void {
    this.nacked = true;
  }
}
