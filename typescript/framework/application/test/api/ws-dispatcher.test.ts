import type { ServerWebSocket } from 'bun';
import { BadRequestException } from '@putnami/runtime';
import { describe, expect, it, mock } from 'bun:test';
import { upgradeToWebSocket } from '../../src/api/api-ws.utils';
import { buildWsContext, WebSocketDispatcher } from '../../src/api/ws-dispatcher';
import type { WebSocketContext, WsDataType, WsMessageContext } from '../../src/api/ws-context.type';
import type { HttpRequestContext } from '../../src/http/http-context.type';
import { RouteController } from '../../src/http/route.controller';

// ---------------------------------------------------------------------------
// Mock ServerWebSocket — records send()/close() so we can assert error frames.
// ---------------------------------------------------------------------------

interface MockWs {
  ws: ServerWebSocket<WsDataType>;
  sent: string[];
  closed: Array<{ code?: number; reason?: string }>;
}

function makeWs(url = 'http://localhost/ws/chat'): MockWs {
  const sent: string[] = [];
  const closed: Array<{ code?: number; reason?: string }> = [];
  const req = new Request(url);
  const context = {
    req,
    route: '/ws/chat',
    params: {},
    queryParams: () => ({}),
    domain: () => 'localhost',
    host: () => 'localhost',
    path: () => new URL(url).pathname,
    query: () => '',
    secured: () => false,
    user: undefined,
  } as unknown as Omit<WebSocketContext, 'ws'>;

  const ws = {
    data: { context },
    send: (msg: string) => {
      sent.push(msg);
      return msg.length;
    },
    close: (code?: number, reason?: string) => {
      closed.push({ code, reason });
    },
  } as unknown as ServerWebSocket<WsDataType>;

  return { ws, sent, closed };
}

const PATH = '/ws/chat';

describe('WebSocketDispatcher', () => {
  describe('message', () => {
    it('invokes the matched MESSAGE handler with the frame', async () => {
      const router = new RouteController<WebSocketContext>();
      const received: unknown[] = [];
      router.route('MESSAGE', PATH, (ctx) => {
        received.push((ctx as WsMessageContext).message);
      });
      const { ws } = makeWs();

      await new WebSocketDispatcher(router).message(ws, 'hello');

      expect(received).toEqual(['hello']);
    });

    it('is a no-op when no handler matches', async () => {
      const router = new RouteController<WebSocketContext>();
      const { ws, sent, closed } = makeWs();

      await new WebSocketDispatcher(router).message(ws, 'hello');

      expect(sent).toEqual([]);
      expect(closed).toEqual([]);
    });

    it('sends a structured error frame when a handler rejects with HttpException (no close)', async () => {
      const router = new RouteController<WebSocketContext>();
      router.route('MESSAGE', PATH, () => {
        throw new BadRequestException('bad frame');
      });
      const { ws, sent, closed } = makeWs();

      await new WebSocketDispatcher(router).message(ws, '{bad');

      expect(sent).toHaveLength(1);
      expect(JSON.parse(sent[0])).toEqual({ error: { code: 400, message: 'bad frame' } });
      // A bad MESSAGE frame must not tear down the whole connection.
      expect(closed).toEqual([]);
    });

    it('redacts and closes the connection on an unexpected handler error', async () => {
      const router = new RouteController<WebSocketContext>();
      router.route('MESSAGE', PATH, () => {
        throw new Error('secret db detail');
      });
      const { ws, sent, closed } = makeWs();

      await new WebSocketDispatcher(router).message(ws, 'x');

      expect(JSON.parse(sent[0])).toEqual({ error: { code: 500, message: 'Internal error' } });
      expect(sent[0]).not.toContain('secret db detail');
      expect(closed[0]?.code).toBe(1011);
    });
  });

  describe('open', () => {
    it('invokes the matched OPEN handler', async () => {
      const router = new RouteController<WebSocketContext>();
      const calls: string[] = [];
      router.route('OPEN', PATH, () => {
        calls.push('open');
      });
      const { ws } = makeWs();

      await new WebSocketDispatcher(router).open(ws);

      expect(calls).toEqual(['open']);
    });

    it('closes with policy-violation (1008) when OPEN validation rejects', async () => {
      const router = new RouteController<WebSocketContext>();
      router.route('OPEN', PATH, () => {
        throw new BadRequestException('invalid params');
      });
      const { ws, sent, closed } = makeWs();

      await new WebSocketDispatcher(router).open(ws);

      expect(JSON.parse(sent[0])).toEqual({ error: { code: 400, message: 'invalid params' } });
      expect(closed[0]?.code).toBe(1008);
    });
  });

  describe('close', () => {
    it('invokes the matched CLOSE handler with code and reason', async () => {
      const router = new RouteController<WebSocketContext>();
      const seen: Array<{ code: number; reason: string }> = [];
      router.route('CLOSE', PATH, (ctx) => {
        const c = ctx as unknown as { code: number; reason: string };
        seen.push({ code: c.code, reason: c.reason });
      });
      const { ws } = makeWs();

      await new WebSocketDispatcher(router).close(ws, 1000, 'bye');

      expect(seen).toEqual([{ code: 1000, reason: 'bye' }]);
    });

    it('swallows errors thrown by a CLOSE handler (socket already closing)', async () => {
      const router = new RouteController<WebSocketContext>();
      router.route('CLOSE', PATH, () => {
        throw new Error('boom');
      });
      const { ws } = makeWs();

      // Must not throw / reject.
      await new WebSocketDispatcher(router).close(ws, 1006, 'gone');
    });
  });
});

