import { describe, expect, it } from 'bun:test';
import { Uuid } from '@putnami/runtime';
import { postgresRealtime, type PostgresNotification, type PostgresNotifyClient } from '../../src/postgres';
import { topic } from '../../src/topic/topic';

const TestTopic = topic('postgres.realtime.event', { id: Uuid, value: String });

async function waitUntil(condition: () => boolean, timeoutMs = 250): Promise<void> {
  const startedAt = Date.now();
  while (!condition()) {
    if (Date.now() - startedAt > timeoutMs) {
      throw new Error(`Timed out after ${timeoutMs}ms`);
    }
    await Bun.sleep(1);
  }
}

describe('PostgresRealtimeBroker', () => {
  it('uses LISTEN/NOTIFY for live fanout', async () => {
    const client = new FakePostgresNotifyClient();
    const broker = postgresRealtime({ client, channelPrefix: 'rt' });
    const received: string[] = [];

    const sub = await broker.subscribe(TestTopic, async (message) => {
      received.push(message.payload.value);
      expect(message.topic).toBe(TestTopic.name);
    });

    await broker.publish(TestTopic, { id: crypto.randomUUID(), value: 'hello' });
    await waitUntil(() => received.length === 1);
    await sub.close();

    expect(received).toEqual(['hello']);
    expect(client.sql).toContain('LISTEN "rt_postgres_realtime_event"');
    expect(client.sql).toContain('UNLISTEN "rt_postgres_realtime_event"');
  });

  it('rejects replay requests because NOTIFY is live-only', async () => {
    const client = new FakePostgresNotifyClient();
    const broker = postgresRealtime({ client });

    await expect(broker.subscribe(TestTopic, async () => {}, { from: '0-0' })).rejects.toThrow('live-only');
  });

  it('guards the PostgreSQL NOTIFY payload size', async () => {
    const client = new FakePostgresNotifyClient();
    const broker = postgresRealtime({ client, maxPayloadBytes: 16 });

    await expect(broker.publish('large.topic', { value: 'this is too large' })).rejects.toThrow('exceeds');
  });
});

class FakePostgresNotifyClient implements PostgresNotifyClient {
  readonly sql: string[] = [];
  private readonly handlers = new Set<(notification: PostgresNotification) => void>();

  async query(sql: string, params?: unknown[]): Promise<void> {
    this.sql.push(sql);
    if (sql.startsWith('select pg_notify')) {
      const [channel, payload] = params as [string, string];
      for (const handler of this.handlers) {
        handler({ channel, payload });
      }
    }
  }

  on(event: 'notification', handler: (notification: PostgresNotification) => void): void {
    if (event === 'notification') {
      this.handlers.add(handler);
    }
  }

  off(event: 'notification', handler: (notification: PostgresNotification) => void): void {
    if (event === 'notification') {
      this.handlers.delete(handler);
    }
  }
}
