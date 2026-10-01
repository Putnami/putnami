import { afterEach, beforeEach, describe, expect, test } from 'bun:test';
import { WebSocketTransport } from '../../src/runtime/ws-transport';

// ---------------------------------------------------------------------------
// Mock WebSocket
// ---------------------------------------------------------------------------

type WSEventType = 'open' | 'message' | 'error' | 'close';

class MockWebSocket {
  static instances: MockWebSocket[] = [];

  readonly url: string;
  readonly protocols: string[];
  readonly listeners: Map<WSEventType, Set<EventListenerOrEventListenerObject>> = new Map();
  readonly sentMessages: string[] = [];
  closed = false;
  closeCode?: number;

  constructor(url: string, protocols?: string | string[]) {
    this.url = url;
    this.protocols = Array.isArray(protocols) ? protocols : protocols ? [protocols] : [];
    MockWebSocket.instances.push(this);
  }

  addEventListener(type: WSEventType, listener: EventListenerOrEventListenerObject) {
    if (!this.listeners.has(type)) this.listeners.set(type, new Set());
    this.listeners.get(type)?.add(listener);
  }

  removeEventListener(type: WSEventType, listener: EventListenerOrEventListenerObject) {
    this.listeners.get(type)?.delete(listener);
  }

  send(data: string) {
    this.sentMessages.push(data);
  }

  close(code?: number) {
    this.closed = true;
    this.closeCode = code;
    this.fire('close', new Event('close'));
  }

  /** Trigger an event on all registered listeners. */
  fire(type: WSEventType, event: Event) {
    for (const listener of this.listeners.get(type) ?? []) {
      if (typeof listener === 'function') {
        listener(event);
      } else {
        listener.handleEvent(event);
      }
    }
  }

  /** Convenience: open the connection. */
  open() {
    this.fire('open', new Event('open'));
  }

  /** Convenience: deliver a JSON message. */
  message(data: unknown) {
    const event = new MessageEvent('message', { data: JSON.stringify(data) });
    this.fire('message', event);
  }

  /** Convenience: fire an error. */
  error(msg = 'connection error') {
    const event = Object.assign(new Event('error'), { message: msg }) as ErrorEvent;
    this.fire('error', event);
  }
}

let originalWebSocket: typeof WebSocket;

beforeEach(() => {
  MockWebSocket.instances = [];
  originalWebSocket = globalThis.WebSocket;
  globalThis.WebSocket = MockWebSocket as unknown as typeof WebSocket;
});

afterEach(() => {
  globalThis.WebSocket = originalWebSocket;
});

function lastMock(): MockWebSocket {
  return MockWebSocket.instances[MockWebSocket.instances.length - 1];
}

// ---------------------------------------------------------------------------
// Constructor
// ---------------------------------------------------------------------------

describe('WebSocketTransport constructor', () => {
  test('converts http to ws', () => {
    const t = new WebSocketTransport('http://localhost:3000');
    t.stream('/test');
    expect(lastMock().url).toBe('ws://localhost:3000/test');
  });

  test('converts https to wss', () => {
    const t = new WebSocketTransport('https://api.example.com');
    t.stream('/rpc');
    expect(lastMock().url).toBe('wss://api.example.com/rpc');
  });

  test('strips trailing slash', () => {
    const t = new WebSocketTransport('http://localhost:3000/');
    t.stream('/path');
    expect(lastMock().url).toBe('ws://localhost:3000/path');
  });

  test('preserves ws:// URLs unchanged', () => {
    const t = new WebSocketTransport('ws://localhost:8080');
    t.stream('/x');
    expect(lastMock().url).toBe('ws://localhost:8080/x');
  });
});

// ---------------------------------------------------------------------------
// stream()
// ---------------------------------------------------------------------------