describe('buildWsContext', () => {
  it('wires request fields and merges the extra payload', () => {
    const { ws } = makeWs('http://localhost/ws/room?x=1');
    const ctx = buildWsContext<WsMessageContext>(ws, { ws, message: 'hi', method: 'MESSAGE' });

    expect(ctx.method).toBe('MESSAGE');
    expect(ctx.message).toBe('hi');
    expect(ctx.url).toBe('http://localhost/ws/room?x=1');
    expect(ctx.path()).toBe('ws/room');
    expect(ctx.queryParams()).toEqual({ x: '1' });
    expect(ctx.req).toBeInstanceOf(Request);
  });

  it('caches the connection-invariant base on ws.data and reuses it across frames', () => {
    const { ws } = makeWs('http://localhost/ws/room?x=1');

    const first = buildWsContext<WsMessageContext>(ws, { ws, message: 'a', method: 'MESSAGE' });
    expect(ws.data.__wsBase).toBeDefined();
    const second = buildWsContext<WsMessageContext>(ws, { ws, message: 'b', method: 'MESSAGE' });

    // Connection-invariant accessor closures are the same instances per frame
    // (computed once), while the per-frame payload still differs.
    expect(second.path).toBe(first.path);
    expect(second.queryParams).toBe(first.queryParams);
    expect(second.req).toBe(first.req);
    expect(first.message).toBe('a');
    expect(second.message).toBe('b');
  });

  it('overrides the cached base method with the per-frame method', () => {
    const { ws } = makeWs('http://localhost/ws/room');

    const msg = buildWsContext<WsMessageContext>(ws, { ws, message: 'a', method: 'MESSAGE' });
    const close = buildWsContext<WsMessageContext>(ws, { ws, method: 'CLOSE' });

    expect(msg.method).toBe('MESSAGE');
    expect(close.method).toBe('CLOSE');
  });
});

describe('upgradeToWebSocket', () => {
  function httpContext(opts: { upgradeResult: boolean; hasServer?: boolean }): HttpRequestContext {
    const req = new Request('http://localhost/ws/chat', { headers: { upgrade: 'websocket' } });
    return {
      req,
      route: '/ws/chat',
      params: {},
      queryParams: () => ({}),
      domain: () => 'localhost',
      host: () => 'localhost',
      path: () => '/ws/chat',
      query: () => '',
      secured: () => false,
      headers: req.headers,
      method: 'GET',
      url: req.url,
      server: opts.hasServer === false ? undefined : { upgrade: mock(() => opts.upgradeResult) },
    } as unknown as HttpRequestContext;
  }

  it('returns a 200 JSON response when the upgrade succeeds', () => {
    const res = upgradeToWebSocket(httpContext({ upgradeResult: true }));
    expect(res.status).toBe(200);
  });

  it('throws BadRequestException when the upgrade fails', () => {
    expect(() => upgradeToWebSocket(httpContext({ upgradeResult: false }))).toThrow(BadRequestException);
  });

  it('throws when no server is available to perform the upgrade', () => {
    expect(() => upgradeToWebSocket(httpContext({ upgradeResult: false, hasServer: false }))).toThrow(
      BadRequestException,
    );
  });
});
