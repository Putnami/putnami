import { describe, expect, it } from 'bun:test';
import { type Logger, resetDefaultLogger, setRootLogger } from '@putnami/runtime';
import { MemoryLogger } from '@putnami/runtime/testing';
import { resolveStreamHandlers } from '../../../src/api/stream/stream-dispatcher';
import { MessageStream } from '../../../src/api/stream/message-stream';
import { buildStreamDefinition } from '../../../src/api/route/stream-endpoint';
import type { WsCloseContext, WsMessageContext, WsOpenContext } from '../../../src/api/ws-context.type';

function mockWs() {
  const sent: string[] = [];
  let closed = false;
  let closeCode: number | undefined;
  return {
    ws: {
      send: (data: string) => {
        sent.push(data);
      },
      close: (code?: number, _reason?: string) => {
        closed = true;
        closeCode = code;
      },
      data: {} as Record<string, unknown>,
    },
    sent,
    get closed() {
      return closed;
    },
    get closeCode() {
      return closeCode;
    },
  };
}

describe('resolveStreamHandlers', () => {
  it('should return OPEN, MESSAGE, and CLOSE handlers', () => {
    const def = buildStreamDefinition('bidirectional', async () => {});
    const handlers = resolveStreamHandlers(def);
    expect(typeof handlers.OPEN).toBe('function');
    expect(typeof handlers.MESSAGE).toBe('function');
    expect(typeof handlers.CLOSE).toBe('function');
  });

  describe('bidirectional mode', () => {
    it('should bridge messages to handler via MessageStream', async () => {
      const received: unknown[] = [];
      const def = buildStreamDefinition('bidirectional', async (ctx) => {
        for await (const msg of ctx.messages()) {
          received.push(msg);
          ctx.send({ echo: msg });
        }
      });

      const handlers = resolveStreamHandlers(def);
      const { ws, sent } = mockWs();

      const openCtx = {
        ws,
        req: new Request('http://localhost/ws'),
        headers: new Headers(),
        url: 'http://localhost/ws',
        params: {},
        queryParams: () => ({}),
        secured: () => false,
        host: () => 'localhost',
        domain: () => 'localhost',
        path: () => '/ws',
        query: () => '',
      } as unknown as WsOpenContext;

      // Open starts the handler
      handlers.OPEN?.(openCtx);
      await Promise.resolve();

      // Send messages
      const msgCtx = {
        ws,
        message: JSON.stringify({ text: 'hello' }),
        params: {},
        queryParams: () => ({}),
      } as unknown as WsMessageContext;
      handlers.MESSAGE?.(msgCtx);
      await Promise.resolve();
      await Promise.resolve();

      expect(received).toHaveLength(1);
      expect(sent.length).toBeGreaterThanOrEqual(1);

      // Close ends the stream
      const closeCtx = { ws } as unknown as WsCloseContext;
      handlers.CLOSE?.(closeCtx);
    });
  });

  describe('server-stream mode', () => {
    it('should allow handler to send messages', async () => {
      const def = buildStreamDefinition('server', async (ctx) => {
        ctx.send({ event: 'one' });
        ctx.send({ event: 'two' });
      });

      const handlers = resolveStreamHandlers(def);
      const { ws, sent } = mockWs();

      const openCtx = {
        ws,
        req: new Request('http://localhost/ws'),
        headers: new Headers(),
        url: 'http://localhost/ws',
        params: {},
        queryParams: () => ({}),
        secured: () => false,
        host: () => 'localhost',
        domain: () => 'localhost',
        path: () => '/ws',
        query: () => '',
      } as unknown as WsOpenContext;

      handlers.OPEN?.(openCtx);

      // Let handler run
      await new Promise((r) => setTimeout(r, 10));

      expect(sent).toContain(JSON.stringify({ event: 'one' }));
      expect(sent).toContain(JSON.stringify({ event: 'two' }));
    });
  });

  describe('message validation', () => {
    it('should validate messages against body schema', async () => {
      const def = buildStreamDefinition(
        'bidirectional',
        async (ctx) => {
          for await (const _msg of ctx.messages()) {
            // consume
          }
        },
        { body: { type: String, data: String } },
      );

      const handlers = resolveStreamHandlers(def);
      const { ws } = mockWs();

      const openCtx = {
        ws,
        req: new Request('http://localhost/ws'),
        headers: new Headers(),
        url: 'http://localhost/ws',
        params: {},
        queryParams: () => ({}),
        secured: () => false,
        host: () => 'localhost',
        domain: () => 'localhost',
        path: () => '/ws',
        query: () => '',
      } as unknown as WsOpenContext;

      handlers.OPEN?.(openCtx);

      // Valid message
      const validCtx = {
        ws,
        message: JSON.stringify({ type: 'chat', data: 'hello' }),
        params: {},
        queryParams: () => ({}),
      } as unknown as WsMessageContext;
      expect(() => handlers.MESSAGE?.(validCtx)).not.toThrow();

      // Invalid message (missing required field)
      const invalidCtx = {
        ws,
        message: JSON.stringify({ type: 'chat' }),
        params: {},
        queryParams: () => ({}),
      } as unknown as WsMessageContext;
      expect(() => handlers.MESSAGE?.(invalidCtx)).toThrow();

      // Clean up
      handlers.CLOSE?.({ ws } as unknown as WsCloseContext);
    });
  });

  describe('handler completion and rejection', () => {
    function openContext(ws: ReturnType<typeof mockWs>['ws']): WsOpenContext {
      return {
        ws,
        req: new Request('http://localhost/ws'),
        headers: new Headers(),
        url: 'http://localhost/ws',
        params: {},
        queryParams: () => ({}),
        secured: () => false,
        host: () => 'localhost',
        domain: () => 'localhost',
        path: () => '/ws',
        query: () => '',
      } as unknown as WsOpenContext;
    }

    it('sends the final value then closes for a resolving client-mode handler', async () => {
      const def = buildStreamDefinition('client', async () => ({ total: 3 }));
      const handlers = resolveStreamHandlers(def);
      // Keep the mock object — `closed`/`closeCode` are live getters.
      const m = mockWs();

      handlers.OPEN?.(openContext(m.ws));
      // Let the handler promise resolve and the .then(send/close) run.
      await new Promise((r) => setTimeout(r, 0));

      expect(m.sent).toContain(JSON.stringify({ total: 3 }));
      expect(m.closed).toBe(true);
      // A clean completion closes without an error code.
      expect(m.closeCode).toBeUndefined();
    });

    it('does not send a final value for non-client modes', async () => {
      const def = buildStreamDefinition('server', async () => ({ ignored: true }));
      const handlers = resolveStreamHandlers(def);
      const m = mockWs();

      handlers.OPEN?.(openContext(m.ws));
      await new Promise((r) => setTimeout(r, 0));

      expect(m.sent).toEqual([]);
      expect(m.closed).toBe(true);
    });

    it('logs the error (cause preserved) and closes 1011 when the handler rejects', async () => {
      const logger = new MemoryLogger();
      setRootLogger(logger);
      try {
        const boom = new Error('stream blew up');
        const def = buildStreamDefinition('bidirectional', async () => {
          throw boom;
        });
        const handlers = resolveStreamHandlers(def);
        const m = mockWs();

        handlers.OPEN?.(openContext(m.ws));
        await new Promise((r) => setTimeout(r, 0));

        expect(m.closeCode).toBe(1011);
        const entry = logger.entries.find((e) => e.logger === 'ws' && /stream handler error/.test(e.message));
        expect(entry).toBeDefined();
        expect(entry?.error?.message).toBe('stream blew up');
      } finally {
        resetDefaultLogger();
      }
    });
  });

  describe('lifecycle nil-guards (frames before OPEN)', () => {
    it('MESSAGE before OPEN is a no-op (no message stream yet)', () => {
      const handlers = resolveStreamHandlers(buildStreamDefinition('bidirectional', async () => {}));
      const { ws } = mockWs();

      const msgCtx = {
        ws,
        message: JSON.stringify({ x: 1 }),
        params: {},
        queryParams: () => ({}),
      } as unknown as WsMessageContext;

      // No __messageStream on ws.data → the `if (!messageStream) return` guard.
      expect(() => handlers.MESSAGE?.(msgCtx)).not.toThrow();
    });

    it('CLOSE before OPEN is a no-op (no abort/stream to tear down)', () => {
      const handlers = resolveStreamHandlers(buildStreamDefinition('bidirectional', async () => {}));
      const { ws } = mockWs();

      expect(() => handlers.CLOSE?.({ ws, path: () => '/ws' } as unknown as WsCloseContext)).not.toThrow();
    });
  });

  describe('message parsing fallback', () => {
    it('feeds invalid JSON to validation as the raw string (parseJson fallback)', async () => {
      const def = buildStreamDefinition(
        'bidirectional',
        async (ctx) => {
          for await (const _msg of ctx.messages()) {
            // consume
          }
        },
        { body: { type: String, data: String } },
      );
      const handlers = resolveStreamHandlers(def);
      const { ws } = mockWs();

      const openCtx = {
        ws,
        req: new Request('http://localhost/ws'),
        headers: new Headers(),
        url: 'http://localhost/ws',
        params: {},
        queryParams: () => ({}),
        secured: () => false,
        host: () => 'localhost',
        domain: () => 'localhost',
        path: () => '/ws',
        query: () => '',
      } as unknown as WsOpenContext;
      handlers.OPEN?.(openCtx);

      // Invalid JSON → parseJson returns the raw string → object-shaped body
      // schema rejects it.
      const badCtx = {
        ws,
        message: '{not valid json',
        params: {},
        queryParams: () => ({}),
      } as unknown as WsMessageContext;
      expect(() => handlers.MESSAGE?.(badCtx)).toThrow();

      handlers.CLOSE?.({ ws } as unknown as WsCloseContext);
    });
  });

  describe('inbound drop observability', () => {
    it('logs once at CLOSE when the inbound buffer dropped frames', () => {
      const warnings: Array<{ msg: unknown; meta: unknown }> = [];
      const fakeLogger = {
        named: () => fakeLogger,
        warn: (msg: unknown, meta: unknown) => warnings.push({ msg, meta }),
      };
      setRootLogger(fakeLogger as unknown as Logger);
      try {
        const handlers = resolveStreamHandlers(buildStreamDefinition('bidirectional', async () => {}));
        const { ws } = mockWs();

        // Pre-seed a stream that already hit its bound and dropped frames, so the
        // assertion doesn't depend on the default queue size.
        const stream = new MessageStream<unknown>(2);
        for (let i = 0; i < 5; i++) stream.push(i); // bound 2 → 3 dropped
        expect(stream.droppedCount).toBe(3);
        ws.data.__messageStream = stream;

        handlers.CLOSE?.({ ws, path: () => '/ws' } as unknown as WsCloseContext);

        expect(warnings).toHaveLength(1);
        expect(String(warnings[0].msg)).toContain('dropped frames');
        expect(warnings[0].meta).toMatchObject({ dropped: 3 });
      } finally {
        resetDefaultLogger();
      }
    });

    it('does not log at CLOSE when nothing was dropped', () => {
      const warnings: unknown[] = [];
      const fakeLogger = {
        named: () => fakeLogger,
        warn: (...args: unknown[]) => warnings.push(args),
      };
      setRootLogger(fakeLogger as unknown as Logger);
      try {
        const handlers = resolveStreamHandlers(buildStreamDefinition('bidirectional', async () => {}));
        const { ws } = mockWs();
        ws.data.__messageStream = new MessageStream<unknown>(2); // no pushes, no drops

        handlers.CLOSE?.({ ws, path: () => '/ws' } as unknown as WsCloseContext);

        expect(warnings).toHaveLength(0);
      } finally {
        resetDefaultLogger();
      }
    });
  });
});
