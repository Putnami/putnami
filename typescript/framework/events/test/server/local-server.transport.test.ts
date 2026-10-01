import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { Uuid } from '@putnami/runtime';
import { MemoryServer } from '../../src/server/memory-server';
import { LocalServerTransport } from '../../src/server/local-server.transport';
import { buildEnvelope } from '../../src/transport/transport';
import { topic } from '../../src/topic/topic';
import type { HandlerDefinition } from '../../src/handler/handler';
import type { Message } from '../../src/topic/message';

const TestTopic = topic('test.transport.event', { id: Uuid, value: String });
const DefaultOptions = {
  distribution: 'competing' as const,
  maxRetries: 1,
  backoff: 'exponential' as const,
  maxBackoff: 1000,
  timeout: 5000,
  concurrency: 0,
  queueLimit: 0,
  overflow: 'throw' as const,
  dlq: true,
  ack: 'auto' as const,
};

function makeHandlerDef(callback: (msg: Message<unknown>) => Promise<void>): HandlerDefinition {
  return {
    __handler: 'putnami:event-handler' as const,
    topic: TestTopic,
    options: DefaultOptions,
    handler: callback,
  };
}

describe('LocalServerTransport', () => {
  let server: MemoryServer;
  let transport: LocalServerTransport;
  let currentPort: number;

  beforeEach(async () => {
    server = new MemoryServer({ port: 0 });
    await server.start();
    currentPort = server.getPort();
    transport = new LocalServerTransport(`http://127.0.0.1:${currentPort}`, server.getAuthToken());
  });

  afterEach(async () => {
    await transport.stop();
    await server.stop();
  });

  it('rejects a non-http endpoint to guard against SSRF / misconfiguration', () => {
    expect(() => new LocalServerTransport('file:///etc/passwd')).toThrow(/scheme/);
    expect(() => new LocalServerTransport('gopher://169.254.169.254/')).toThrow(/scheme/);
    expect(() => new LocalServerTransport('not a url')).toThrow(/not a valid URL/);
  });

  describe('publish', () => {
    it('sends an envelope to the server', async () => {
      const envelope = buildEnvelope(TestTopic, {
        id: crypto.randomUUID(),
        value: 'hello',
      });

      // Should not throw
      await transport.publish(TestTopic.name, envelope);
    });
  });

  describe('subscribe and delivery', () => {
    it('receives published messages via the pull loop', async () => {
      const received: Message<unknown>[] = [];
      let resolveDelivered: () => void;
      const delivered = new Promise<void>((resolve) => {
        resolveDelivered = resolve;
      });
      const def = makeHandlerDef(async (msg) => {
        received.push(msg);
        resolveDelivered();
      });
      // Await the subscription: a message published before the server knows the
      // subscriber has nobody to be queued for and is never delivered.
      await transport.subscribe(def, def.handler);
      await transport.start();

      // Publish a message via the server's broker (simulating another service)
      const envelope = buildEnvelope(TestTopic, {
        id: crypto.randomUUID(),
        value: 'from-publisher',
      });
      await server.getBroker().publish(TestTopic.name, envelope);

      // Wait for the transport to pull and deliver the message
      await delivered;

      expect(received).toHaveLength(1);
      expect(received[0].topic).toBe(TestTopic.name);
      expect((received[0].payload as { value: string }).value).toBe('from-publisher');
    });

    it('rejects a schema-invalid payload before it reaches the handler', async () => {
      const received: Message<unknown>[] = [];
      let resolveDelivered: () => void;
      const delivered = new Promise<void>((resolve) => {
        resolveDelivered = resolve;
      });
      const def = makeHandlerDef(async (msg) => {
        received.push(msg);
        if ((msg.payload as { value?: string }).value === 'valid') {
          resolveDelivered();
        }
      });
      await transport.subscribe(def, def.handler);
      await transport.start();

      // A malformed payload (missing the required `value`) as a buggy or malicious
      // producer might send. The local publisher normally validates, but the
      // consumer must not trust that — it validates against the topic schema too.
      const invalid = buildEnvelope(TestTopic, { id: crypto.randomUUID() });
      await server.getBroker().publish(TestTopic.name, invalid);

      const valid = buildEnvelope(TestTopic, { id: crypto.randomUUID(), value: 'valid' });
      await server.getBroker().publish(TestTopic.name, valid);

      await delivered;

      // Only the valid message reached the handler; the invalid one was rejected
      // before invocation rather than delivered unvalidated.
      expect(received).toHaveLength(1);
      expect((received[0].payload as { value: string }).value).toBe('valid');
    });

    it('delivers multiple messages in order', async () => {
      const values: string[] = [];
      let resolveAll: () => void;
      const allDelivered = new Promise<void>((resolve) => {
        resolveAll = resolve;
      });

      const def = makeHandlerDef(async (msg) => {
        values.push((msg.payload as { value: string }).value);
        if (values.length === 3) resolveAll();
      });
      await transport.subscribe(def, def.handler);
      await transport.start();

      for (const v of ['first', 'second', 'third']) {
        const envelope = buildEnvelope(TestTopic, {
          id: crypto.randomUUID(),
          value: v,
        });
        await server.getBroker().publish(TestTopic.name, envelope);
      }

      await allDelivered;

      expect(values).toEqual(['first', 'second', 'third']);
    });
  });

  describe('handler errors', () => {
    it('continues processing after handler throws', async () => {
      const values: string[] = [];
      let resolveSecond: () => void;
      const secondDelivered = new Promise<void>((resolve) => {
        resolveSecond = resolve;
      });

      let callCount = 0;
      const def = makeHandlerDef(async (msg) => {
        callCount++;
        if (callCount === 1) {
          throw new Error('handler failure');
        }
        values.push((msg.payload as { value: string }).value);
        resolveSecond();
      });
      await transport.subscribe(def, def.handler);
      await transport.start();

      // First message — handler throws
      await server
        .getBroker()
        .publish(TestTopic.name, buildEnvelope(TestTopic, { id: crypto.randomUUID(), value: 'fail' }));

      // Second message — handler succeeds
      await server
        .getBroker()
        .publish(TestTopic.name, buildEnvelope(TestTopic, { id: crypto.randomUUID(), value: 'ok' }));

      await secondDelivered;
      expect(values).toContain('ok');
    });
  });

  describe('stop', () => {
    it('stops the pull loop and unsubscribes', async () => {
      const def = makeHandlerDef(async () => {});
      await transport.subscribe(def, def.handler);
      await transport.start();

      // Should not throw or hang
      await transport.stop();
    });

    it('can stop before start without error', async () => {
      await transport.stop();
    });
  });
});
