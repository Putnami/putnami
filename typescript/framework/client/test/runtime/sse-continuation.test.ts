import { afterEach, describe, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import type { ClientContractOperation, ClientSchema, ClientSseContinuation } from '@putnami/application';
import {
  negotiatesSseWire,
  SSE_COMPLETE_FRAME,
  SSE_WIRE_HEADER,
  SSE_WIRE_V1,
  setCollector,
  TelemetryCollector,
} from '@putnami/application';
import { BaseClient } from '../../src/runtime/base-client';
import {
  ClientCanceledError,
  ClientDeadlineError,
  ClientError,
  ClientFrameworkError,
  ClientResponseContractError,
} from '../../src/runtime/errors';
import { MAX_STREAM_RESUME_ATTEMPTS } from '../../src/runtime/service-ws-transport';
import { SseDelivery } from '../../src/runtime/sse-continuation';
import { StreamSession } from '../../src/runtime/stream-session';
import type { StreamObserver } from '../../src/runtime/stream.type';

const FEATURE = 'typescript/service-clients';
const REQUIREMENT = 'declared-sse-continuation';
const CORPUS_PATH = join(import.meta.dir, '../../../../../protocols/clientcontract/fixtures/sse/scenes.json');

/** protocols/clientcontract/fixtures/sse/scenes.json: the reader outcomes both runtimes replay (ADR 0013). */
interface SceneCorpus {
  readonly maxFrameBytes: number;
  readonly continuations: Record<string, ClientSseContinuation>;
  readonly scenes: readonly Scene[];
}

interface Scene {
  readonly name: string;
  readonly continuation: string;
  readonly requested: boolean;
  readonly acknowledgment: readonly string[];
  readonly query: Record<string, readonly string[]>;
  readonly maxFrameBytes?: number;
  readonly body: readonly string[];
  readonly end: 'close' | 'reset';
  readonly expect: {
    readonly outcome: 'complete' | 'error' | 'interrupted' | 'contract-error' | 'transport-error';
    readonly messages: readonly Record<string, unknown>[];
    readonly deliveredCursor: string | null;
    readonly error?: { readonly status: number; readonly code: string };
    readonly reopen?: { readonly query: Record<string, readonly string[]> };
  };
}

const CORPUS = JSON.parse(readFileSync(CORPUS_PATH, 'utf8')) as SceneCorpus;

const output = {
  type: 'object',
  properties: { cursor: { type: 'string' }, line: { type: 'string' }, service: { type: 'string' } },
  additionalProperties: false,
} as const satisfies ClientSchema;

type LogEntry = Record<string, unknown>;

/** What each connection of one session asked for. */
interface Seen {
  readonly query: Record<string, string[]>;
  readonly wire: string | null;
  readonly authorization: string | null;
}

function seen(request: Request): Seen {
  const url = new URL(request.url);
  const query: Record<string, string[]> = {};
  for (const [key, value] of url.searchParams) {
    const values = query[key] ?? [];
    values.push(value);
    query[key] = values;
  }
  return {
    query,
    wire: request.headers.get(SSE_WIRE_HEADER),
    authorization: request.headers.get('authorization'),
  };
}

const servers: (() => void)[] = [];
let collector: TelemetryCollector;

afterEach(() => {
  setCollector(undefined);
  for (const stop of servers.splice(0)) stop();
});

/**
 * The operation the scenes describe: a safe server stream of log lines. An
 * undefined continuation is the operation an old runtime was generated for.
 */
function logOperation(
  continuation: ClientSseContinuation | undefined,
  maxFrameBytes: number,
  reconnect: boolean,
  security: ClientContractOperation['security'] = { alternatives: [{ allOf: [] }] },
): ClientContractOperation {
  const required = continuation?.mode === 'cursor' ? ['cursor', 'line'] : ['line'];
  return {
    stream: 'server',
    messages: { output: { ...output, required } },
    transports: [
      {
        protocol: 'sse',
        path: '/logs/tail',
        encoding: 'json',
        ...(continuation ? { sse: { continuation } } : {}),
      },
    ],
    security,
    errors: [
      { status: 400, code: 'http.bad_request' },
      { status: 403, code: 'forbidden' },
      { status: 404, code: 'not_found' },
    ],
    idempotency: { kind: 'safe' },
    resilience: { stream: { maxFrameBytes, maxBufferedMessages: 8, ...(continuation ? { reconnect } : {}) } },
  };
}

class LogsClient extends BaseClient {
  readonly serviceName = 'logs';

  tail(query: Record<string, readonly string[]>, signal?: AbortSignal): StreamObserver<LogEntry> {
    return this.serviceStream<LogEntry>('GET', '/logs/tail', { operationId: 'tailLogs', query, signal });
  }
}

function logsClient(url: string, operation: ClientContractOperation, authorization?: () => string): LogsClient {
  return new LogsClient({
    baseUrl: url,
    transport: 'http',
    serviceId: 'logs',
    operationContracts: { tailLogs: operation },
    streamInterceptors: [
      async (request, next) => {
        request.headers.set('X-Client-Id', 'fixtures-consumer');
        if (authorization) request.headers.set('Authorization', authorization());
        return next(request);
      },
    ],
  });
}

interface Run {
  readonly messages: LogEntry[];
  readonly failure?: Error;
  readonly requests: Seen[];
}

/**
 * Serve the scene's first connection over real HTTP and read it through the
 * real runtime, at the observer boundary. A reopening is answered with an
 * acknowledged `complete`, so the query it asked for is observable and the
 * session ends. An interrupted or reset scene holds the break until the
 * caller has received every message the scene expects.
 */
async function replayScene(scene: Scene, reconnect: boolean): Promise<Run> {
  const continuation = scene.requested ? CORPUS.continuations[scene.continuation] : undefined;
  const maxFrameBytes = scene.maxFrameBytes ?? CORPUS.maxFrameBytes;
  const requests: Seen[] = [];
  let releaseBreak: () => void = () => undefined;
  const release = new Promise<void>((resolve) => {
    releaseBreak = resolve;
  });
  // A reset discards the chunks the client has not read yet, so a scene that
  // ends with one holds it, like an interrupted scene, until the caller has
  // received every message the scene expects.
  const holdBreak = scene.expect.outcome === 'interrupted' || scene.end === 'reset';
  const server = Bun.serve({
    port: 0,
    fetch(request) {
      requests.push(seen(request));
      const headers = new Headers({ 'Content-Type': 'text/event-stream' });
      if (requests.length > 1) {
        headers.set(SSE_WIRE_HEADER, SSE_WIRE_V1);
        return new Response(SSE_COMPLETE_FRAME, { headers });
      }
      for (const line of scene.acknowledgment) headers.append(SSE_WIRE_HEADER, line);
      const encoder = new TextEncoder();
      return new Response(
        new ReadableStream({
          async start(controller) {
            for (const chunk of scene.body) {
              controller.enqueue(encoder.encode(chunk));
              await Bun.sleep(1);
            }
            if (holdBreak) await release;
            if (scene.end === 'reset') controller.error(new Error('connection reset'));
            else controller.close();
          },
        }),
        { headers },
      );
    },
  });
  servers.push(() => server.stop(true));
  const client = logsClient(`http://localhost:${server.port}`, logOperation(continuation, maxFrameBytes, reconnect));
  const stream = client.tail(scene.query);
  const messages: LogEntry[] = [];
  const expected = scene.expect.messages.length;
  if (expected === 0) releaseBreak();
  const outcome = await new Promise<Error | undefined>((resolve) => {
    stream.onMessage((entry) => {
      messages.push(entry);
      if (messages.length === expected) releaseBreak();
    });
    stream.onError((error) => resolve(error));
    stream.onComplete(() => resolve(undefined));
  });
  client.dispose();
  return { messages, failure: outcome, requests };
}

function assertScene(scene: Scene, reconnect: boolean, run: Run): void {
  const label = `${scene.name} (reconnect=${reconnect})`;
  expect(run.requests.length, label).toBeGreaterThan(0);
  const first = run.requests[0] as Seen;
  expect(negotiatesSseWire(first.wire), `${label}: the runtime asked for the negotiated wire`).toBe(scene.requested);
  expect(run.messages, label).toEqual(scene.expect.messages);
  if (scene.requested && scene.continuation === 'cursor') {
    const last = run.messages.at(-1);
    expect(last === undefined ? null : last['cursor'], `${label}: delivered cursor`).toBe(scene.expect.deliveredCursor);
  }
  switch (scene.expect.outcome) {
    case 'complete':
      expect(run.failure, label).toBeUndefined();
      break;
    case 'error':
      expect(run.failure, label).toBeInstanceOf(ClientFrameworkError);
      expect((run.failure as ClientFrameworkError).status, label).toBe(scene.expect.error?.status as number);
      expect((run.failure as ClientFrameworkError).code, label).toBe(scene.expect.error?.code as string);
      break;
    case 'contract-error':
      expect(run.failure, label).toBeInstanceOf(ClientError);
      expect((run.failure as ClientError).code, label).toBe('client.response');
      break;
    case 'transport-error':
      // Legacy framing only: the reader that predates ADR 0013 keeps its
      // reading of a broken socket, the opaque remote failure.
      expect(run.failure, label).toBeInstanceOf(ClientError);
      expect(['client.response', 'client.remote'], label).toContain((run.failure as ClientError).code);
      break;
    case 'interrupted': {
      if (!reconnect) {
        expect(run.failure, label).toBeInstanceOf(ClientResponseContractError);
        expect((run.failure as Error).message, label).toContain('interrupted');
        break;
      }
      expect(run.failure, label).toBeUndefined();
      expect(run.requests.length, `${label}: the first connection and one reopening`).toBe(2);
      const reopened = run.requests[1] as Seen;
      expect(reopened.query, `${label}: reopened query`).toEqual(scene.expect.reopen?.query ?? {});
      expect(negotiatesSseWire(reopened.wire), `${label}: the reopening asked for the negotiated wire`).toBe(true);
      return;
    }
  }
  expect(run.requests.length, `${label}: only an interruption reopens`).toBe(1);
}

describe('the shared SSE scenes replay against the real runtime', () => {
  const outcomes = new Set<string>();
  for (const scene of CORPUS.scenes) {
    specTest(
      scene.name,
      { feature: FEATURE, requirement: REQUIREMENT, check: 'the-shared-sse-scenes-replay-against-the-real-runtime' },
      async () => {
        outcomes.add(scene.expect.outcome);
        const reconnects = scene.expect.outcome === 'interrupted' ? [true, false] : [false];
        for (const reconnect of reconnects) {
          // biome-ignore lint/performance/noAwaitInLoops: each replay owns its provider
          assertScene(scene, reconnect, await replayScene(scene, reconnect));
        }
      },
    );
  }
  specTest(
    'reaches every outcome the corpus pins',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'the-shared-sse-scenes-replay-against-the-real-runtime' },
    () => {
      for (const outcome of ['complete', 'error', 'interrupted', 'contract-error', 'transport-error']) {
        expect(outcomes.has(outcome), `no scene reaches the ${outcome} outcome`).toBe(true);
      }
    },
  );
});

