import { afterEach, describe, expect } from 'bun:test';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { api, application, endpoint, http, openapi, Stream } from '@putnami/application';
import { Optional, resetConfigLoader } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { readOpenApiSource } from '../../src/generator/openapi-reader';
import { generateTypeScriptClient } from '../../src/generator/ts/ts-generator';
import type { StreamObserver } from '../../src/runtime/stream.type';

const CLIENT_ENTRY = resolve(import.meta.dir, '..', '..', 'src', 'index.ts');
const temporaryDirectories: string[] = [];
const running: (() => Promise<void>)[] = [];
const servers: (() => void)[] = [];

afterEach(async () => {
  for (const stop of running.splice(0)) await stop();
  for (const stop of servers.splice(0)) stop();
  for (const directory of temporaryDirectories.splice(0)) rmSync(directory, { recursive: true, force: true });
  resetConfigLoader();
});

interface LogEntry {
  readonly cursor: string;
  readonly line: string;
}

/** The event log the provider instances share: the only thing they share. */
class DurableLog {
  readonly lines: string[] = [];
  closed = false;
  private waiters: (() => void)[] = [];

  append(...lines: string[]): void {
    this.lines.push(...lines);
    this.wake();
  }

  end(): void {
    this.closed = true;
    this.wake();
  }

