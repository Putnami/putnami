import { afterEach, describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { resetConfigLoader, Stream } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { api } from '../../../src/api/api.plugin';
import { endpoint } from '../../../src/api/route';
import { buildStreamDefinition } from '../../../src/api/route/stream-endpoint';
import { createSseHandler } from '../../../src/api/stream/sse-handler';
import {
  classifySseEvent,
  negotiatesSseWire,
  SSE_COMPLETE_FRAME,
  SSE_WIRE_HEADER,
  SSE_WIRE_V1,
  sseCursorValue,
  sseReopenQuery,
} from '../../../src/api/stream/sse-wire';
import { application } from '../../../src/application';
import type { HttpRequestContext } from '../../../src/http/http-context.type';
import { http, type HttpPlugin } from '../../../src/http/http.plugin';
import type { HttpResponse } from '../../../src/http/http-response';

/** protocols/clientcontract/fixtures/sse/wire.json: what a provider writes for each request. */
interface WireCorpus {
  readonly header: string;
  readonly token: string;
  readonly completeFrame: string;
  readonly negotiation: readonly { readonly lines: readonly string[]; readonly negotiates: boolean }[];
  readonly provider: readonly {
    readonly name: string;
    readonly routeDeclaresContinuation: boolean;
    readonly request: readonly string[];
    readonly admission: 'admitted' | 'refused';
    readonly handler: 'returns' | 'fails' | 'canceled' | 'never-runs';
    readonly expect: { readonly acknowledges: boolean; readonly terminal: 'complete' | 'error' | 'none' };
  }[];
}

const CORPUS = JSON.parse(
  readFileSync(join(import.meta.dir, '../../../../../../protocols/clientcontract/fixtures/sse/wire.json'), 'utf8'),
) as WireCorpus;

const FIRST = 'data: {"cursor":"c1","line":"one"}\n\n';
const SECOND = 'data: {"cursor":"c2","line":"two"}\n\n';
const INTERNAL_ERROR =
  'event: error\ndata: {"status":500,"code":"http.internal_server","error":"Internal Server Error","message":"Internal Server Error"}\n\n';

/** A request as the HTTP plugin hands it over, with the drain of the run serving it. */
function sseContext(lines: readonly string[] = [], drain?: AbortSignal): HttpRequestContext {
  const headers = new Headers({ accept: 'text/event-stream' });
  for (const line of lines) headers.append(SSE_WIRE_HEADER, line);
  return {
    req: new Request('http://localhost/logs', { headers }),
    headers,
    url: 'http://localhost/logs',
    params: {},
    queryParams: () => ({}),
    secured: () => false,
    host: () => 'localhost',
    domain: () => 'localhost',
    path: () => '/logs',
    query: () => '',
    __drain: drain,
  } as unknown as HttpRequestContext;
}

async function serve(
  handler: (ctx: { send: (value: unknown) => void; signal: AbortSignal }) => Promise<void>,
  lines: readonly string[],
  options: Parameters<typeof createSseHandler>[1] & { readonly drain?: AbortSignal } = { negotiatesWire: true },
): Promise<{ acknowledgment: string | null; body: string }> {
  const { drain, ...handlerOptions } = options;
  const sse = createSseHandler(buildStreamDefinition('server', handler), handlerOptions);
  const response = ((await sse(sseContext(lines, drain))) as HttpResponse).get();
  return { acknowledgment: response.headers.get(SSE_WIRE_HEADER), body: await response.text() };
}

const sendTwo = async (ctx: { send: (value: unknown) => void }) => {
  ctx.send({ cursor: 'c1', line: 'one' });
  ctx.send({ cursor: 'c2', line: 'two' });
};

describe('the negotiated SSE wire', () => {
  specTest(
    'reads the negotiation lines of the shared corpus the way the contract does',
    {
      feature: 'typescript/server-streams',
      requirement: 'negotiated-sse-wire',
      check: 'an-sse-request-that-does-not-negotiate-keeps-the-legacy-framing',
    },
    () => {
      expect([CORPUS.header, CORPUS.token, CORPUS.completeFrame]).toEqual([
        SSE_WIRE_HEADER,
        SSE_WIRE_V1,
        SSE_COMPLETE_FRAME,
      ]);
      for (const row of CORPUS.negotiation) {
        const headers = new Headers();
        for (const line of row.lines) headers.append(SSE_WIRE_HEADER, line);
        expect({ lines: row.lines, negotiates: negotiatesSseWire(headers.get(SSE_WIRE_HEADER)) }).toEqual({
          lines: row.lines,
          negotiates: row.negotiates,
        });
      }
    },
  );

  specTest(
    'acknowledges a negotiating request and ends its stream with the complete frame',
    {
      feature: 'typescript/server-streams',
      requirement: 'negotiated-sse-wire',
      check: 'a-negotiated-sse-stream-is-acknowledged-and-ends-with-complete',
    },
    async () => {
      expect(await serve(sendTwo, [SSE_WIRE_V1])).toEqual({
        acknowledgment: SSE_WIRE_V1,
        body: FIRST + SECOND + SSE_COMPLETE_FRAME,
      });
    },
  );

  specTest(
    'keeps the legacy framing for every request that does not negotiate',
    {
      feature: 'typescript/server-streams',
      requirement: 'negotiated-sse-wire',
      check: 'an-sse-request-that-does-not-negotiate-keeps-the-legacy-framing',
    },
    async () => {
      const legacy = { acknowledgment: null, body: FIRST + SECOND };
      expect(await serve(sendTwo, [])).toEqual(legacy);
      expect(await serve(sendTwo, ['putnami.sse.v2'])).toEqual(legacy);
      expect(await serve(sendTwo, [SSE_WIRE_V1], {})).toEqual(legacy);
    },
  );

  specTest(
    'never ends a failed stream, or one that dropped a message, with the complete frame',
    {
      feature: 'typescript/server-streams',
      requirement: 'negotiated-sse-wire',
      check: 'a-failed-or-canceled-negotiated-stream-never-ends-with-complete',
    },
    async () => {
      const failing = async (ctx: { send: (value: unknown) => void }) => {
        ctx.send({ cursor: 'c1', line: 'one' });
        throw new Error('the log store is gone');
      };
      expect(await serve(failing, [SSE_WIRE_V1])).toEqual({
        acknowledgment: SSE_WIRE_V1,
        body: FIRST + INTERNAL_ERROR,
      });
      // A handler that swallows the bound it hit returns normally, and the
      // message it could not queue never left: no success is claimed.
      const swallowing = async (ctx: { send: (value: unknown) => void }) => {
        ctx.send({ cursor: 'c1', line: 'one' });
        try {
          ctx.send({ cursor: 'c2', line: 'two' });
        } catch {
          // ignored on purpose
        }
      };
      expect(await serve(swallowing, [SSE_WIRE_V1], { negotiatesWire: true, maxBufferedMessages: 1 })).toEqual({
        acknowledgment: SSE_WIRE_V1,
        body: FIRST,
      });
    },
  );

  specTest(
    'ends a negotiated stream with no terminal when the provider drains, and keeps a legacy one',
    {
      feature: 'typescript/server-streams',
      requirement: 'negotiated-sse-wire',
      check: 'a-draining-provider-ends-a-negotiated-stream-without-a-terminal',
    },
    async () => {
      const drain = new AbortController();
      const draining = async (ctx: { send: (value: unknown) => void }) => {
        ctx.send({ cursor: 'c1', line: 'one' });
        drain.abort(); // the handler returns before it could observe the drain
      };
      expect(await serve(draining, [SSE_WIRE_V1], { negotiatesWire: true, drain: drain.signal })).toEqual({
        acknowledgment: SSE_WIRE_V1,
        body: FIRST,
      });
      const legacyDrain = new AbortController();
      const legacy = async (ctx: { send: (value: unknown) => void }) => {
        await sendTwo(ctx);
        legacyDrain.abort();
      };
      expect(await serve(legacy, [], { negotiatesWire: true, drain: legacyDrain.signal })).toEqual({
        acknowledgment: null,
        body: FIRST + SECOND,
      });
    },
  );

  specTest(
    'ends a stream the consumer canceled with no terminal',
    {
      feature: 'typescript/server-streams',
      requirement: 'negotiated-sse-wire',
      check: 'a-failed-or-canceled-negotiated-stream-never-ends-with-complete',
    },
    async () => {
      let returned: () => void = () => undefined;
      const finished = new Promise<void>((resolve) => {
        returned = resolve;
      });
      const sse = createSseHandler(
        buildStreamDefinition('server', async (ctx) => {
          ctx.send({ cursor: 'c1', line: 'one' });
          await new Promise((resolve) => ctx.signal.addEventListener('abort', resolve, { once: true }));
          returned();
        }),
        { negotiatesWire: true },
      );
      const response = ((await sse(sseContext([SSE_WIRE_V1]))) as HttpResponse).get();
      expect(response.headers.get(SSE_WIRE_HEADER)).toBe(SSE_WIRE_V1);
      const reader = response.body?.getReader();
      if (!reader) throw new Error('SSE response has no body');
      const first = await reader.read();
      expect(new TextDecoder().decode(first.value)).toBe(FIRST);
      await reader.cancel('consumer left');
      // The handler observed the cancellation and returned normally; the
      // provider claims no success for a stream nobody finished reading.
      await finished;
      const after = await reader.read().catch(() => ({ done: true, value: undefined }));
      expect(after.done).toBe(true);
    },
  );
});

const running: (() => Promise<void>)[] = [];

afterEach(async () => {
  for (const stop of running.splice(0)) await stop();
  resetConfigLoader();
});

/** A real provider: one stream per handler outcome, with and without a declared continuation. */
async function startLogProvider(): Promise<string> {
  const providerHttp: HttpPlugin = http({ port: 0 });
  const providerApi = api({
    autoScan: false,
    client: { service: { id: 'logs', audience: 'api://logs' }, credentials: {} },
  });
  const handlers = {
    returns: sendTwo,
    drains: async (ctx: { send: (value: unknown) => void; signal: AbortSignal }) => {
      ctx.send({ cursor: 'c1', line: 'one' });
      await new Promise((resolve) => ctx.signal.addEventListener('abort', resolve, { once: true }));
    },
    fails: async (ctx: { send: (value: unknown) => void }) => {
      ctx.send({ cursor: 'c1', line: 'one' });
      throw new Error('the log store is gone');
    },
  };
  for (const [outcome, handle] of Object.entries(handlers)) {
    for (const declared of [true, false]) {
      providerApi.register(
        `/${declared ? 'continued' : 'plain'}/${outcome}`,
        endpoint()
          .query({ run: String })
          .returns(Stream({ cursor: String, line: String }))
          .client(declared ? { sseContinuation: { mode: 'best-effort' } } : {})
          .handle(handle),
        'GET',
      );
    }
  }
  const app = application().use(providerHttp).use(providerApi);
  await app.start();
  running.push(async () => {
    await app.stop();
  });
  return `http://localhost:${providerHttp.getServer()?.port}`;
}

describe('a real provider replaying the shared wire corpus', () => {
  specTest(
    'writes what the corpus pins for every request it can observe',
    {
      feature: 'typescript/server-streams',
      requirement: 'negotiated-sse-wire',
      check: 'a-negotiated-sse-stream-is-acknowledged-and-ends-with-complete',
    },
    async () => {
      const base = await startLogProvider();
      // A cancellation is observed in-process above: nothing reaches a consumer that left.
      for (const row of CORPUS.provider.filter((entry) => entry.handler !== 'canceled')) {
        const outcome = row.handler === 'never-runs' ? 'returns' : row.handler;
        const route = `${base}/${row.routeDeclaresContinuation ? 'continued' : 'plain'}/${outcome}`;
        const headers = new Headers({ accept: 'text/event-stream' });
        for (const line of row.request) headers.append(SSE_WIRE_HEADER, line);
        // biome-ignore lint/performance/noAwaitInLoops: rows are independent requests read in order
        const response = await fetch(row.admission === 'refused' ? route : `${route}?run=r1`, { headers });
        const body = await response.text();
        expect({ row: row.name, acknowledges: negotiatesSseWire(response.headers.get(SSE_WIRE_HEADER)) }).toEqual({
          row: row.name,
          acknowledges: row.expect.acknowledges,
        });
        if (row.admission === 'refused') {
          expect(response.status).toBeGreaterThanOrEqual(400);
          expect(response.status).toBeLessThan(500);
          continue;
        }
        expect(response.status).toBe(200);
        const expected = outcome === 'returns' ? FIRST + SECOND : FIRST;
        expect(body.startsWith(expected)).toBe(true);
        expect(body.endsWith(SSE_COMPLETE_FRAME)).toBe(row.expect.terminal === 'complete');
        expect(body.includes('event: error\n')).toBe(row.expect.terminal === 'error');
        if (row.expect.terminal === 'none') expect(body).toBe(expected);
      }
    },
  );

  specTest(
    'ends a negotiated stream with no terminal when the application stops',
    {
      feature: 'typescript/server-streams',
      requirement: 'negotiated-sse-wire',
      check: 'a-draining-provider-ends-a-negotiated-stream-without-a-terminal',
    },
    async () => {
      const base = await startLogProvider();
      const stop = running.pop();
      if (!stop) throw new Error('the provider registered no stop');
      const response = await fetch(`${base}/continued/drains?run=r1`, {
        headers: { accept: 'text/event-stream', [SSE_WIRE_HEADER]: SSE_WIRE_V1 },
      });
      expect(response.headers.get(SSE_WIRE_HEADER)).toBe(SSE_WIRE_V1);
      const reader = response.body?.getReader();
      if (!reader) throw new Error('SSE response has no body');
      const decoder = new TextDecoder();
      let seen = '';
      while (!seen.includes(FIRST)) {
        const { done, value } = await reader.read();
        if (done) break;
        seen += decoder.decode(value, { stream: true });
      }
      expect(seen).toBe(FIRST);
      const stopping = stop();
      let rest = '';
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        rest += decoder.decode(value, { stream: true });
      }
      await stopping;
      // The response ended with no terminal: the consumer reads an interruption.
      expect(rest).toBe('');
    },
  );

  specTest(
    'ends a negotiated stream of a loaded route folder with no terminal when the application stops',
    {
      feature: 'typescript/server-streams',
      requirement: 'negotiated-sse-wire',
      check: 'a-draining-provider-ends-a-negotiated-stream-without-a-terminal',
    },
    async () => {
      // A scanned route folder is loaded as its own ApiPlugin, merged into the
      // root one at warmup, and no lifecycle hook of its own ever runs. Its
      // negotiated streams must still drain when the application stops —
      // on the drain of the HTTP plugin serving them — rather than wait for
      // the server to force the connection closed.
      const client = { service: { id: 'logs', audience: 'api://logs' }, credentials: {} };
      const loaded = api({ autoScan: false, client });
      let observed: 'drained' | 'forced' | undefined;
      loaded.register(
        '/continued/drains',
        endpoint()
          .query({ run: String })
          .returns(Stream({ cursor: String, line: String }))
          .client({ sseContinuation: { mode: 'best-effort' } })
          .handle(async (ctx) => {
            ctx.send({ cursor: 'c1', line: 'one' });
            await new Promise<void>((resolve) => ctx.signal.addEventListener('abort', () => resolve(), { once: true }));
            observed = observed ?? 'drained';
          }),
        'GET',
      );
      const providerHttp: HttpPlugin = http({ port: 0 });
      const root = api({ autoScan: false, client, preloadedModule: { loaded } });
      const app = application().use(providerHttp).use(root);
      await app.start();
      const response = await fetch(`http://localhost:${providerHttp.getServer()?.port}/continued/drains?run=r1`, {
        headers: { accept: 'text/event-stream', [SSE_WIRE_HEADER]: SSE_WIRE_V1 },
      });
      expect(response.headers.get(SSE_WIRE_HEADER)).toBe(SSE_WIRE_V1);
      const reader = response.body?.getReader();
      if (!reader) throw new Error('SSE response has no body');
      const decoder = new TextDecoder();
      let seen = '';
      while (!seen.includes(FIRST)) {
        const { done, value } = await reader.read();
        if (done) break;
        seen += decoder.decode(value, { stream: true });
      }
      expect(seen).toBe(FIRST);
      const startedStopping = Date.now();
      const stopping = app.stop();
      let rest = '';
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        rest += decoder.decode(value, { stream: true });
      }
      await stopping;
      // The handler observed the drain and the response ended with no
      // terminal, well before the server's forced-close deadline.
      expect(rest).toBe('');
      expect(observed).toBe('drained');
      expect(Date.now() - startedStopping).toBeLessThan(2000);
    },
  );

  specTest(
    'drains only the instance that stops when two instances share a loaded route folder',
    {
      feature: 'typescript/server-streams',
      requirement: 'negotiated-sse-wire',
      check: 'a-draining-provider-ends-a-negotiated-stream-without-a-terminal',
    },
    async () => {
      // A route folder is loaded once per process. Two application instances
      // that scan it — two replicas in one test process, a provider and its
      // own consumer — serve the same loaded plugin; stopping one must drain
      // its own streams and nothing of the other's.
      const client = { service: { id: 'logs', audience: 'api://logs' }, credentials: {} };
      const loaded = api({ autoScan: false, client });
      loaded.register(
        '/continued/drains',
        endpoint()
          .query({ run: String })
          .returns(Stream({ cursor: String, line: String }))
          .client({ sseContinuation: { mode: 'best-effort' } })
          .handle(async (ctx) => {
            ctx.send({ cursor: 'c1', line: 'one' });
            await new Promise<void>((resolve) => ctx.signal.addEventListener('abort', () => resolve(), { once: true }));
          }),
        'GET',
      );
      const instances = await Promise.all(
        [1, 2].map(async () => {
          const providerHttp: HttpPlugin = http({ port: 0 });
          const app = application()
            .use(providerHttp)
            .use(api({ autoScan: false, client, preloadedModule: { loaded } }));
          await app.start();
          return { app, port: providerHttp.getServer()?.port };
        }),
      );
      const open = async (port: number | undefined) => {
        const response = await fetch(`http://localhost:${port}/continued/drains?run=r1`, {
          headers: { accept: 'text/event-stream', [SSE_WIRE_HEADER]: SSE_WIRE_V1 },
        });
        expect(response.headers.get(SSE_WIRE_HEADER)).toBe(SSE_WIRE_V1);
        const reader = response.body?.getReader();
        if (!reader) throw new Error('SSE response has no body');
        const first = await reader.read();
        expect(new TextDecoder().decode(first.value)).toBe(FIRST);
        return reader;
      };
      const [onFirst, onSecond] = await Promise.all([open(instances[0]?.port), open(instances[1]?.port)]);
      try {
        await instances[0]?.app.stop();
        // The first instance's stream ended with no terminal.
        expect((await onFirst.read()).done).toBe(true);
        // The second instance's stream is still open: nothing arrived on it.
        const untouched = await Promise.race([
          onSecond.read().then((chunk) => (chunk.done ? 'ended' : 'received')),
          Bun.sleep(100).then(() => 'open'),
        ]);
        expect(untouched).toBe('open');
      } finally {
        await instances[1]?.app.stop();
      }
      expect((await onSecond.read()).done).toBe(true);
    },
  );

  it('refuses an sse continuation on a provider-owned wire', () => {
    expect(() =>
      endpoint()
        .subprotocol('vendor.v1')
        .body(Stream({ line: String }))
        .returns(Stream({ line: String }))
        .client({ sseContinuation: { mode: 'best-effort' } })
        .handle(async () => undefined),
    ).toThrow('carries no SSE');
  });
});

