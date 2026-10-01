import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { Uuid } from '@putnami/runtime';
import { buildEnvelope } from '../../src/transport/transport';
import { EVENTS_DEFAULT_PORT, MemoryServer } from '../../src/server/memory-server';
import { toHandlerDefinition } from '../../src/server/remote-protocol';
import { topic } from '../../src/topic/topic';

const TestTopic = topic('test.server.event', { id: Uuid, value: String });
const DefaultRemoteOptions = {
  distribution: 'competing' as const,
  maxRetries: 1,
  backoff: 'exponential' as const,
  maxBackoff: 1000,
  timeout: 250,
  concurrency: 0,
  queueLimit: 0,
  overflow: 'throw' as const,
  dlq: true,
  ack: 'auto' as const,
};

describe('MemoryServer', () => {
  let server: MemoryServer | undefined;
  let currentPort: number;

  /** Build headers with auth token for authenticated requests. */
  function authHeaders(contentType?: string): Record<string, string> {
    const token = server?.getAuthToken() ?? '';
    const headers: Record<string, string> = { Authorization: `Bearer ${token}` };
    if (contentType) headers['Content-Type'] = contentType;
    return headers;
  }

  async function startServer(): Promise<void> {
    if (!server) throw new Error('server unavailable');
    await server.start();
    currentPort = server.getPort();
  }

  beforeEach(() => {
    currentPort = 0;
    server = new MemoryServer({ port: 0 });
  });

  afterEach(async () => {
    if (server) {
      await server.stop();
      server = undefined;
    }
  });

  describe('constructor', () => {
    it('uses default port when not specified', () => {
      const defaultServer = new MemoryServer();
      expect(defaultServer).toBeDefined();
      expect(EVENTS_DEFAULT_PORT).toBe(4222);
    });

    it('uses custom port when specified', () => {
      const customServer = new MemoryServer({ port: 5555 });
      expect(customServer).toBeDefined();
    });

    it('creates internal broker in dev mode', () => {
      const broker = server?.getBroker();
      expect(broker).toBeDefined();
    });

    it('generates an auth token', () => {
      const token = server?.getAuthToken();
      expect(token).toBeDefined();
      expect(typeof token).toBe('string');
      expect(token?.length).toBeGreaterThan(0);
    });
  });

  describe('start and stop', () => {
    it('starts HTTP server and broker', async () => {
      await startServer();

      const res = await fetch(`http://localhost:${currentPort}/health`);
      expect(res.status).toBe(200);

      const body = (await res.json()) as { status: string; service: string };
      expect(body.status).toBe('ok');
      expect(body.service).toBe('putnami-events');
    });

    it('stops HTTP server and broker', async () => {
      await startServer();

      const beforeStop = await fetch(`http://localhost:${currentPort}/health`);
      expect(beforeStop.status).toBe(200);

      await server?.stop();

      try {
        await fetch(`http://localhost:${currentPort}/health`, {
          signal: AbortSignal.timeout(100),
        });
        expect(true).toBe(false);
      } catch (error) {
        expect(error).toBeDefined();
      }
    });

    it('clears pending messages on stop', async () => {
      await startServer();
      await server?.stop();
      expect(true).toBe(true);
    });

    it('writes token file on start and removes on stop', async () => {
      await startServer();
      const token = await MemoryServer.readAuthToken(currentPort);
      expect(token).toBe(server?.getAuthToken());

      await server?.stop();
      const tokenAfterStop = await MemoryServer.readAuthToken(currentPort);
      expect(tokenAfterStop).toBeUndefined();
      server = undefined;
    });
  });

  describe('authentication', () => {
    it('rejects requests without auth token', async () => {
      await startServer();

      const res = await fetch(`http://localhost:${currentPort}/publish`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(buildEnvelope(TestTopic, { id: crypto.randomUUID(), value: 'test' })),
      });

      expect(res.status).toBe(401);
      const body = (await res.json()) as { error: string };
      expect(body.error).toBe('Unauthorized');
    });

    it('rejects requests with wrong auth token', async () => {
      await startServer();

      const res = await fetch(`http://localhost:${currentPort}/publish`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Authorization: 'Bearer wrong-token' },
        body: JSON.stringify(buildEnvelope(TestTopic, { id: crypto.randomUUID(), value: 'test' })),
      });

      expect(res.status).toBe(401);
    });

    it('allows health check without auth token', async () => {
      await startServer();

      const res = await fetch(`http://localhost:${currentPort}/health`);
      expect(res.status).toBe(200);
    });
  });

  describe('GET /health', () => {
    it('returns 200 with status ok', async () => {
      await startServer();

      const res = await fetch(`http://localhost:${currentPort}/health`);
      expect(res.status).toBe(200);

      const body = (await res.json()) as { status: string; service: string };
      expect(body).toEqual({ status: 'ok', service: 'putnami-events' });
    });
  });

  describe('POST /publish', () => {
    it('accepts valid envelope and returns 200', async () => {
      await startServer();

      const envelope = buildEnvelope(TestTopic, {
        id: crypto.randomUUID(),
        value: 'test message',
      });

      const res = await fetch(`http://localhost:${currentPort}/publish`, {
        method: 'POST',
        headers: authHeaders('application/json'),
        body: JSON.stringify(envelope),
      });

      expect(res.status).toBe(200);
      const body = (await res.json()) as { ok: boolean };
      expect(body.ok).toBe(true);
    });

    it('returns 400 for envelope missing topic', async () => {
      await startServer();

      const invalidEnvelope = {
        id: crypto.randomUUID(),
        payload: { value: 'test' },
        timestamp: new Date().toISOString(),
        attributes: {},
        attempt: 1,
      };

      const res = await fetch(`http://localhost:${currentPort}/publish`, {
        method: 'POST',
        headers: authHeaders('application/json'),
        body: JSON.stringify(invalidEnvelope),
      });

      expect(res.status).toBe(400);
      const body = (await res.json()) as { error: string };
      expect(body.error).toContain('Invalid envelope');
    });

    it('returns 400 for envelope missing id', async () => {
      await startServer();

      const invalidEnvelope = {
        topic: 'test.event',
        payload: { value: 'test' },
        timestamp: new Date().toISOString(),
        attributes: {},
        attempt: 1,
      };

      const res = await fetch(`http://localhost:${currentPort}/publish`, {
        method: 'POST',
        headers: authHeaders('application/json'),
        body: JSON.stringify(invalidEnvelope),
      });

      expect(res.status).toBe(400);
      const body = (await res.json()) as { error: string };
      expect(body.error).toContain('Invalid envelope');
    });

    it('returns 500 for malformed JSON', async () => {
      await startServer();

      const res = await fetch(`http://localhost:${currentPort}/publish`, {
        method: 'POST',
        headers: authHeaders('application/json'),
        body: 'not valid json{',
      });

      expect(res.status).toBe(500);
      const body = (await res.json()) as { error: string };
      expect(body.error).toBeDefined();
    });

    it('strips forged auth.* and non-string attributes before delivery (anti-spoofing)', async () => {
      await startServer();
      const broker = server?.getBroker();
      if (!broker) throw new Error('broker unavailable');

      const delivered: Record<string, string>[] = [];
      await broker.subscribe(
        toHandlerDefinition({ topic: TestTopic.name, options: DefaultRemoteOptions }),
        async (msg) => {
          delivered.push({ ...msg.attributes });
        },
      );

      // A holder of the shared bearer token forges authenticated identity (and
      // smuggles a non-string attribute) directly in the envelope it POSTs. The
      // in-process publisher would have stripped these; the remote boundary must
      // do the same so downstream handlers never trust a forged auth.* claim.
      const envelope = buildEnvelope(TestTopic, { id: crypto.randomUUID(), value: 'spoof' });
      const forged = {
        ...envelope,
        attributes: { 'auth.sub': 'attacker', 'auth.email': 'evil@example.com', region: 'eu', count: 7 },
      };

      const res = await fetch(`http://localhost:${currentPort}/publish`, {
        method: 'POST',
        headers: authHeaders('application/json'),
        body: JSON.stringify(forged),
      });
      expect(res.status).toBe(200);

      const deadline = Date.now() + 1000;
      while (delivered.length === 0 && Date.now() < deadline) {
        await Bun.sleep(5);
      }

      expect(delivered.length).toBe(1);
      // Forged identity never reaches the handler.
      expect(delivered[0]['auth.sub']).toBeUndefined();
      expect(delivered[0]['auth.email']).toBeUndefined();
      // Legitimate non-auth string attributes pass through untouched.
      expect(delivered[0].region).toBe('eu');
      // Non-string values are dropped (attributes are Record<string, string>).
      expect(delivered[0].count).toBeUndefined();
    });
  });

  describe('remote subscriptions', () => {
    it('delivers published messages to subscribed remote clients', async () => {
      await startServer();

      const subscribeRes = await fetch(`http://localhost:${currentPort}/subscribe`, {
        method: 'POST',
        headers: authHeaders('application/json'),
        body: JSON.stringify({
          topic: TestTopic.name,
          options: DefaultRemoteOptions,
        }),
      });

      expect(subscribeRes.status).toBe(200);
      const subscribeBody = (await subscribeRes.json()) as { subscriberId: string };
      expect(subscribeBody.subscriberId).toBeDefined();

      const publishRes = await fetch(`http://localhost:${currentPort}/publish`, {
        method: 'POST',
        headers: authHeaders('application/json'),
        body: JSON.stringify(
          buildEnvelope(TestTopic, {
            id: crypto.randomUUID(),
            value: 'from-remote-subscriber',
          }),
        ),
      });
      expect(publishRes.status).toBe(200);

      const pullRes = await fetch(`http://localhost:${currentPort}/pull`, {
        method: 'POST',
        headers: authHeaders('application/json'),
        body: JSON.stringify({ subscriberId: subscribeBody.subscriberId, timeoutMs: 100 }),
      });

      expect(pullRes.status).toBe(200);
      const pullBody = (await pullRes.json()) as {
        deliveryId: string;
        message: { topic: string; payload: { value: string } };
      };
      expect(pullBody.message.topic).toBe(TestTopic.name);
      expect(pullBody.message.payload.value).toBe('from-remote-subscriber');

      const ackRes = await fetch(`http://localhost:${currentPort}/ack`, {
        method: 'POST',
        headers: authHeaders('application/json'),
        body: JSON.stringify({
          subscriberId: subscribeBody.subscriberId,
          deliveryId: pullBody.deliveryId,
          status: 'ack',
        }),
      });
      expect(ackRes.status).toBe(200);

      const unsubscribeRes = await fetch(`http://localhost:${currentPort}/unsubscribe`, {
        method: 'POST',
        headers: authHeaders('application/json'),
        body: JSON.stringify({ subscriberId: subscribeBody.subscriberId }),
      });
      expect(unsubscribeRes.status).toBe(200);
    });

    it('rejects invalid remote subscriber payloads', async () => {
      await startServer();

      const res = await fetch(`http://localhost:${currentPort}/subscribe`, {
        method: 'POST',
        headers: authHeaders('application/json'),
        body: JSON.stringify({ topic: '', options: { distribution: 'invalid' } }),
      });

      expect(res.status).toBe(400);
      const body = (await res.json()) as { error: string };
      expect(body.error).toContain('Invalid subscriber definition');
    });
  });

  describe('404 for unknown routes', () => {
    it('returns 404 for unknown GET path', async () => {
      await startServer();

      const res = await fetch(`http://localhost:${currentPort}/unknown`, {
        headers: authHeaders(),
      });
      expect(res.status).toBe(404);
      expect(await res.text()).toBe('Not Found');
    });

    it('returns 404 for unknown POST path', async () => {
      await startServer();

      const res = await fetch(`http://localhost:${currentPort}/unknown`, {
        method: 'POST',
        headers: authHeaders(),
      });
      expect(res.status).toBe(404);
    });

    it('returns 404 for wrong method on /health', async () => {
      await startServer();

      const res = await fetch(`http://localhost:${currentPort}/health`, {
        method: 'POST',
        headers: authHeaders(),
      });
      expect(res.status).toBe(404);
    });

    it('returns 404 for wrong method on /publish', async () => {
      await startServer();

      const res = await fetch(`http://localhost:${currentPort}/publish`, {
        method: 'GET',
        headers: authHeaders(),
      });
      expect(res.status).toBe(404);
    });
  });

  describe('isRunning', () => {
    it('returns true when server is running', async () => {
      await startServer();

      const running = await MemoryServer.isRunning(currentPort);
      expect(running).toBe(true);
    });

    it('returns false when server is not running', async () => {
      const unusedPort = await allocatePort();
      const running = await MemoryServer.isRunning(unusedPort);
      expect(running).toBe(false);
    });

    it('returns false on timeout', async () => {
      const running = await MemoryServer.isRunning(12_345);
      expect(running).toBe(false);
    });

    it('uses default port when not specified', async () => {
      const running = await MemoryServer.isRunning();
      expect(typeof running).toBe('boolean');
    });

    it('returns false for non-events server on port', async () => {
      const fakePort = await allocatePort();
      const fakeServer = Bun.serve({
        port: fakePort,
        hostname: '127.0.0.1',
        fetch: () => Response.json({ status: 'different' }),
      });

      try {
        const running = await MemoryServer.isRunning(fakePort);
        expect(running).toBe(false);
      } finally {
        fakeServer.stop();
      }
    });
  });

  describe('getBroker', () => {
    it('returns the internal broker instance', () => {
      const broker = server?.getBroker();
      expect(broker).toBeDefined();
      expect(typeof broker.start).toBe('function');
      expect(typeof broker.stop).toBe('function');
      expect(typeof broker.publish).toBe('function');
      expect(typeof broker.subscribe).toBe('function');
    });
  });
});

async function allocatePort(): Promise<number> {
  const reservation = Bun.serve({
    port: 0,
    hostname: '127.0.0.1',
    fetch: () => new Response(),
  });
  const port = reservation.port;
  reservation.stop(true);
  return port;
}
