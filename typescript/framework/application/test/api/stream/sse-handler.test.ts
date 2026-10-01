import { describe, expect, it } from 'bun:test';
import { resetDefaultLogger, setRootLogger } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { MemoryLogger } from '@putnami/runtime/testing';
import { createSseHandler } from '../../../src/api/stream/sse-handler';
import { buildStreamDefinition } from '../../../src/api/route/stream-endpoint';
import { HttpResponse } from '../../../src/http/http-response';
import type { HttpRequestContext } from '../../../src/http/http-context.type';

function mockSseContext(overrides: Partial<HttpRequestContext> = {}): HttpRequestContext {
  return {
    req: new Request('http://localhost/events'),
    headers: new Headers(),
    url: 'http://localhost/events',
    params: {},
    queryParams: () => ({}),
    secured: () => false,
    host: () => 'localhost',
    domain: () => 'localhost',
    path: () => '/events',
    query: () => '',
    ...overrides,
  } as unknown as HttpRequestContext;
}

describe('createSseHandler', () => {
  it('should return a HttpResponse with text/event-stream content type', async () => {
    const def = buildStreamDefinition('server', async (ctx) => {
      ctx.send({ event: 'hello' });
    });

    const handler = createSseHandler(def);
    const result = await handler(mockSseContext());

    expect(result).toBeInstanceOf(HttpResponse);
    const response = result as HttpResponse;
    expect(response.getHeader('Content-Type')).toBe('text/event-stream');
    expect(response.getHeader('Cache-Control')).toBe('no-cache');
  });

  it('should emit SSE data frames', async () => {
    const def = buildStreamDefinition('server', async (ctx) => {
      ctx.send({ count: 1 });
      ctx.send({ count: 2 });
    });

    const handler = createSseHandler(def);
    const result = await handler(mockSseContext());
    const response = (result as HttpResponse).get();
    const text = await response.text();

    expect(text).toContain('data: {"count":1}\n\n');
    expect(text).toContain('data: {"count":2}\n\n');
  });

  specTest(
    'bounds a synchronous producer and preserves a terminal error slot',
    {
      feature: 'typescript/server-streams',
      requirement: 'sse-admission-and-bounds',
      check: 'the-declared-queue-depth-and-frame-size-bound-the-producer',
    },
    async () => {
      const def = buildStreamDefinition('server', async (ctx) => {
        ctx.send({ count: 1 });
        ctx.send({ count: 2 });
      });

      const handler = createSseHandler(def, { maxBufferedMessages: 1 });
      const result = await handler(mockSseContext());
      const text = await (result as HttpResponse).get().text();

      expect(text).toContain('data: {"count":1}\n\n');
      expect(text).not.toContain('data: {"count":2}\n\n');
      expect(text).toContain(
        'event: error\ndata: {"status":500,"code":"http.internal_server","error":"Internal Server Error","message":"Internal Server Error"}\n\n',
      );
    },
  );

  it('should validate params schema', async () => {
    const def = buildStreamDefinition(
      'server',
      async (ctx) => {
        ctx.send({ id: ctx.params.id });
      },
      { params: { id: String } },
    );

    const handler = createSseHandler(def);
    const result = await handler(mockSseContext({ params: { id: 'abc' } }));
    const response = (result as HttpResponse).get();
    const text = await response.text();
    expect(text).toContain('data: {"id":"abc"}\n\n');
  });

  it('should validate and coerce query schema', async () => {
    const def = buildStreamDefinition(
      'server',
      async (ctx) => {
        ctx.send({ limit: (ctx.queryParams() as { limit: number }).limit });
      },
      { query: { limit: Number } },
    );

    const handler = createSseHandler(def);
    const result = await handler(mockSseContext({ queryParams: () => ({ limit: '5' }) }));
    const response = (result as HttpResponse).get();
    const text = await response.text();
    // Coerced from the string "5" to the number 5.
    expect(text).toContain('data: {"limit":5}\n\n');
  });

  it('logs a throwing handler error (cause preserved) and closes the stream cleanly', async () => {
    const logger = new MemoryLogger();
    setRootLogger(logger);
    try {
      const boom = new Error('handler exploded');
      const def = buildStreamDefinition('server', async () => {
        throw boom;
      });

      const handler = createSseHandler(def);
      const result = await handler(mockSseContext());
      const response = (result as HttpResponse).get();
      // finally → controller.close() must still run, so reading the stream
      // resolves with the stable terminal framework error rather than hanging.
      const text = await response.text();
      expect(text).toBe(
        'event: error\ndata: {"status":500,"code":"http.internal_server","error":"Internal Server Error","message":"Internal Server Error"}\n\n',
      );

      const entry = logger.entries.find((e) => e.logger === 'http' && /SSE stream handler error/.test(e.message));
      expect(entry).toBeDefined();
      expect(entry?.error?.message).toBe('handler exploded');
    } finally {
      resetDefaultLogger();
    }
  });

  specTest(
    'refuses a server-stream request that carries a body before any event is written',
    {
      feature: 'typescript/server-streams',
      requirement: 'sse-admission-and-bounds',
      check: 'a-request-body-is-refused-before-any-event',
    },
    async () => {
      const def = buildStreamDefinition('server', async (ctx) => {
        ctx.send({ event: 'never' });
      });

      const handler = createSseHandler(def);
      // A GET stream has no declared body, so accepting one would discard what
      // the caller sent. The refusal is an HTTP status, not a terminal event.
      expect(() =>
        handler(
          mockSseContext({
            req: new Request('http://localhost/events', { method: 'POST', body: '{"drop":true}' }),
          }),
        ),
      ).toThrow('server stream requests must not carry a body');
    },
  );

  it('refuses a body declared only by its framing headers', async () => {
    const def = buildStreamDefinition('server', async (ctx) => {
      ctx.send({ event: 'never' });
    });

    const handler = createSseHandler(def);
    const headers = new Headers({ 'content-length': '12' });
    expect(() => handler(mockSseContext({ req: { headers } as unknown as Request, headers }))).toThrow(
      'server stream requests must not carry a body',
    );
  });

  specTest(
    'reaches the caller as a non-2xx before any event when the query does not validate',
    {
      feature: 'typescript/server-streams',
      requirement: 'sse-admission-and-bounds',
      check: 'an-invalid-query-reaches-the-caller-as-a-non-2xx',
    },
    async () => {
      const def = buildStreamDefinition(
        'server',
        async (ctx) => {
          ctx.send({ limit: 1 });
        },
        { query: { limit: Number } },
      );

      const handler = createSseHandler(def);
      // Admission fails while the response is still an ordinary HTTP result: no
      // stream is created, so no `data:` frame can precede the refusal.
      let thrown: unknown;
      try {
        handler(mockSseContext({ queryParams: () => ({ limit: 'not-a-number' }) }));
      } catch (error) {
        thrown = error;
      }
      expect(thrown).toBeDefined();
      expect((thrown as { status?: number }).status).toBeGreaterThanOrEqual(400);
      expect((thrown as { status?: number }).status).toBeLessThan(500);
    },
  );

  specTest(
    'bounds a single frame and names the producer',
    {
      feature: 'typescript/server-streams',
      requirement: 'sse-admission-and-bounds',
      check: 'the-declared-queue-depth-and-frame-size-bound-the-producer',
    },
    async () => {
      const def = buildStreamDefinition('server', async (ctx) => {
        ctx.send({ small: true });
        ctx.send({ big: 'x'.repeat(256) });
      });

      const handler = createSseHandler(def, { maxFrameBytes: 64 });
      const text = await ((await handler(mockSseContext())) as HttpResponse).get().text();

      expect(text).toContain('data: {"small":true}\n\n');
      expect(text).not.toContain('"big"');
      expect(text).toContain(
        'event: error\ndata: {"status":500,"code":"http.internal_server","error":"Internal Server Error","message":"Internal Server Error"}\n\n',
      );
    },
  );

  specTest(
    'writes liveness comments that carry no data while the handler produces nothing',
    {
      feature: 'typescript/server-streams',
      requirement: 'sse-admission-and-bounds',
      check: 'a-liveness-comment-carries-no-data',
    },
    async () => {
      let release: (() => void) | undefined;
      const quiet = new Promise<void>((resolve) => {
        release = resolve;
      });
      const def = buildStreamDefinition('server', async (ctx) => {
        await quiet;
        ctx.send({ late: true });
      });

      const handler = createSseHandler(def, { heartbeatMs: 5 });
      const response = ((await handler(mockSseContext())) as HttpResponse).get();
      const reader = response.body?.getReader();
      if (!reader) throw new Error('SSE response has no body');
      const decoder = new TextDecoder();
      let seen = '';
      while (!seen.includes(': heartbeat\n\n')) {
        const { done, value } = await reader.read();
        if (done) break;
        seen += decoder.decode(value, { stream: true });
      }
      // A comment is liveness only: no consumer ever sees it as a message.
      expect(seen).toContain(': heartbeat\n\n');
      expect(seen).not.toContain('data:');

      release?.();
      let rest = '';
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        rest += decoder.decode(value, { stream: true });
      }
      expect(rest).toContain('data: {"late":true}\n\n');
    },
  );

  it('ignores enqueue-after-close when send() is called past stream completion', async () => {
    let lateSend: ((data: unknown) => void) | undefined;
    const def = buildStreamDefinition('server', async (ctx) => {
      ctx.send({ first: true });
      lateSend = ctx.send; // capture to call after the stream closes
    });

    const handler = createSseHandler(def);
    const result = await handler(mockSseContext());
    const response = (result as HttpResponse).get();
    await response.text(); // drives start() to completion → controller.close()

    // The handler returned, the controller is closed; a late send must be a
    // no-op (swallowed enqueue-after-close catch) rather than throwing.
    expect(() => lateSend?.({ tooLate: true })).not.toThrow();
  });
});