function session(capacity: number): StreamSession<string> {
  return new StreamSession<string>({
    serviceId: 'logs',
    operationId: 'tailLogs',
    protocol: 'sse',
    budgets: {
      handshakeMs: 0,
      idleMs: 0,
      sessionMs: 0,
      heartbeatMs: 0,
      maxFrameBytes: 4096,
      maxBufferedMessages: capacity,
    },
  });
}

describe('the delivery bridge', () => {
  specTest(
    'advances the position only on a value the subscribed caller received, and a reset drops the rest',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'values-queued-but-not-received-never-advance-the-position-nor-arrive-twice',
    },
    async () => {
      const owner = session(3);
      const delivery = new SseDelivery<string>(owner, 3);
      const settled: string[] = [];
      const enqueue = (value: string) =>
        delivery.enqueue({ value, position: `c-${value}` }).then(() => settled.push(value));
      expect(delivery.reset()).toBe('');
      // Three values fill the bound; the fourth waits for room.
      void enqueue('1');
      void enqueue('2');
      void enqueue('3');
      void enqueue('4');
      await Bun.sleep(5);
      expect(settled).toEqual(['1', '2']);
      expect(delivery.pending).toBe(4);
      expect(delivery.position).toBe('');
      // The caller subscribes: every queued value is handed over, in order,
      // and the position is the last one they took.
      const received: string[] = [];
      owner.observer().onMessage((value) => received.push(value));
      await Bun.sleep(5);
      expect(received).toEqual(['1', '2', '3', '4']);
      expect(settled).toEqual(['1', '2', '3', '4']);
      expect(delivery.position).toBe('c-4');
      expect(delivery.reset()).toBe('c-4');
      // A subscribed caller receives at once; nothing is retained.
      await enqueue('5');
      expect(received).toEqual(['1', '2', '3', '4', '5']);
      expect(delivery.position).toBe('c-5');

      // An unsubscribed caller: queued values are decoded, not received. A
      // reset drops them and reports the last position they did receive.
      const other = session(4);
      const bridge = new SseDelivery<string>(other, 4);
      await bridge.enqueue({ value: 'a', position: 'c-a' });
      await bridge.enqueue({ value: 'b', position: 'c-b' });
      expect(bridge.position).toBe('');
      expect(bridge.reset()).toBe('');
      expect(bridge.pending).toBe(0);
      const taken: string[] = [];
      other.observer().onMessage((value) => taken.push(value));
      expect(taken).toEqual([]);
      await bridge.enqueue({ value: 'c', position: 'c-c' });
      expect(taken).toEqual(['c']);
      expect(bridge.reset()).toBe('c-c');
      // finish hands over what is still queued, before the terminal.
      other.observer().onMessage(() => undefined);
      await bridge.enqueue({ value: 'd', position: 'c-d' });
      bridge.finish();
      owner.close();
      other.close();
    },
  );

  specTest(
    'releases a reader held on a full queue when the session ends',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'closing-a-reopening-stream-releases-its-connection-and-emits-one-measurement',
    },
    async () => {
      const owner = session(1);
      const delivery = new SseDelivery<string>(owner, 1);
      let released = false;
      const held = delivery.enqueue({ value: '1', position: 'c1' }).then(() => {
        released = true;
      });
      await Bun.sleep(5);
      expect(released).toBe(false);
      owner.close();
      await held;
      expect(released).toBe(true);
    },
  );
});