  async next(after: number, signal: AbortSignal): Promise<LogEntry | 'end' | 'aborted'> {
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
  readonly port: number;
  readonly queries: string[];
  sent: number;
  stop(): Promise<void>;
  spec(): unknown;
}

/**
 * One real Putnami provider instance: `api({ client })` with a route that
 * declares a cursor continuation, so the api plugin fills the negotiated wire
 * from the declaration. Stopping the application drains it: every negotiated
 * stream ends with no terminal.
 */
async function startInstance(log: DurableLog): Promise<Instance> {
  const providerHttp = http({ port: 0 });
  const providerApi = api({
    autoScan: false,
    client: { service: { id: 'logs', audience: 'api://logs' }, credentials: {} },
  });
  const served: { queries: string[]; sent: number } = { queries: [], sent: 0 };
  providerApi.register(
    '/logs/tail',
    endpoint()
      .query({ selector: String, cursor: Optional(String) })
      .returns(Stream({ cursor: String, line: String }))
      .client({
        security: { alternatives: [{ allOf: [] }] },
        idempotency: { kind: 'safe' },
        sseContinuation: { mode: 'cursor', cursor: { outputField: 'cursor', queryParameter: 'cursor' } },
        resilience: { stream: { reconnect: true } },
      })
      .handle(async (context) => {
        served.queries.push(context.query());
        const { cursor } = context.queryParams();
        let after = cursor ? Number(String(cursor).slice(1)) : 0;
        for (;;) {
          // biome-ignore lint/performance/noAwaitInLoops: entries are written in order
          const entry = await log.next(after, context.signal);
          if (entry === 'aborted' || entry === 'end') return;
          context.send(entry);
          served.sent += 1;
          after += 1;
        }
      }),
    'GET',
  );
  // The live tail declares a best-effort continuation: it starts at the head
  // of the log when the connection opens and reads no position, so what was
  // written while no connection was open is lost.
  providerApi.register(
    '/logs/follow',
    endpoint()
      .query({ selector: String })
      .returns(Stream({ cursor: String, line: String }))
      .client({
        security: { alternatives: [{ allOf: [] }] },
        idempotency: { kind: 'safe' },
        sseContinuation: { mode: 'best-effort' },
        resilience: { stream: { reconnect: true } },
      })
      .handle(async (context) => {
        let after = log.lines.length;
        served.queries.push(context.query());
        for (;;) {
          // biome-ignore lint/performance/noAwaitInLoops: entries are written in order
          const entry = await log.next(after, context.signal);
          if (entry === 'aborted' || entry === 'end') return;
          context.send(entry);
          served.sent += 1;
          after += 1;
        }
      }),
    'GET',
  );
  const providerOpenApi = openapi({ title: 'Logs', version: '1.0.0' });
  const app = application().use(providerHttp).use(providerApi).use(providerOpenApi);
  await app.start();
  const stop = async () => {
    await app.stop();
  };
  running.push(stop);
  const port = providerHttp.getServer()?.port;
  if (!port) throw new Error('provider HTTP server did not start');
  return {
    port,
    queries: served.queries,
    get sent() {
      return served.sent;
    },
    stop,
    spec: () => providerOpenApi.spec(),
  };
}

/** Routes every connection to the current instance, the way a load balancer does: the one double. */
function front(first: Instance): { url: string; current: Instance } {
  const router = { url: '', current: first };
  const server = Bun.serve({
    port: 0,
    async fetch(request) {
      const url = new URL(request.url);
      const upstream = await fetch(`http://localhost:${router.current.port}${url.pathname}${url.search}`, {
        headers: request.headers,
        signal: request.signal,
      });
      return new Response(upstream.body, { status: upstream.status, headers: upstream.headers });
    },
  });
  servers.push(() => server.stop(true));
  router.url = `http://localhost:${server.port}`;
  return router;
}

/** The parameters one provider instance read from a request's query string. */
function parameters(query: string): Record<string, string> {
  return Object.fromEntries(new URLSearchParams(query.replace(/^\?/, '')));
}

async function eventually(what: string, condition: () => boolean): Promise<void> {
  const deadline = Date.now() + 5000;
  while (!condition()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    // biome-ignore lint/performance/noAwaitInLoops: polling
    await Bun.sleep(2);
  }
}

describe('a declared SSE continuation, from provider declaration to a generated TypeScript consumer', () => {
  specTest(
    'generates a client that pins the capability and continues on another instance after the last received position',
    {
      feature: 'typescript/service-clients',
      requirement: 'declared-sse-continuation',
      check: 'the-emitted-ts-client-continues-a-declared-cursor-stream-through-the-real-runtime',
    },
    async () => {
      const log = new DurableLog();
      const first = await startInstance(log);
      const second = await startInstance(log);
      const router = front(first);

      // declare -> publish -> generate -> load: the emitted package requires the capability.
      const document = first.spec();
      if (!document) throw new Error('provider OpenAPI contract was not emitted');
      const ir = readOpenApiSource(JSON.stringify(document), { mode: 'firstParty' });
      const method = ir.services.flatMap((service) => service.methods).find((entry) => entry.path === '/logs/tail');
      expect(method?.client?.transports[0]?.sse).toEqual({
        continuation: { mode: 'cursor', cursor: { outputField: 'cursor', queryParameter: 'cursor' } },
      });
      const files = generateTypeScriptClient(ir, { packageName: '@test/logs-client' });
      const clientSource = files.find((file) => file.path.endsWith('-client.ts'))?.content ?? '';
      expect(clientSource).toContain("requireClientRuntimeCapabilities(['sse-continuation']);");
      const directory = mkdtempSync(join(tmpdir(), 'putnami-sse-continuation-client-'));
      temporaryDirectories.push(directory);
      for (const file of files) {
        const destination = join(directory, file.path);
        mkdirSync(dirname(destination), { recursive: true });
        writeFileSync(destination, file.content.replaceAll("'@putnami/client'", `'${CLIENT_ENTRY}'`));
      }
      const generated = await import(join(directory, 'src', 'index.ts'));
      const bind = Object.entries(generated).find(([name]) => name.startsWith('bind'))?.[1] as (binding: {
        url: string;
        clientId: string;
        allowInsecure: boolean;
      }) => Record<string, (input: { query: { selector: string } }) => StreamObserver<LogEntry>> & {
        dispose(): void;
      };
      const client = bind({ url: router.url, clientId: 'consumer-tests', allowInsecure: true });
      const tailLogs = 'getLogsTail';
      expect(typeof client[tailLogs]).toBe('function');

      // The real bound consumer call: a paused consumer (not subscribed yet)
      // has values decoded and queued when the first instance drains.
      const received: string[] = [];
      const stream = client[tailLogs]?.({ query: { selector: 'svc=api' } }) as StreamObserver<LogEntry>;
      const ended = new Promise<Error | undefined>((resolve) => {
        stream.onError(resolve);
        stream.onComplete(() => resolve(undefined));
      });
      await eventually('the first connection', () => first.queries.length === 1);
      log.append('one', 'two');
      await eventually('the first instance to write two entries', () => first.sent === 2);
      router.current = second;
      await first.stop();
      // Nothing was received: the reopening keeps the original selector, and
      // the second instance sends the two entries again, once.
      await eventually('the continuation to reach the second instance', () => second.queries.length === 1);
      stream.onMessage((entry) => received.push(`${entry.cursor}=${entry.line}`));
      await eventually('the two entries to reach the caller', () => received.length >= 2);
      log.append('three');
      await eventually('the third entry', () => received.length >= 3);
      log.end();
      expect(await ended).toBeUndefined();
      expect(received).toEqual(['c1=one', 'c2=two', 'c3=three']);
      expect(first.queries.map(parameters)).toEqual([{ selector: 'svc=api' }]);
      expect(second.queries.map(parameters)).toEqual([{ selector: 'svc=api' }]);
      client.dispose();
    },
  );

  specTest(
    'continues after the position the subscribed consumer received, and stops on the explicit terminal',
    {
      feature: 'typescript/service-clients',
      requirement: 'declared-sse-continuation',
      check: 'the-emitted-ts-client-continues-a-declared-cursor-stream-through-the-real-runtime',
    },
    async () => {
      const log = new DurableLog();
      const first = await startInstance(log);
      const second = await startInstance(log);
      const router = front(first);
      const document = first.spec();
      if (!document) throw new Error('provider OpenAPI contract was not emitted');
      const files = generateTypeScriptClient(readOpenApiSource(JSON.stringify(document), { mode: 'firstParty' }), {
        packageName: '@test/logs-client',
      });
      const directory = mkdtempSync(join(tmpdir(), 'putnami-sse-continuation-client-'));
      temporaryDirectories.push(directory);
      for (const file of files) {
        const destination = join(directory, file.path);
        mkdirSync(dirname(destination), { recursive: true });
        writeFileSync(destination, file.content.replaceAll("'@putnami/client'", `'${CLIENT_ENTRY}'`));
      }
      const generated = await import(join(directory, 'src', 'index.ts'));
      const bind = Object.entries(generated).find(([name]) => name.startsWith('bind'))?.[1] as (binding: {
        url: string;
        clientId: string;
        allowInsecure: boolean;
      }) => Record<string, (input: { query: { selector: string } }) => StreamObserver<LogEntry>> & {
        dispose(): void;
      };
      const client = bind({ url: router.url, clientId: 'consumer-tests', allowInsecure: true });
      const tailLogs = 'getLogsTail';
      expect(typeof client[tailLogs]).toBe('function');
      const received: string[] = [];
      const stream = client[tailLogs]?.({ query: { selector: 'svc=api' } }) as StreamObserver<LogEntry>;
      const ended = new Promise<Error | undefined>((resolve) => {
        stream.onMessage((entry) => received.push(`${entry.cursor}=${entry.line}`));
        stream.onError(resolve);
        stream.onComplete(() => resolve(undefined));
      });
      log.append('one', 'two');
      await eventually('two entries to reach the caller', () => received.length === 2);
      router.current = second;
      await first.stop();
      await eventually('the continuation to reach the second instance', () => second.queries.length === 1);
      log.append('three');
      log.end();
      expect(await ended).toBeUndefined();
      expect(received).toEqual(['c1=one', 'c2=two', 'c3=three']);
      // c2 is the last position the caller received; the terminal opened nothing more.
      expect(second.queries.map(parameters)).toEqual([{ selector: 'svc=api', cursor: 'c2' }]);
      client.dispose();
    },
  );

  specTest(
    'reopens a declared best-effort stream on another instance with the original query',
    {
      feature: 'typescript/service-clients',
      requirement: 'declared-sse-continuation',
      check: 'the-emitted-ts-client-reopens-a-declared-best-effort-stream-through-the-real-runtime',
    },
    async () => {
      const log = new DurableLog();
      const first = await startInstance(log);
      const second = await startInstance(log);
      const router = front(first);
      const document = first.spec();
      if (!document) throw new Error('provider OpenAPI contract was not emitted');
      const ir = readOpenApiSource(JSON.stringify(document), { mode: 'firstParty' });
      const method = ir.services.flatMap((service) => service.methods).find((entry) => entry.path === '/logs/follow');
      expect(method?.client?.transports[0]?.sse).toEqual({ continuation: { mode: 'best-effort' } });
      const files = generateTypeScriptClient(ir, { packageName: '@test/logs-client' });
      const directory = mkdtempSync(join(tmpdir(), 'putnami-sse-continuation-client-'));
      temporaryDirectories.push(directory);
      for (const file of files) {
        const destination = join(directory, file.path);
        mkdirSync(dirname(destination), { recursive: true });
        writeFileSync(destination, file.content.replaceAll("'@putnami/client'", `'${CLIENT_ENTRY}'`));
      }
      const generated = await import(join(directory, 'src', 'index.ts'));
      const bind = Object.entries(generated).find(([name]) => name.startsWith('bind'))?.[1] as (binding: {
        url: string;
        clientId: string;
        allowInsecure: boolean;
      }) => Record<string, (input: { query: { selector: string } }) => StreamObserver<LogEntry>> & {
        dispose(): void;
      };
      const client = bind({ url: router.url, clientId: 'consumer-tests', allowInsecure: true });
      const followLogs = 'getLogsFollow';
      expect(typeof client[followLogs]).toBe('function');
      const received: string[] = [];
      const stream = client[followLogs]?.({ query: { selector: 'svc=api' } }) as StreamObserver<LogEntry>;
      const ended = new Promise<Error | undefined>((resolve) => {
        stream.onMessage((entry) => received.push(`${entry.cursor}=${entry.line}`));
        stream.onError(resolve);
        stream.onComplete(() => resolve(undefined));
      });
      await eventually('the first connection', () => first.queries.length === 1);
      log.append('one');
      await eventually('the first entry to reach the caller', () => received.length === 1);
      router.current = second;
      await first.stop();
      await eventually('the reopening to reach the second instance', () => second.queries.length === 1);
      log.append('two');
      log.end();
      expect(await ended).toBeUndefined();
      expect(received).toEqual(['c1=one', 'c2=two']);
      // The reopening sends the original query and no position: the runtime
      // never synthesizes one in best-effort mode.
      expect(first.queries.map(parameters)).toEqual([{ selector: 'svc=api' }]);
      expect(second.queries.map(parameters)).toEqual([{ selector: 'svc=api' }]);
      client.dispose();
    },
  );
});
