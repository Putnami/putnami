import { describe, expect, it } from 'bun:test';
import { Uuid } from '@putnami/runtime';
import { eventClient } from '../../src/client';
import { topic } from '../../src/topic';

const UserUpdated = topic('user.updated', { id: Uuid, name: String }, { channel: 'realtime' });

describe('eventClient()', () => {
  it('opens a websocket and sends auth plus subscribe frames', async () => {
    MockWebSocket.instances = [];
    const client = eventClient({
      url: 'https://api.example.com/events',
      token: 'Bearer token-123',
      headers: { 'x-device-id': 'device-1' },
      webSocket: MockWebSocket as unknown as typeof WebSocket,
    });

    const subscription = client.subscribe(UserUpdated, () => undefined, { from: 'latest' });
    const ws = lastMock();
    ws.open();
    await subscription.ready;

    expect(ws.url).toBe('wss://api.example.com/events');
    expect(ws.sentMessages).toEqual([
      JSON.stringify({
        protocol: 'putnami.events.v1',
        type: 'auth',
        token: 'Bearer token-123',
        headers: { 'x-device-id': 'device-1' },
      }),
      JSON.stringify({
        protocol: 'putnami.events.v1',
        type: 'subscribe',
        topic: 'user.updated',
        channel: 'realtime',
        from: 'latest',
      }),
    ]);
  });

  it('delivers typed event messages', async () => {
    MockWebSocket.instances = [];
    const received: string[] = [];
    const client = eventClient({
      url: 'ws://localhost/events',
      webSocket: MockWebSocket as unknown as typeof WebSocket,
    });

    const subscription = client.subscribe(UserUpdated, (message) => {
      expect(message.channel).toBe('realtime');
      expect(message.key).toBe('user-1');
      expect(message.dedupeKey).toBe('dedupe-1');
      expect(message.topicVersion).toBe('v1');
      received.push(message.payload.name);
    });
    const ws = lastMock();
    ws.open();
    await subscription.ready;

    ws.message({
      protocol: 'putnami.events.v1',
      type: 'event',
      id: 'msg-1',
      topic: 'user.updated',
      channel: 'realtime',
      payload: { id: crypto.randomUUID(), name: 'Jane' },
      key: 'user-1',
      dedupeKey: 'dedupe-1',
      topicVersion: 'v1',
      timestamp: '2026-01-01T00:00:00.000Z',
      attributes: { region: 'eu' },
      traceId: 'trace-1',
    });

    expect(received).toEqual(['Jane']);
  });

  it('reports invalid payloads without invoking the handler', async () => {
    MockWebSocket.instances = [];
    const errors: string[] = [];
    let received = 0;
    const client = eventClient({
      url: 'ws://localhost/events',
      webSocket: MockWebSocket as unknown as typeof WebSocket,
    });

    const subscription = client.subscribe(
      UserUpdated,
      () => {
        received++;
      },
      { onError: (error) => errors.push(error.message) },
    );
    const ws = lastMock();
    ws.open();
    await subscription.ready;

    ws.message({
      protocol: 'putnami.events.v1',
      type: 'event',
      id: 'msg-1',
      topic: 'user.updated',
      payload: { id: 'not-a-uuid', name: 'Jane' },
    });

    expect(received).toBe(0);
    expect(errors[0]).toContain("Invalid payload for topic 'user.updated'");
  });

  it('routes async handler failures to onError', async () => {
    MockWebSocket.instances = [];
    const errors: string[] = [];
    const client = eventClient({
      url: 'ws://localhost/events',
      webSocket: MockWebSocket as unknown as typeof WebSocket,
    });

    const subscription = client.subscribe(
      UserUpdated,
      async () => {
        throw new Error('handler boom');
      },
      { onError: (error) => errors.push(error.message) },
    );
    const ws = lastMock();
    ws.open();
    await subscription.ready;

    ws.message({
      protocol: 'putnami.events.v1',
      type: 'event',
      id: 'msg-1',
      topic: 'user.updated',
      payload: { id: crypto.randomUUID(), name: 'Jane' },
    });

    await Bun.sleep(1);
    expect(errors).toEqual(['handler boom']);
  });

  it('does not advance the replay cursor past a failed handler', async () => {
    MockWebSocket.instances = [];
    const client = eventClient({
      url: 'ws://localhost/events',
      reconnect: { retries: 1, minDelayMs: 0, maxDelayMs: 0 },
      webSocket: MockWebSocket as unknown as typeof WebSocket,
    });

    const subscription = client.subscribe(
      UserUpdated,
      async () => {
        throw new Error('handler boom');
      },
      { onError: () => undefined },
    );
    const first = lastMock();
    first.open();
    await subscription.ready;
    first.message({
      protocol: 'putnami.events.v1',
      type: 'event',
      id: 'msg-1',
      topic: 'user.updated',
      payload: { id: crypto.randomUUID(), name: 'Jane' },
    });
    // Let the rejected handler settle before forcing a reconnect.
    await Bun.sleep(1);
    first.close();

    await Bun.sleep(1);
    const second = lastMock();
    second.open();
    await Bun.sleep(0);

    // The handler rejected, so the cursor must not have advanced to msg-1 — the
    // resubscribe must resume from latest so the failed message is replayed.
    expect(second.sentMessages).toEqual([
      JSON.stringify({
        protocol: 'putnami.events.v1',
        type: 'subscribe',
        topic: 'user.updated',
        channel: 'realtime',
      }),
    ]);
  });

  it('does not let a later async handler advance the replay cursor past an earlier failure', async () => {
    MockWebSocket.instances = [];
    const firstDelivery = deferred<void>();
    const handled: string[] = [];
    const errors: string[] = [];
    const client = eventClient({
      url: 'ws://localhost/events',
      reconnect: { retries: 1, minDelayMs: 0, maxDelayMs: 0 },
      webSocket: MockWebSocket as unknown as typeof WebSocket,
    });

    const subscription = client.subscribe(
      UserUpdated,
      (message) => {
        handled.push(message.id);
        if (message.id === 'msg-1') {
          return firstDelivery.promise;
        }
      },
      { onError: (error) => errors.push(error.message) },
    );
    const firstWs = lastMock();
    firstWs.open();
    await subscription.ready;

    firstWs.message({
      protocol: 'putnami.events.v1',
      type: 'event',
      id: 'msg-1',
      topic: 'user.updated',
      payload: { id: crypto.randomUUID(), name: 'Jane' },
    });
    firstWs.message({
      protocol: 'putnami.events.v1',
      type: 'event',
      id: 'msg-2',
      topic: 'user.updated',
      payload: { id: crypto.randomUUID(), name: 'June' },
    });

    await Bun.sleep(1);
    expect(handled).toEqual(['msg-1']);

    firstDelivery.reject(new Error('handler boom'));
    await waitUntil(() => handled.includes('msg-2'));
    firstWs.close();

    await Bun.sleep(1);
    const second = lastMock();
    second.open();
    await Bun.sleep(0);

    expect(errors).toEqual(['handler boom']);
    expect(second.sentMessages).toEqual([
      JSON.stringify({
        protocol: 'putnami.events.v1',
        type: 'subscribe',
        topic: 'user.updated',
        channel: 'realtime',
      }),
    ]);
  });

  it('resubscribes from the last delivered id when reconnecting', async () => {
    MockWebSocket.instances = [];
    const client = eventClient({
      url: 'ws://localhost/events',
      reconnect: { retries: 1, minDelayMs: 0, maxDelayMs: 0 },
      webSocket: MockWebSocket as unknown as typeof WebSocket,
    });

    const subscription = client.subscribe(UserUpdated, () => undefined);
    const first = lastMock();
    first.open();
    await subscription.ready;
    first.message({
      protocol: 'putnami.events.v1',
      type: 'event',
      id: 'msg-1',
      topic: 'user.updated',
      payload: { id: crypto.randomUUID(), name: 'Jane' },
    });
    first.close();

    await Bun.sleep(1);
    const second = lastMock();
    second.open();
    await Bun.sleep(0);

    expect(second.sentMessages).toEqual([
      JSON.stringify({
        protocol: 'putnami.events.v1',
        type: 'subscribe',
        topic: 'user.updated',
        channel: 'realtime',
        from: 'msg-1',
      }),
    ]);
  });

  it('closes all active subscriptions', async () => {
    MockWebSocket.instances = [];
    const client = eventClient({
      url: 'ws://localhost/events',
      webSocket: MockWebSocket as unknown as typeof WebSocket,
    });

    client.subscribe(UserUpdated, () => undefined);
    const ws = lastMock();
    ws.open();
    client.close();

    expect(ws.closed).toBe(true);
  });
});