// ---------------------------------------------------------------------------
// Two real provider instances over a durable log
// ---------------------------------------------------------------------------

/** The event log two provider instances share, and the only thing they share. */
class DurableLog {
  readonly lines: string[] = [];
  closed = false;
  floor = 0;
  private waiters: (() => void)[] = [];

  append(...lines: string[]): void {
    this.lines.push(...lines);
    this.wake();
  }

  end(): void {
    this.closed = true;
    this.wake();
  }

  /** Resolve a cursor the provider issued: "c<n>" is the position after entry n. */
  position(cursor: string): number | undefined {
    if (cursor === '') return 0;
    const index = Number(cursor.slice(1));
    if (
      !cursor.startsWith('c') ||
      !Number.isInteger(index) ||
      index < 1 ||
      index > this.lines.length ||
      index < this.floor
    )
      return undefined;
    return index;
  }

  async next(after: number, signal: AbortSignal): Promise<{ cursor: string; line: string } | 'end' | 'aborted'> {
    for (;;) {
      if (after < this.lines.length) return { cursor: `c${after + 1}`, line: this.lines[after] as string };
      if (this.closed) return 'end';
      if (signal.aborted) return 'aborted';
      // biome-ignore lint/performance/noAwaitInLoops: the log is polled until it changes
      await new Promise<void>((resolve) => {
        this.waiters.push(resolve);
        signal.addEventListener('abort', () => resolve(), { once: true });
      });
    }
  }

