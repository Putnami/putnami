import { afterAll, beforeAll, describe, expect, it, spyOn } from 'bun:test';
import { api, staticFiles } from '@putnami/application';
import { createTestApp, type TestApp } from '@putnami/application/testing';
import notifications from '../src/api/notifications/get';

async function closeWebSocket(ws: WebSocket): Promise<void> {
  if (ws.readyState === WebSocket.CLOSED) return;

  await new Promise<void>((resolve) => {
    ws.addEventListener('close', () => resolve(), { once: true });
    ws.close();
  });
}

describe('real-time sample', () => {
  let testApp: TestApp;
  let wsUrl: string;

  beforeAll(async () => {
    testApp = await createTestApp({
      plugins: [api({ scanPath: 'src/api' }), staticFiles()],
    });
    wsUrl = testApp.baseUrl.replace('http', 'ws');
  });

  afterAll(async () => {
    await testApp.stop();
  });

  it('should serve the client UI at /', async () => {
    const res = await testApp.fetch('/');
    expect(res.status).toBe(200);
    expect(res.headers.get('content-type')).toContain('text/html');
  });

  it('should accept WebSocket connections at /chat', async () => {
    const ws = new WebSocket(`${wsUrl}/chat`);

    const connected = await new Promise<boolean>((resolve) => {
      ws.onopen = () => resolve(true);
      ws.onerror = () => resolve(false);
      setTimeout(() => resolve(false), 2000);
    });

    expect(connected).toBe(true);
    await closeWebSocket(ws);
  });

  it('should broadcast join and user list', async () => {
    const ws = new WebSocket(`${wsUrl}/chat`);
    await new Promise<void>((resolve) => {
      ws.onopen = () => resolve();
    });

    ws.send(JSON.stringify({ type: 'join', username: 'TestUser' }));

    const message = await new Promise<{ type: string; users?: string[] }>((resolve) => {
      ws.onmessage = (event) => {
        const data = JSON.parse(event.data);
        if (data.type === 'users') resolve(data);
      };
      setTimeout(() => resolve({ type: 'timeout' }), 2000);
    });

    expect(message.type).toBe('users');
    expect(message.users).toContain('TestUser');
    await closeWebSocket(ws);
  });

  it('should serve SSE notifications', async () => {
    const disconnect = new AbortController();
    const res = await testApp.fetch('/notifications', {
      headers: { Accept: 'text/event-stream' },
      signal: disconnect.signal,
    });
    expect(res.status).toBe(200);
    expect(res.headers.get('content-type')).toContain('text/event-stream');
    // Aborting closes the connection and errors the body stream, so a later
    // `res.body.cancel()` rejects with the AbortError.
    disconnect.abort();
  });

  it('releases the notification interval when a stream request is aborted', async () => {
    const controller = new AbortController();
    const interval = {} as ReturnType<typeof setInterval>;
    const setIntervalSpy = spyOn(globalThis, 'setInterval').mockImplementation((() => interval) as never);
    const clearIntervalSpy = spyOn(globalThis, 'clearInterval').mockImplementation((() => {}) as never);

    try {
      const handler = notifications.handler({
        signal: controller.signal,
        send: () => {},
      } as never);
      expect(setIntervalSpy).toHaveBeenCalledTimes(1);

      controller.abort();
      await handler;

      expect(clearIntervalSpy).toHaveBeenCalledWith(interval);
    } finally {
      setIntervalSpy.mockRestore();
      clearIntervalSpy.mockRestore();
    }
  });

  it('does not start notifications for an already-aborted stream request', async () => {
    const controller = new AbortController();
    controller.abort();
    const setIntervalSpy = spyOn(globalThis, 'setInterval').mockImplementation((() => 0) as never);
    const sent: unknown[] = [];

    try {
      await notifications.handler({
        signal: controller.signal,
        send: (message: unknown) => sent.push(message),
      } as never);

      expect(sent).toEqual([]);
      expect(setIntervalSpy).not.toHaveBeenCalled();
    } finally {
      setIntervalSpy.mockRestore();
    }
  });
});