type WSEventType = 'open' | 'message' | 'error' | 'close';

class MockWebSocket {
  static instances: MockWebSocket[] = [];

  readonly listeners = new Map<WSEventType, Set<EventListenerOrEventListenerObject>>();
  readonly sentMessages: string[] = [];
  readonly url: string;
  closed = false;

  constructor(url: string) {
    this.url = url;
    MockWebSocket.instances.push(this);
  }

  addEventListener(type: WSEventType, listener: EventListenerOrEventListenerObject): void {
    const listeners = this.listeners.get(type) ?? new Set();
    listeners.add(listener);
    this.listeners.set(type, listeners);
  }

  removeEventListener(type: WSEventType, listener: EventListenerOrEventListenerObject): void {
    this.listeners.get(type)?.delete(listener);
  }

  send(data: string): void {
    this.sentMessages.push(data);
  }

  close(): void {
    this.closed = true;
    this.fire('close', new CloseEvent('close'));
  }

  open(): void {
    this.fire('open', new Event('open'));
  }

  message(data: unknown): void {
    this.fire('message', new MessageEvent('message', { data: JSON.stringify(data) }));
  }

  fire(type: WSEventType, event: Event): void {
    for (const listener of this.listeners.get(type) ?? []) {
      if (typeof listener === 'function') {
        listener(event);
      } else {
        listener.handleEvent(event);
      }
    }
  }
}

function lastMock(): MockWebSocket {
  return MockWebSocket.instances[MockWebSocket.instances.length - 1];
}

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function waitUntil(predicate: () => boolean, timeoutMs = 100): Promise<void> {
  const start = Date.now();
  return new Promise<void>((resolve, reject) => {
    const poll = () => {
      if (predicate()) {
        resolve();
        return;
      }
      if (Date.now() - start > timeoutMs) {
        reject(new Error('timed out waiting for condition'));
        return;
      }
      setTimeout(poll, 1);
    };
    poll();
  });
}