  private wake(): void {
    for (const waiter of this.waiters.splice(0)) waiter();
  }
}

interface Instance {
  readonly requests: Seen[];
  sent: number;
  drain(): void;
  handle(request: Request): Response;
}

/**
 * One provider instance: the negotiated wire as the Go and TS providers
 * write it. A cursor-mode instance continues exclusively after the cursor it
 * is given; a live one tails the log from the moment the connection opens.
 * Draining it ends every negotiated stream with no terminal.
 */
function instance(log: DurableLog, live: boolean): Instance {
  const drain = new AbortController();
  const served: Instance = {
    requests: [],
    sent: 0,
    drain: () => drain.abort(),
    handle(request) {
      const url = new URL(request.url);
      const after = log.lines.length;
      const position = log.position(url.searchParams.get('cursor') ?? '');
      served.requests.push(seen(request));
      const headers = new Headers({ 'Content-Type': 'text/event-stream' });
      if (!negotiatesSseWire(request.headers.get(SSE_WIRE_HEADER))) return new Response('legacy', { headers });
      if (!live && position === undefined) {
        return Response.json(
          {
            status: 400,
            code: 'http.bad_request',
            error: 'Bad Request',
            message: 'the position is not in the retained log',
          },
          { status: 400 },
        );
      }
      headers.set(SSE_WIRE_HEADER, SSE_WIRE_V1);
      const encoder = new TextEncoder();
      const stop = new AbortController();
      request.signal.addEventListener('abort', () => stop.abort(), { once: true });
      drain.signal.addEventListener('abort', () => stop.abort(), { once: true });
      return new Response(
        new ReadableStream({
          async start(controller) {
            let cursor = live ? after : (position as number);
            for (;;) {
              // biome-ignore lint/performance/noAwaitInLoops: entries are written in order
              const entry = await log.next(cursor, stop.signal);
              if (entry === 'aborted') break;
              if (entry === 'end') {
                controller.enqueue(encoder.encode(SSE_COMPLETE_FRAME));
                break;
              }
              controller.enqueue(encoder.encode(`data: ${JSON.stringify(entry)}\n\n`));
              served.sent += 1;
              cursor += 1;
            }
            try {
              controller.close();
            } catch {
              // already closed
            }
          },
        }),
        { headers },
      );
    },
  };
  return served;
}

/** Routes every connection to the current instance, the way a load balancer does: the one double. */
function front(first: Instance): { url: string; current: Instance } {
  const router = { url: '', current: first };
  const server = Bun.serve({ port: 0, fetch: (request) => router.current.handle(request) });
  servers.push(() => server.stop(true));
  router.url = `http://localhost:${server.port}`;
  return router;
}

async function eventually(what: string, condition: () => boolean): Promise<void> {
  const deadline = Date.now() + 5000;
  while (!condition()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    // biome-ignore lint/performance/noAwaitInLoops: polling
    await Bun.sleep(2);
  }
}

const CURSOR: ClientSseContinuation = { mode: 'cursor', cursor: { outputField: 'cursor', queryParameter: 'cursor' } };
const BEST_EFFORT: ClientSseContinuation = { mode: 'best-effort' };

function calls() {
  return collector
    .drainAll()
    .flatMap((bucket) => bucket.counterSeries ?? [])
    .filter((series) => series.name === 'rpc.client.calls');
}