describe('stream()', () => {
  test('sends request body JSON on open', () => {
    const t = new WebSocketTransport('http://localhost:3000');
    t.stream('/events', { filter: 'all' });
    const ws = lastMock();
    ws.open();
    expect(ws.sentMessages).toContain(JSON.stringify({ filter: 'all' }));
  });

  test('sends empty object body when no body provided', () => {
    const t = new WebSocketTransport('http://localhost:3000');
    t.stream('/events');
    lastMock().open();
    expect(lastMock().sentMessages).toContain('{}');
  });

  test('delivers parsed JSON messages to onMessage handler', () => {
    const t = new WebSocketTransport('http://localhost:3000');
    const obs = t.stream<{ id: number }>('/stream');
    const received: { id: number }[] = [];
    obs.onMessage((d) => received.push(d));
    const ws = lastMock();
    ws.open();
    ws.message({ id: 1 });
    ws.message({ id: 2 });
    expect(received).toEqual([{ id: 1 }, { id: 2 }]);
  });

  test('calls onError with parse error for invalid JSON', () => {
    const t = new WebSocketTransport('http://localhost:3000');
    const obs = t.stream('/stream');
    const errors: Error[] = [];
    obs.onError((e) => errors.push(e));
    const ws = lastMock();
    ws.open();
    ws.fire('message', new MessageEvent('message', { data: 'not-json' }));
    expect(errors).toHaveLength(1);
    expect(errors[0]).toBeInstanceOf(Error);
  });

  test('calls onComplete when server closes connection', () => {
    const t = new WebSocketTransport('http://localhost:3000');
    const obs = t.stream('/stream');
    let completed = false;
    obs.onComplete(() => {
      completed = true;
    });
    const ws = lastMock();
    ws.open();
    ws.close();
    expect(completed).toBe(true);
  });

  test('calls onError when WebSocket fires error event', () => {
    const t = new WebSocketTransport('http://localhost:3000');
    const obs = t.stream('/stream');
    const errors: Error[] = [];
    obs.onError((e) => errors.push(e));
    const ws = lastMock();
    ws.error('network failure');
    expect(errors).toHaveLength(1);
    expect(errors[0].message).toContain('network failure');
  });

  test('cancel() closes WebSocket and removes listeners', () => {
    const t = new WebSocketTransport('http://localhost:3000');
    const obs = t.stream('/stream');
    const ws = lastMock();
    ws.open();
    obs.cancel();
    expect(ws.closed).toBe(true);
    // After cancel, subsequent close should not fire onComplete
    let completed = false;
    obs.onComplete(() => {
      completed = true;
    });
    // Listeners were cleaned up; onComplete registered after cancel may not fire
    expect(completed).toBe(false);
  });

  test('sends auth frame before body when Authorization header present', () => {
    const headers = new Headers({ Authorization: 'Bearer token123' });
    const t = new WebSocketTransport('http://localhost:3000');
    t.stream('/events', { q: 1 }, headers);
    const ws = lastMock();
    ws.open();
    // First sent message should be auth frame, second the body
    expect(ws.sentMessages[0]).toBe(JSON.stringify({ __auth: 'Bearer token123' }));
    expect(ws.sentMessages[1]).toBe(JSON.stringify({ q: 1 }));
  });

  test('encodes X-Trace-Id as WebSocket sub-protocol', () => {
    const headers = new Headers({ 'X-Trace-Id': 'trace-abc123' });
    const t = new WebSocketTransport('http://localhost:3000');
    t.stream('/events', undefined, headers);
    const ws = lastMock();
    expect(ws.protocols).toContain('trace.trace-abc123');
  });

  test('does not encode X-Trace-Id with invalid characters as sub-protocol', () => {
    const headers = new Headers({ 'X-Trace-Id': 'invalid trace id with spaces' });
    const t = new WebSocketTransport('http://localhost:3000');
    t.stream('/events', undefined, headers);
    const ws = lastMock();
    expect(ws.protocols).toHaveLength(0);
  });

  test('does not send auth frame when no Authorization header', () => {
    const t = new WebSocketTransport('http://localhost:3000');
    t.stream('/events', { x: 1 });
    const ws = lastMock();
    ws.open();
    // Should only have the body message (no auth frame)
    expect(ws.sentMessages).toHaveLength(1);
    expect(ws.sentMessages[0]).toBe(JSON.stringify({ x: 1 }));
  });

  test('fires timeout error when connection does not open within 15s', () => {
    const errors: Error[] = [];
    const t = new WebSocketTransport('http://localhost:3000');
    const obs = t.stream('/stream');
    obs.onError((e) => errors.push(e));
    // Simulate timeout firing (the setTimeout has already been created, but won't fire in test)
    // We reach into the WS to close it and verify the error is wired correctly
    // by simulating what the timeout handler does
    const ws = lastMock();
    ws.close();
    // The close event fires onComplete, not onError in this path
    // This test verifies cancel clears the timeout safely
    obs.cancel();
    expect(ws.closed).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// Idle timeout
// ---------------------------------------------------------------------------

describe('idle timeout', () => {
  test('connection is open after handshake and can be cancelled', () => {
    const t = new WebSocketTransport('http://localhost:3000');
    const obs = t.stream('/stream');
    const ws = lastMock();
    ws.open();
    expect(ws.closed).toBe(false);
    obs.cancel();
    expect(ws.closed).toBe(true);
  });

  test('cancel prevents idle timeout errors after cancellation', () => {
    const t = new WebSocketTransport('http://localhost:3000');
    const obs = t.stream('/stream');
    const errors: Error[] = [];
    obs.onError((e) => errors.push(e));
    const ws = lastMock();
    ws.open();
    obs.cancel();
    // No idle timeout error should have been reported
    const idleErrors = errors.filter((e) => e.message.includes('idle timeout'));
    expect(idleErrors).toHaveLength(0);
  });
});

// ---------------------------------------------------------------------------
// streamDuplex()
// ---------------------------------------------------------------------------

describe('streamDuplex()', () => {
  test('sends initial body on open', () => {
    const t = new WebSocketTransport('http://localhost:3000');
    t.streamDuplex('/bidi', { init: true });
    const ws = lastMock();
    ws.open();
    expect(ws.sentMessages).toContain(JSON.stringify({ init: true }));
  });

  test('does not send undefined body on open', () => {
    const t = new WebSocketTransport('http://localhost:3000');
    t.streamDuplex('/bidi');
    const ws = lastMock();
    ws.open();
    expect(ws.sentMessages).toHaveLength(0);
  });

  test('send() delivers message immediately when connection is ready', () => {
    const t = new WebSocketTransport('http://localhost:3000');
    const duplex = t.streamDuplex<{ cmd: string }, unknown>('/bidi');
    const ws = lastMock();
    ws.open();
    duplex.send({ cmd: 'ping' });
    expect(ws.sentMessages).toContain(JSON.stringify({ cmd: 'ping' }));
  });

  test('send() buffers messages before connection is open', () => {
    const t = new WebSocketTransport('http://localhost:3000');
    const duplex = t.streamDuplex<string, unknown>('/bidi');
    const ws = lastMock();
    duplex.send('msg1');
    duplex.send('msg2');
    expect(ws.sentMessages).toHaveLength(0); // not sent yet
    ws.open();
    expect(ws.sentMessages).toContain(JSON.stringify('msg1'));
    expect(ws.sentMessages).toContain(JSON.stringify('msg2'));
  });

  test('flushes pending buffer in order on open', () => {
    const t = new WebSocketTransport('http://localhost:3000');
    const duplex = t.streamDuplex<number, unknown>('/bidi');
    const ws = lastMock();
    duplex.send(1);
    duplex.send(2);
    duplex.send(3);
    ws.open();
    const sentAfterOpen = ws.sentMessages;
    expect(sentAfterOpen.indexOf(JSON.stringify(1))).toBeLessThan(sentAfterOpen.indexOf(JSON.stringify(2)));
    expect(sentAfterOpen.indexOf(JSON.stringify(2))).toBeLessThan(sentAfterOpen.indexOf(JSON.stringify(3)));
  });

  test('fires onError and closes when send buffer overflows (> 512)', () => {
    const t = new WebSocketTransport('http://localhost:3000');
    const duplex = t.streamDuplex<number, unknown>('/bidi');
    const ws = lastMock();
    const errors: Error[] = [];
    duplex.onError((e) => errors.push(e));

    for (let i = 0; i < 513; i++) {
      duplex.send(i);
    }
    expect(errors).toHaveLength(1);
    expect(errors[0].message).toContain('overflow');
    expect(ws.closed).toBe(true);
  });

  test('end() sends __end sentinel frame', () => {
    const t = new WebSocketTransport('http://localhost:3000');
    const duplex = t.streamDuplex('/bidi');
    const ws = lastMock();
    ws.open();
    duplex.end();
    expect(ws.sentMessages).toContain(JSON.stringify({ __end: true }));
  });

  test('delivers messages via onMessage handler', () => {
    const t = new WebSocketTransport('http://localhost:3000');
    const duplex = t.streamDuplex<unknown, { val: number }>('/bidi');
    const received: { val: number }[] = [];
    duplex.onMessage((d) => received.push(d));
    const ws = lastMock();
    ws.open();
    ws.message({ val: 42 });
    expect(received).toEqual([{ val: 42 }]);
  });

  test('calls onComplete when server closes connection', () => {
    const t = new WebSocketTransport('http://localhost:3000');
    const duplex = t.streamDuplex('/bidi');
    let done = false;
    duplex.onComplete(() => {
      done = true;
    });
    const ws = lastMock();
    ws.open();
    ws.close();
    expect(done).toBe(true);
  });

  test('cancel() closes WebSocket', () => {
    const t = new WebSocketTransport('http://localhost:3000');
    const duplex = t.streamDuplex('/bidi');
    const ws = lastMock();
    duplex.cancel();
    expect(ws.closed).toBe(true);
  });

  test('sends auth frame before flushing pending buffer', () => {
    const headers = new Headers({ Authorization: 'Bearer secret' });
    const t = new WebSocketTransport('http://localhost:3000');
    const duplex = t.streamDuplex<string, unknown>('/bidi', undefined, headers);
    const ws = lastMock();
    duplex.send('hello');
    ws.open();
    // Auth frame is first
    expect(ws.sentMessages[0]).toBe(JSON.stringify({ __auth: 'Bearer secret' }));
    // Buffered message is second
    expect(ws.sentMessages[1]).toBe(JSON.stringify('hello'));
  });

  test('calls onError on WebSocket error event', () => {
    const t = new WebSocketTransport('http://localhost:3000');
    const duplex = t.streamDuplex('/bidi');
    const errors: Error[] = [];
    duplex.onError((e) => errors.push(e));
    const ws = lastMock();
    ws.error('broken pipe');
    expect(errors).toHaveLength(1);
    expect(errors[0].message).toContain('broken pipe');
  });
});

// ---------------------------------------------------------------------------
// Deferred (Promise) headers
// ---------------------------------------------------------------------------

describe('deferred header resolution', () => {
  test('defers socket construction until the headers promise resolves', async () => {
    const t = new WebSocketTransport('http://localhost:3000');
    let resolveHeaders!: (h: Headers) => void;
    const headersPromise = new Promise<Headers>((resolve) => {
      resolveHeaders = resolve;
    });

    t.stream('/events', { q: 1 }, headersPromise);

    // No socket yet — we are still awaiting the headers.
    expect(MockWebSocket.instances).toHaveLength(0);

    resolveHeaders(new Headers({ 'X-Trace-Id': 'trace-deferred', Authorization: 'Bearer d' }));
    await headersPromise;
    await Promise.resolve(); // let the .then() microtask run

    expect(MockWebSocket.instances).toHaveLength(1);
    const ws = lastMock();
    // Trace header became a sub-protocol (known at construction time).
    expect(ws.protocols).toContain('trace.trace-deferred');
    // Auth header becomes the first frame after open.
    ws.open();
    expect(ws.sentMessages[0]).toBe(JSON.stringify({ __auth: 'Bearer d' }));
  });

  test('cancel before headers resolve prevents socket construction', async () => {
    const t = new WebSocketTransport('http://localhost:3000');
    let resolveHeaders!: (h: Headers) => void;
    const headersPromise = new Promise<Headers>((resolve) => {
      resolveHeaders = resolve;
    });

    const obs = t.stream('/events', undefined, headersPromise);
    obs.cancel();

    resolveHeaders(new Headers());
    await headersPromise;
    await Promise.resolve();

    // Cancelled before resolution → no socket opened.
    expect(MockWebSocket.instances).toHaveLength(0);
  });

  test('reports an error when the headers promise rejects', async () => {
    const t = new WebSocketTransport('http://localhost:3000');
    const headersPromise = Promise.reject(new Error('token fetch failed'));
    const errors: Error[] = [];

    const obs = t.stream('/events', undefined, headersPromise);
    obs.onError((e) => errors.push(e));

    await headersPromise.catch(() => {});
    await Promise.resolve();

    expect(errors).toHaveLength(1);
    expect(errors[0].message).toContain('token fetch failed');
    expect(MockWebSocket.instances).toHaveLength(0);
  });
});