describe('the reader helpers both runtimes share', () => {
  specTest(
    'classify the negotiated vocabulary, read a position verbatim and reopen without synthesizing one',
    {
      feature: 'typescript/server-streams',
      requirement: 'negotiated-sse-wire',
      check: 'an-sse-request-that-does-not-negotiate-keeps-the-legacy-framing',
    },
    () => {
      // Mirrors ClassifySSEEvent, SSECursorValue and SSEReopenQuery of protocols/clientcontract.
      expect(classifySseEvent('', '{}', true)).toBe('message');
      expect(classifySseEvent('message', '{}', true)).toBe('message');
      expect(classifySseEvent('error', '{}', true)).toBe('error');
      expect(classifySseEvent('complete', '{}', true)).toBe('complete');
      expect(() => classifySseEvent('complete', '{"done":true}', true)).toThrow(/complete/);
      expect(() => classifySseEvent('progress', '{}', true)).toThrow(/progress/);
      expect(classifySseEvent('progress', '{}', false)).toBe('message');
      expect(classifySseEvent('error', '{}', false)).toBe('error');
      for (const message of [[], null, { cursor: null }, { cursor: ['c1'] }, { line: 'a' }, 'text', { cursor: '' }]) {
        expect(() => sseCursorValue(message, 'cursor')).toThrow();
      }
      expect(sseCursorValue({ cursor: 'opaqueé/+=', line: 'a' }, 'cursor')).toBe('opaqueé/+=');
      const cursor = { mode: 'cursor', cursor: { queryParameter: 'cursor' } } as const;
      const original = { selector: 'svc=api', cursor: ['c0', 'c00'] };
      const reopened = sseReopenQuery(cursor, original, 'c7');
      expect(reopened).toEqual({ selector: 'svc=api', cursor: 'c7' });
      expect(original).toEqual({ selector: 'svc=api', cursor: ['c0', 'c00'] });
      expect(sseReopenQuery(cursor, original, '')).toEqual({ selector: 'svc=api', cursor: ['c0', 'c00'] });
      expect(sseReopenQuery(cursor, undefined, '')).toEqual({});
      expect(sseReopenQuery({ mode: 'best-effort' }, { selector: 'a' }, 'c7')).toEqual({ selector: 'a' });
    },
  );
});