describe('a declared SSE continuation over real provider instances', () => {
  specTest(
    'continues on another instance after the last position the caller received, with a renewed credential',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'a-cursor-stream-continues-on-another-instance-after-the-last-position-the-caller-received',
    },
    async () => {
      collector = new TelemetryCollector();
      setCollector(collector);
      const log = new DurableLog();
      const first = instance(log, false);
      const second = instance(log, false);
      const third = instance(log, false);
      const router = front(first);
      let issued = 0;
      const client = logsClient(router.url, logOperation(CURSOR, 4096, true), () => `Bearer token-${++issued}`);
      const received: string[] = [];
      const stream = client.tail({ selector: ['svc=api'] });
      const ended = new Promise<Error | undefined>((resolve) => {
        stream.onError(resolve);
        stream.onComplete(() => resolve(undefined));
      });
      // A paused consumer is one who has not subscribed: what the first
      // instance writes is decoded and queued, never received.
      await eventually('the first connection', () => first.requests.length === 1);
      log.append('one', 'two', 'three', 'four', 'five');
      await eventually('the first instance to write five entries', () => first.sent === 5);
      router.current = second;
      first.drain();
      // Nothing was received, so the reopening keeps the original selector and
      // the queued values are dropped: the second instance sends them again.
      await eventually('the continuation to reach the second instance', () => second.requests.length === 1);
      await eventually('the second instance to write five entries', () => second.sent === 5);
      expect(received).toEqual([]);
      stream.onMessage((entry) => received.push(`${entry['cursor']}=${entry['line']}`));
      await eventually('the five entries to reach the subscribed caller', () => received.length >= 5);
      await Bun.sleep(20);
      // The first instance's copies were dropped, not retained: each arrives once.
      expect(received).toEqual(['c1=one', 'c2=two', 'c3=three', 'c4=four', 'c5=five']);
      // Every value was received, so the next reopening continues after c5.
      router.current = third;
      second.drain();
      await eventually('the continuation to reach the third instance', () => third.requests.length === 1);
      log.append('six');
      log.end();
      expect(await ended).toBeUndefined();
      expect(received).toEqual(['c1=one', 'c2=two', 'c3=three', 'c4=four', 'c5=five', 'c6=six']);
      expect(first.requests.map((request) => request.query)).toEqual([{ selector: ['svc=api'] }]);
      expect(second.requests.map((request) => request.query)).toEqual([{ selector: ['svc=api'] }]);
      expect(third.requests.map((request) => request.query)).toEqual([{ selector: ['svc=api'], cursor: ['c5'] }]);
      // Each opening carries a credential resolved when it dials.
      const authorizations = [first, second, third].map((served) => served.requests[0]?.authorization);
      expect(authorizations[0]).toBe('Bearer token-1');
      expect(new Set(authorizations).size).toBe(3);
      expect(authorizations[2]).toBe(`Bearer token-${issued}`);
      for (const served of [first, second, third])
        expect(negotiatesSseWire(served.requests[0]?.wire ?? null)).toBe(true);
      client.dispose();
      expect(calls()).toEqual([
        {
          name: 'rpc.client.calls',
          value: 1,
          attributes: {
            'rpc.system': 'putnami',
            'rpc.service': 'logs',
            'rpc.method': 'tailLogs',
            'network.protocol.name': 'sse',
            'http.response.status_code': 200,
          },
        },
      ]);
    },
  );

  specTest(
    'reopens a best-effort stream with the original query and keeps its queue',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'a-best-effort-stream-reopens-the-original-query-and-keeps-its-queue',
    },
    async () => {
      const log = new DurableLog();
      const first = instance(log, true);
      const second = instance(log, true);
      const router = front(first);
      const client = logsClient(router.url, logOperation(BEST_EFFORT, 4096, true));
      const stream = client.tail({ selector: ['svc=api'] });
      const received: string[] = [];
      const ended = new Promise<Error | undefined>((resolve) => {
        stream.onError(resolve);
        stream.onComplete(() => resolve(undefined));
      });
      await eventually('the first connection', () => first.requests.length === 1);
      log.append('a1', 'a2', 'a3');
      await eventually('the first instance to write three entries', () => first.sent === 3);
      // Nothing was received yet: the caller has not subscribed.
      router.current = second;
      first.drain();
      await eventually('the reopening to reach the second instance', () => second.requests.length === 1);
      log.append('b1');
      log.end();
      stream.onMessage((entry) => received.push(entry['line'] as string));
      expect(await ended).toBeUndefined();
      expect(received).toEqual(['a1', 'a2', 'a3', 'b1']);
      expect(second.requests[0]?.query).toEqual({ selector: ['svc=api'] });
      client.dispose();
    },
  );

  specTest(
    'ends the session with the typed refusal of a position, without another reopening',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'a-typed-refusal-of-a-position-ends-the-session-without-another-reopening',
    },
    async () => {
      const log = new DurableLog();
      const first = instance(log, false);
      const second = instance(log, false);
      const router = front(first);
      const client = logsClient(router.url, logOperation(CURSOR, 4096, true));
      const stream = client.tail({});
      const received: string[] = [];
      const ended = new Promise<Error | undefined>((resolve) => {
        stream.onMessage((entry) => received.push(entry['cursor'] as string));
        stream.onError(resolve);
        stream.onComplete(() => resolve(undefined));
      });
      log.append('one', 'two', 'three');
      await eventually('three values', () => received.length === 3);
      log.floor = 5;
      router.current = second;
      first.drain();
      const failure = await ended;
      expect(failure).toBeInstanceOf(ClientFrameworkError);
      expect((failure as ClientFrameworkError).status).toBe(400);
      expect(received).toEqual(['c1', 'c2', 'c3']);
      expect(second.requests.map((request) => request.query)).toEqual([{ cursor: ['c3'] }]);
      client.dispose();
    },
  );

  test('ends the session when the message handler throws, without reopening', async () => {
    const log = new DurableLog();
    const first = instance(log, false);
    const router = front(first);
    const client = logsClient(router.url, logOperation(CURSOR, 4096, true));
    const stream = client.tail({});
    const received: string[] = [];
    const thrown = new Error('the caller could not store c2');
    const ended = new Promise<Error | undefined>((resolve) => {
      stream.onMessage((entry) => {
        received.push(entry['cursor'] as string);
        if (entry['cursor'] === 'c2') throw thrown;
      });
      stream.onError(resolve);
      stream.onComplete(() => resolve(undefined));
    });
    log.append('one', 'two', 'three');
    // The session sanitizes the handler's error as the legacy reader does.
    const failure = await ended;
    expect(failure).toBeInstanceOf(ClientFrameworkError);
    expect((failure as ClientFrameworkError).code).toBe('client.remote');
    // The handler saw c2 once: its exception is not a broken connection.
    expect(received).toEqual(['c1', 'c2']);
    expect(first.requests).toHaveLength(1);
    client.dispose();
  });

  specTest(
    'stops at the framework bound shared with WebSocket resume',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'a-negotiated-stream-that-keeps-breaking-stops-at-the-framework-bound',
    },
    async () => {
      const requests: Seen[] = [];
      const server = Bun.serve({
        port: 0,
        fetch(request) {
          requests.push(seen(request));
          // Every connection ends before a terminal.
          return new Response(`data: {"cursor":"c${requests.length}","line":"x"}\n\n`, {
            headers: { 'Content-Type': 'text/event-stream', [SSE_WIRE_HEADER]: SSE_WIRE_V1 },
          });
        },
      });
      servers.push(() => server.stop(true));
      const client = logsClient(`http://localhost:${server.port}`, logOperation(CURSOR, 4096, true));
      const stream = client.tail({});
      const failure = await new Promise<Error | undefined>((resolve) => {
        stream.onMessage(() => undefined);
        stream.onError(resolve);
        stream.onComplete(() => resolve(undefined));
      });
      expect(failure).toBeInstanceOf(ClientResponseContractError);
      expect((failure as Error).message).toContain('interrupted');
      expect(requests).toHaveLength(1 + MAX_STREAM_RESUME_ATTEMPTS);
      client.dispose();
    },
  );

  specTest(
    'refuses a provider that does not acknowledge the wire, without fallback',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'a-provider-that-does-not-acknowledge-the-wire-is-refused-without-fallback',
    },
    async () => {
      let sse = 0;
      let upgrades = 0;
      const server = Bun.serve({
        port: 0,
        fetch(request) {
          if (request.headers.get('upgrade')) {
            upgrades += 1;
            return new Response('no', { status: 404 });
          }
          sse += 1;
          // An old provider: the legacy framing, no acknowledgment.
          return new Response('data: {"cursor":"c1","line":"one"}\n\n', {
            headers: { 'Content-Type': 'text/event-stream' },
          });
        },
      });
      servers.push(() => server.stop(true));
      const operation = logOperation(CURSOR, 4096, true);
      // A declared-replayable operation that also declares a WebSocket: the
      // dispatcher may fall back, but only when the provider says a wire is
      // absent. A missing acknowledgment says the provider is too old.
      operation.transports.push({
        protocol: 'websocket',
        path: '/logs/tail',
        encoding: 'json',
        websocket: { subprotocol: 'putnami.service.v1', resume: false },
      });
      const client = logsClient(`http://localhost:${server.port}`, operation);
      const stream = client.tail({});
      const messages: unknown[] = [];
      const failure = await new Promise<Error | undefined>((resolve) => {
        stream.onMessage((entry) => messages.push(entry));
        stream.onError(resolve);
        stream.onComplete(() => resolve(undefined));
      });
      expect(failure).toBeInstanceOf(ClientResponseContractError);
      expect((failure as Error).message).toContain(SSE_WIRE_V1);
      expect(messages).toEqual([]);
      expect({ sse, upgrades }).toEqual({ sse: 1, upgrades: 0 });
      client.dispose();
    },
  );

  specTest(
    'releases the connection of a reopening the caller cancels and emits one measurement',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'closing-a-reopening-stream-releases-its-connection-and-emits-one-measurement',
    },
    async () => {
      collector = new TelemetryCollector();
      setCollector(collector);
      let connections = 0;
      let released = false;
      let reopening: () => void = () => undefined;
      const reopened = new Promise<void>((resolve) => {
        reopening = resolve;
      });
      const server = Bun.serve({
        port: 0,
        fetch(request) {
          connections += 1;
          const headers = { 'Content-Type': 'text/event-stream', [SSE_WIRE_HEADER]: SSE_WIRE_V1 };
          if (connections === 1) return new Response('data: {"cursor":"c1","line":"one"}\n\n', { headers });
          // The continuation's handshake never completes on its own.
          reopening();
          return new Promise<Response>((resolve) => {
            request.signal.addEventListener('abort', () => {
              released = true;
              resolve(new Response(null, { status: 499 }));
            });
          });
        },
      });
      servers.push(() => server.stop(true));
      const client = logsClient(`http://localhost:${server.port}`, logOperation(CURSOR, 4096, true));
      const abort = new AbortController();
      const stream = client.tail({}, abort.signal);
      const failure = new Promise<Error | undefined>((resolve) => {
        stream.onMessage(() => undefined);
        stream.onError(resolve);
        stream.onComplete(() => resolve(undefined));
      });
      await reopened;
      abort.abort();
      expect(await failure).toBeInstanceOf(ClientCanceledError);
      await eventually('the reopening connection to be released', () => released);
      client.dispose();
      const measured = calls();
      expect(measured).toHaveLength(1);
      expect(measured[0]?.attributes).toMatchObject({ 'rpc.method': 'tailLogs', 'network.protocol.name': 'sse' });
    },
  );

  specTest(
    'runs one idle budget across every connection: a reopening never resets it',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'a-reopening-never-resets-the-idle-budget',
    },
    async () => {
      const idleMs = 300;
      const holdMs = 150;
      let connections = 0;
      const server = Bun.serve({
        port: 0,
        fetch() {
          connections += 1;
          const headers = { 'Content-Type': 'text/event-stream', [SSE_WIRE_HEADER]: SSE_WIRE_V1 };
          if (connections === 1) return new Response('data: {"cursor":"c1","line":"one"}\n\n', { headers });
          // Every reopening is admitted and acknowledged, stays silent for half
          // the idle budget, then breaks before a terminal.
          return new Response(
            new ReadableStream({
              async start(controller) {
                await Bun.sleep(holdMs);
                controller.close();
              },
            }),
            { headers },
          );
        },
      });
      servers.push(() => server.stop(true));
      const base = logOperation(CURSOR, 4096, true);
      const operation: ClientContractOperation = {
        ...base,
        resilience: { stream: { ...base.resilience?.stream, idleTimeoutMs: idleMs } },
      };
      const client = logsClient(`http://localhost:${server.port}`, operation);
      const stream = client.tail({});
      const received: unknown[] = [];
      const failure = await new Promise<Error | undefined>((resolve) => {
        stream.onMessage((entry) => received.push(entry));
        stream.onError(resolve);
        stream.onComplete(() => resolve(undefined));
      });
      // Five silent reopenings outlast the idle budget. Had the admission of a
      // reopening reset it, the session would have reached the sixth break.
      expect(failure).toBeInstanceOf(ClientDeadlineError);
      expect(received).toEqual([{ cursor: 'c1', line: 'one' }]);
      expect(connections).toBeLessThan(1 + MAX_STREAM_RESUME_ATTEMPTS);
      client.dispose();
    },
  );

  specTest(
    'bounds the handshake of a reopening on its own, and its failure ends the session',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'a-reopening-has-its-own-bounded-handshake-and-its-failure-ends-the-session',
    },
    async () => {
      let connections = 0;
      const server = Bun.serve({
        port: 0,
        fetch(request) {
          connections += 1;
          const headers = { 'Content-Type': 'text/event-stream', [SSE_WIRE_HEADER]: SSE_WIRE_V1 };
          if (connections === 1) return new Response('data: {"cursor":"c1","line":"one"}\n\n', { headers });
          // The reopening's provider never answers.
          return new Promise<Response>((resolve) => {
            request.signal.addEventListener('abort', () => resolve(new Response(null, { status: 499 })));
          });
        },
      });
      servers.push(() => server.stop(true));
      const base = logOperation(CURSOR, 4096, true);
      const operation: ClientContractOperation = {
        ...base,
        // The SSE handshake budget is the declared attempt timeout in both runtimes.
        resilience: { ...base.resilience, attemptTimeoutMs: 200 },
      };
      const client = logsClient(`http://localhost:${server.port}`, operation);
      const stream = client.tail({});
      const received: unknown[] = [];
      const failure = await new Promise<Error | undefined>((resolve) => {
        stream.onMessage((entry) => received.push(entry));
        stream.onError(resolve);
        stream.onComplete(() => resolve(undefined));
      });
      expect(failure).toBeInstanceOf(ClientDeadlineError);
      expect(received).toEqual([{ cursor: 'c1', line: 'one' }]);
      expect(connections).toBe(2);
      client.dispose();
    },
  );
});

describe('a reopening refused with 401', () => {
  specTest(
    're-mints the forwarded user credential once, and the first opening never does',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'every-reopening-resolves-credentials-and-a-refused-one-re-mints-a-forwarded-user-credential-once',
    },
    async () => {
      const { runInContext } = await import('@putnami/runtime');
      const { registerServiceClient } = await import('../../src/runtime/service-binding');
      const { application } = await import('@putnami/application');
      const cases = [
        {
          name: 'the re-minted credential continues the stream',
          accepts: (n: number, auth: string | null) => n === 1 || auth === 'Bearer user-2',
          terminal: undefined,
        },
        { name: 'a second refusal after the re-mint stands', accepts: (n: number) => n === 1, terminal: 401 },
      ];
      for (const tc of cases) {
        const requests: Seen[] = [];
        let taken: () => void = () => undefined;
        const firstTaken = new Promise<void>((resolve) => {
          taken = resolve;
        });
        const server = Bun.serve({
          port: 0,
          fetch(request) {
            requests.push(seen(request));
            const n = requests.length;
            if (!tc.accepts(n, request.headers.get('authorization'))) {
              return Response.json(
                { code: 'unauthorized', error: 'Unauthorized', message: 'expired' },
                { status: 401 },
              );
            }
            const headers = { 'Content-Type': 'text/event-stream', [SSE_WIRE_HEADER]: SSE_WIRE_V1 };
            if (n > 1) return new Response(SSE_COMPLETE_FRAME, { headers });
            const encoder = new TextEncoder();
            return new Response(
              new ReadableStream({
                async start(controller) {
                  controller.enqueue(encoder.encode('data: {"cursor":"c1","line":"one"}\n\n'));
                  await firstTaken;
                  controller.close();
                },
              }),
              { headers },
            );
          },
        });
        servers.push(() => server.stop(true));
        const operation = logOperation(CURSOR, 4096, true, { alternatives: [{ allOf: [{ profile: 'caller' }] }] });
        let refreshes = 0;
        const app = application();
        registerServiceClient(
          app,
          LogsClient,
          {
            contract: {
              protocolVersion: 1,
              service: { id: 'logs', audience: 'api://logs' },
              credentials: { caller: { kind: 'forwarded-user-token' } },
            },
            service: 'logs',
            operations: { tailLogs: operation },
            transport: 'http',
          },
          {
            url: `http://localhost:${server.port}`,
            clientId: 'consumer',
            allowInsecure: true,
            credentials: {
              caller: { source: 'forwarded-user', refresh: () => Promise.resolve(`user-${++refreshes + 1}`) },
            },
          },
        );
        await app.start();
        try {
          const outcome = await runInContext({ __authorizationHeader: 'Bearer user-1' }, () => {
            const stream = app.context.get(LogsClient).tail({});
            return new Promise<Error | undefined>((resolve) => {
              stream.onMessage((entry) => {
                expect(entry['cursor']).toBe('c1');
                taken();
              });
              stream.onError(resolve);
              stream.onComplete(() => resolve(undefined));
            });
          });
          if (tc.terminal === undefined) expect(outcome, tc.name).toBeUndefined();
          else expect((outcome as ClientFrameworkError).status, tc.name).toBe(tc.terminal);
          expect(refreshes, tc.name).toBe(1);
          expect(requests.length, tc.name).toBe(3);
          expect(
            requests.map((request) => request.authorization),
            tc.name,
          ).toEqual(['Bearer user-1', 'Bearer user-1', 'Bearer user-2']);
          expect(
            requests.slice(1).map((request) => request.query),
            tc.name,
          ).toEqual([{ cursor: ['c1'] }, { cursor: ['c1'] }]);
        } finally {
          await app.stop();
        }
      }
      // The first opening never re-mints.
      let openings = 0;
      const server = Bun.serve({
        port: 0,
        fetch() {
          openings += 1;
          return Response.json({ code: 'unauthorized', error: 'Unauthorized', message: 'expired' }, { status: 401 });
        },
      });
      servers.push(() => server.stop(true));
      let refreshes = 0;
      const app = application();
      registerServiceClient(
        app,
        LogsClient,
        {
          contract: {
            protocolVersion: 1,
            service: { id: 'logs', audience: 'api://logs' },
            credentials: { caller: { kind: 'forwarded-user-token' } },
          },
          service: 'logs',
          operations: {
            tailLogs: logOperation(CURSOR, 4096, true, { alternatives: [{ allOf: [{ profile: 'caller' }] }] }),
          },
          transport: 'http',
        },
        {
          url: `http://localhost:${server.port}`,
          clientId: 'consumer',
          allowInsecure: true,
          credentials: {
            caller: { source: 'forwarded-user', refresh: () => Promise.resolve(`user-${++refreshes + 1}`) },
          },
        },
      );
      await app.start();
      try {
        const outcome = await runInContext({ __authorizationHeader: 'Bearer user-1' }, () => {
          const stream = app.context.get(LogsClient).tail({});
          return new Promise<Error | undefined>((resolve) => {
            stream.onError(resolve);
            stream.onComplete(() => resolve(undefined));
          });
        });
        expect((outcome as ClientFrameworkError).status).toBe(401);
        expect({ refreshes, openings }).toEqual({ refreshes: 0, openings: 1 });
      } finally {
        await app.stop();
      }
    },
  );
});