describe('a provider that stops and starts again', () => {
  specTest(
    'serves negotiated streams again: the drain of the previous run does not end them',
    {
      feature: 'typescript/server-streams',
      requirement: 'negotiated-sse-wire',
      check: 'a-restarted-provider-serves-negotiated-streams-again',
    },
    async () => {
      const providerHttp: HttpPlugin = http({ port: 0 });
      const providerApi = api({
        autoScan: false,
        client: { service: { id: 'logs', audience: 'api://logs' }, credentials: {} },
      });
      providerApi.register(
        '/continued/returns',
        endpoint()
          .query({ run: String })
          .returns(Stream({ cursor: String, line: String }))
          .client({ sseContinuation: { mode: 'best-effort' } })
          .handle(sendTwo),
        'GET',
      );
      const app = application().use(providerHttp).use(providerApi);
      const read = async (): Promise<string> => {
        const response = await fetch(`http://localhost:${providerHttp.getServer()?.port}/continued/returns?run=r1`, {
          headers: { accept: 'text/event-stream', [SSE_WIRE_HEADER]: SSE_WIRE_V1 },
        });
        expect(response.headers.get(SSE_WIRE_HEADER)).toBe(SSE_WIRE_V1);
        return response.text();
      };
      await app.start();
      try {
        expect(await read()).toBe(FIRST + SECOND + SSE_COMPLETE_FRAME);
        await app.stop();
        await app.start();
        // The previous run's drain is spent; this run's streams complete.
        expect(await read()).toBe(FIRST + SECOND + SSE_COMPLETE_FRAME);
      } finally {
        await app.stop();
      }
    },
  );
});
