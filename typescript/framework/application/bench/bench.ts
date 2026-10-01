/**
 * Performance benchmarks for the HTTP/API layer.
 *
 * Run:  bun run bench/bench.ts
 *
 * Outputs a summary table comparing component-level and end-to-end throughput.
 */

import { Int } from '@putnami/runtime';
import { validateSchema } from '../src/api/route/validate';
import { application } from '../src/application';
import { CorsMiddleware } from '../src/http/cors.middleware';
import { http } from '../src/http/http.plugin';
import { buildHttpContext } from '../src/http/http-context.builder';
import type { HttpRequestContext } from '../src/http/http-context.type';
import type { HttpMiddleware } from '../src/http/http-middleware.type';
import { HttpResponse } from '../src/http/http-response';
import { LoggerMiddleware } from '../src/http/logger.middleware';
import { OriginGuardMiddleware } from '../src/http/origin-guard.middleware';
import { Router } from '../src/http/router';
import { SecurityHeadersMiddleware } from '../src/http/security-headers.middleware';
import { UrlScanner } from '../src/http/url.scanner';

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

interface BenchResult {
  name: string;
  ops: number;
  avgNs: number;
  p99Ns: number;
}

async function bench(name: string, iterations: number, fn: () => void | Promise<void>): Promise<BenchResult> {
  // Warmup
  const warmup = Math.min(1000, Math.floor(iterations / 10));
  for (let i = 0; i < warmup; i++) await fn();

  // Measure
  const timings: number[] = [];
  const batchSize = 100;
  const batches = Math.ceil(iterations / batchSize);

  for (let b = 0; b < batches; b++) {
    const count = Math.min(batchSize, iterations - b * batchSize);
    const start = performance.now();
    for (let i = 0; i < count; i++) await fn();
    const elapsed = performance.now() - start;
    timings.push(elapsed / count);
  }

  timings.sort((a, b) => a - b);
  const avg = timings.reduce((s, t) => s + t, 0) / timings.length;
  const p99 = timings[Math.floor(timings.length * 0.99)] ?? timings[timings.length - 1];

  return {
    name,
    ops: Math.round(1000 / avg),
    avgNs: Math.round(avg * 1_000_000),
    p99Ns: Math.round(p99 * 1_000_000),
  };
}

function printResults(results: BenchResult[]) {
  const nameWidth = Math.max(40, ...results.map((r) => r.name.length + 2));
  const sep = '-'.repeat(nameWidth + 40);

  const log = (msg: string) => process.stdout.write(`${msg}\n`);

  log(sep);
  log(`${'Benchmark'.padEnd(nameWidth)}${'ops/s'.padStart(12)}${'avg (ns)'.padStart(14)}${'p99 (ns)'.padStart(14)}`);
  log(sep);
  for (const r of results) {
    log(
      `${r.name.padEnd(nameWidth)}${r.ops.toLocaleString().padStart(12)}${r.avgNs.toLocaleString().padStart(14)}${r.p99Ns.toLocaleString().padStart(14)}`,
    );
  }
  log(sep);
}

// ---------------------------------------------------------------------------
// 1. Router micro-benchmarks
// ---------------------------------------------------------------------------

async function benchRouter(): Promise<BenchResult[]> {
  const results: BenchResult[] = [];

  // Build a realistic router
  const router = new Router<() => HttpResponse>();
  const handler = () => HttpResponse.json({ ok: true });

  // Register 50 static routes and 20 parameterized routes
  for (let i = 0; i < 50; i++) {
    router.add(`/api/v1/resource${i}`, handler, { accept: new Set(['application/json', '*/*']) });
  }
  for (let i = 0; i < 20; i++) {
    router.add(`/api/v1/users/[id]/posts/[postId]/resource${i}`, handler, {
      accept: new Set(['application/json', '*/*']),
    });
  }
  router.add('/api/v1/catch/*', handler, { accept: new Set(['*/*']) });

  // All-static router: no param/wildcard routes, so find() takes the O(1) map
  // fast path instead of walking the trie.
  const staticRouter = new Router<() => HttpResponse>();
  for (let i = 0; i < 50; i++) {
    staticRouter.add(`/api/v1/resource${i}`, handler, { accept: new Set(['application/json', '*/*']) });
  }

  const N = 50_000;

  results.push(
    await bench('router.find (static route)', N, () => {
      router.find('/api/v1/resource25', ['application/json']);
    }),
  );

  results.push(
    await bench('router.find (static, all-static router)', N, () => {
      staticRouter.find('/api/v1/resource25', ['application/json']);
    }),
  );

  results.push(
    await bench('router.find (param route)', N, () => {
      router.find('/api/v1/users/abc-123/posts/456/resource10', ['application/json']);
    }),
  );

  results.push(
    await bench('router.find (wildcard)', N, () => {
      router.find('/api/v1/catch/some/deep/path', ['*/*']);
    }),
  );

  results.push(
    await bench('router.find (miss / 404)', N, () => {
      router.find('/not/registered/at/all', ['application/json']);
    }),
  );

  return results;
}

// ---------------------------------------------------------------------------
// 2. Middleware composition benchmark
// ---------------------------------------------------------------------------

async function benchMiddleware(): Promise<BenchResult[]> {
  const results: BenchResult[] = [];
  const N = 50_000;

  // Simulate the current per-request reduceRight composition
  const middlewares: HttpMiddleware[] = [
    async (_ctx, next) => next(), // trace
    async (_ctx, next) => next(), // logger
    async (_ctx, next) => next(), // cors
    async (_ctx, next) => next(), // rate-limit
    async (_ctx, next) => next(), // compression
  ];

  const fakeContext = {} as HttpRequestContext;
  const fakeDispatch = async () => HttpResponse.json({ ok: true });

  // Current approach: reduceRight per request
  results.push(
    await bench('middleware compose (reduceRight/req)', N, () => {
      const composed = middlewares.reduceRight<() => Promise<HttpResponse | undefined>>(
        (next, middleware) => async () => middleware(fakeContext, next),
        fakeDispatch,
      );
      return composed();
    }),
  );

  // Optimized: pre-compose once, call with context per request
  type MiddlewareChainFn = (
    ctx: HttpRequestContext,
    dispatch: () => Promise<HttpResponse | undefined>,
  ) => Promise<HttpResponse | undefined>;

  const preComposed = middlewares.reduceRight<MiddlewareChainFn>(
    (next, mw) => (ctx, dispatch) => mw(ctx, () => next(ctx, dispatch)),
    (_ctx, dispatch) => dispatch(),
  );

  results.push(await bench('middleware compose (pre-composed)', N, () => preComposed(fakeContext, fakeDispatch)));

  // Optimized pre-composition: the innermost middleware receives `dispatch`
  // directly as its `next`, dropping the redundant terminal closure per request.
  let optimized: MiddlewareChainFn = (ctx, dispatch) => middlewares[middlewares.length - 1](ctx, dispatch);
  for (let i = middlewares.length - 2; i >= 0; i--) {
    const mw = middlewares[i];
    const next = optimized;
    optimized = (ctx, dispatch) => mw(ctx, () => next(ctx, dispatch));
  }

  results.push(
    await bench('middleware compose (pre-composed, passthrough)', N, () => optimized(fakeContext, fakeDispatch)),
  );

  return results;
}

// ---------------------------------------------------------------------------
// 2b. Secure-default vs minimal (secure:false) chain
//
// Quantifies the per-request "security tax" the minimal / secure:false fast lane
// recovers: the always-on security middleware (headers + origin guard) wrapping
// dispatch, versus the empty-chain bypass that minimal mode falls into.
// ---------------------------------------------------------------------------

async function benchSecureVsMinimal(): Promise<BenchResult[]> {
  const results: BenchResult[] = [];
  const N = 50_000;

  const securityHeaders = SecurityHeadersMiddleware();
  const originGuard = OriginGuardMiddleware();
  // securityHeaders outermost, then origin guard — matching the plugin order.
  const secureChain = (ctx: HttpRequestContext, dispatch: () => Promise<HttpResponse | undefined>) =>
    securityHeaders(ctx, () => originGuard(ctx, dispatch));

  const ctx = { method: 'GET' } as HttpRequestContext;
  const dispatch = async () => HttpResponse.json({ ok: true });

  results.push(await bench('chain: secure default (headers + origin guard)', N, () => secureChain(ctx, dispatch)));
  results.push(await bench('chain: minimal (secure:false bypass)', N, () => dispatch()));

  return results;
}

// ---------------------------------------------------------------------------
// 3. HttpResponse chaining benchmark
// ---------------------------------------------------------------------------

async function benchHttpResponse(): Promise<BenchResult[]> {
  const results: BenchResult[] = [];
  const N = 50_000;

  // Mutable chaining (setHeader mutates in-place, returns this)
  results.push(
    await bench('HttpResponse setHeader x5 (mutable)', N, () => {
      HttpResponse.json({ ok: true })
        .setHeader('X-One', '1')
        .setHeader('X-Two', '2')
        .setHeader('X-Three', '3')
        .setHeader('X-Four', '4')
        .setHeader('X-Five', '5');
    }),
  );

  // Baseline: single construction
  results.push(
    await bench('HttpResponse single construction', N, () => {
      new HttpResponse(JSON.stringify({ ok: true }), {
        headers: {
          'Content-Type': 'application/json',
          'X-One': '1',
          'X-Two': '2',
          'X-Three': '3',
          'X-Four': '4',
          'X-Five': '5',
        },
      });
    }),
  );

  return results;
}

// ---------------------------------------------------------------------------
// 3b. Request-context construction benchmark
//
// Constructs a class-based context and touches the hot accessors
// (method/path/query/queryParams).
// ---------------------------------------------------------------------------

async function benchContext(): Promise<BenchResult[]> {
  const results: BenchResult[] = [];
  const N = 50_000;
  const url = 'http://localhost:3000/api/v1/users/abc-123/posts?page=2&limit=20';
  const headers = new Headers({ host: 'localhost:3000', Accept: 'application/json' });
  const make = () => new Request(url, { headers });

  results.push(
    await bench('context build+access (class/prototype)', N, () => {
      const ctx = buildHttpContext<HttpRequestContext>({ req: make() });
      void ctx.method;
      ctx.path();
      ctx.query();
      ctx.queryParams();
    }),
  );

  return results;
}

// ---------------------------------------------------------------------------
// 3c. DI request-scope benchmark
//
// Measures the per-request scope create+close cost that the lazy DI scope now
// skips entirely for routes (in a DI-enabled app) that never resolve anything.
// ---------------------------------------------------------------------------

async function benchScope(): Promise<BenchResult[]> {
  const results: BenchResult[] = [];
  const N = 50_000;

  class BenchScopeService {}
  const diApp = application().provide(BenchScopeService);
  await diApp.start();
  const containerContext = diApp.getActiveContext();

  if (containerContext) {
    results.push(
      await bench('di scope create+close (avoided when no inject)', N, async () => {
        const scope = containerContext.createScopeSync();
        await scope.close();
      }),
    );
  }

  await diApp.stop();
  return results;
}

// ---------------------------------------------------------------------------
// 3c2. Build-time AOT validation benchmark
//
// The win Phase 1 captures: generic schema-walking validation vs the inlined
// validator the codegen emits for simple scalar schemas (here, two Int query
// keys). Mirrors the emitted shape (inline happy path, delegate anomalies to
// the generic validator).
// ---------------------------------------------------------------------------

async function benchValidation(): Promise<BenchResult[]> {
  const results: BenchResult[] = [];
  const N = 200_000;

  const querySchema = { page: Int, limit: Int };
  const raw: Record<string, unknown> = { page: '2', limit: '20' };
  const fallback = (r: unknown) => validateSchema(querySchema, r, { coerce: true, label: 'query' });

  // Shape emitted by api({ aot: true }) for { page: Int, limit: Int }.
  const aotValidate = (input: Record<string, unknown>, fb: (r: unknown) => unknown) => {
    if (input === null || typeof input !== 'object') return fb(input);
    let v0 = input['page'];
    if (typeof v0 === 'string') v0 = Number(v0);
    if (typeof v0 !== 'number' || !Number.isInteger(v0)) return fb(input);
    let v1 = input['limit'];
    if (typeof v1 === 'string') v1 = Number(v1);
    if (typeof v1 !== 'number' || !Number.isInteger(v1)) return fb(input);
    return { page: v0, limit: v1 };
  };

  results.push(
    await bench('validateSchema (generic, 2 Int query keys)', N, () => {
      validateSchema(querySchema, raw, { coerce: true, label: 'query' });
    }),
  );
  results.push(await bench('AOT inlined validator (build-time codegen)', N, () => aotValidate(raw, fallback)));

  return results;
}

// ---------------------------------------------------------------------------
// 3d. JSON serialization benchmark
//
// Documents why a Phase-0 (no runtime `new Function`) schema-compiled serializer
// is NOT wired into the response path: JSC's native JSON.stringify beats a
// hand-specialized, schema-shaped serializer. The real serializer win requires
// build-time AOT codegen (Phase 1). Keep this as a regression guard so nobody
// re-adds a slower JS serializer expecting a speedup.
// ---------------------------------------------------------------------------

async function benchSerialization(): Promise<BenchResult[]> {
  const results: BenchResult[] = [];
  const N = 200_000;

  const flat = { id: 'abc-123-def-456', name: 'Ada Lovelace', email: 'ada@example.com', age: 36, active: true };
  // Best-case specialized serializer: fully inlined literal template, schema-shaped.
  const serializeFlat = (o: typeof flat) =>
    `{"id":${JSON.stringify(o.id)},"name":${JSON.stringify(o.name)},"email":${JSON.stringify(o.email)},"age":${Number.isFinite(o.age) ? o.age : 'null'},"active":${o.active ? 'true' : 'false'}}`;

  results.push(await bench('JSON.stringify (flat object)', N, () => void JSON.stringify(flat)));
  results.push(await bench('specialized serializer (flat object)', N, () => void serializeFlat(flat)));

  const nested = {
    id: 'x1',
    items: [
      { k: 'a', v: 1 },
      { k: 'b', v: 2 },
      { k: 'c', v: 3 },
    ],
    meta: { total: 3, ok: true },
  };
  const serializeItem = (o: (typeof nested.items)[number]) =>
    `{"k":${JSON.stringify(o.k)},"v":${Number.isFinite(o.v) ? o.v : 'null'}}`;
  const serializeNested = (o: typeof nested) =>
    `{"id":${JSON.stringify(o.id)},"items":[${o.items.map(serializeItem).join(',')}],"meta":{"total":${o.meta.total},"ok":${o.meta.ok ? 'true' : 'false'}}}`;

  results.push(await bench('JSON.stringify (nested + array)', N, () => void JSON.stringify(nested)));
  results.push(await bench('specialized serializer (nested + array)', N, () => void serializeNested(nested)));

  return results;
}

// ---------------------------------------------------------------------------
// 4. URL parsing benchmark
// ---------------------------------------------------------------------------

async function benchUrlParsing(): Promise<BenchResult[]> {
  const results: BenchResult[] = [];
  const N = 50_000;
  const url = 'http://localhost:3000/api/v1/users/abc-123/posts?page=2&limit=20';
  const headers = new Headers({ host: 'localhost:3000' });

  results.push(
    await bench('UrlScanner (path + query + host)', N, () => {
      const scanner = new UrlScanner(url, headers);
      scanner.path;
      scanner.query;
      scanner.host;
    }),
  );

  results.push(
    await bench('new URL() (native)', N, () => {
      const u = new URL(url);
      u.pathname;
      u.search;
      u.host;
    }),
  );

  return results;
}

// ---------------------------------------------------------------------------
// 5. Accept header parsing benchmark
// ---------------------------------------------------------------------------

async function benchAcceptParsing(): Promise<BenchResult[]> {
  const results: BenchResult[] = [];
  const N = 50_000;
  const acceptHeader = 'text/html, application/json;q=0.9, */*;q=0.8';

  // Current listRequestAccept (char-by-char)
  const listRequestAccept = (h?: string) => {
    const res: string[] = [];
    let i = 0;
    const buf: number[] = [];
    let skip = false;
    while (h?.charAt(i)) {
      if (h.charAt(i) === ',') {
        res.push(String.fromCharCode(...buf));
        buf.length = 0;
        skip = false;
      } else if (h.charAt(i) === ';') {
        skip = true;
      } else if (!skip) {
        buf.push(h.charCodeAt(i));
      }
      i++;
    }
    if (buf.length > 0) res.push(String.fromCharCode(...buf));
    return res.length === 0 ? ['*/*'] : res;
  };

  // Simpler split-based approach
  const listRequestAcceptSimple = (h?: string) => {
    if (!h) return ['*/*'];
    const parts = h.split(',');
    const result: string[] = [];
    for (let i = 0; i < parts.length; i++) {
      const semi = parts[i].indexOf(';');
      result.push(semi >= 0 ? parts[i].slice(0, semi).trim() : parts[i].trim());
    }
    return result.length === 0 ? ['*/*'] : result;
  };

  results.push(
    await bench('Accept parse (char-by-char)', N, () => {
      listRequestAccept(acceptHeader);
    }),
  );

  results.push(
    await bench('Accept parse (split-based)', N, () => {
      listRequestAcceptSimple(acceptHeader);
    }),
  );

  return results;
}

// ---------------------------------------------------------------------------
// 6. Router param extraction benchmark
// ---------------------------------------------------------------------------

async function benchParamExtraction(): Promise<BenchResult[]> {
  const results: BenchResult[] = [];
  const N = 50_000;

  const router = new Router<string>();
  router.add('/users/[id]/posts/[postId]', 'handler', { accept: new Set(['*/*']) });

  results.push(
    await bench('router param extraction (2 params)', N, () => {
      router.find('/users/abc-def-ghi-123/posts/post-456-789', ['*/*']);
    }),
  );

  return results;
}

// ---------------------------------------------------------------------------
// 7. Full HTTP integration benchmark
// ---------------------------------------------------------------------------

async function benchIntegration(): Promise<BenchResult[]> {
  const results: BenchResult[] = [];

  // Build a realistic server with middleware
  const plugin = http({ port: 0 });
  plugin.use(LoggerMiddleware({ exclude: ['/health'] }));
  plugin.use(CorsMiddleware({ origin: '*' }));

  // Static route returning JSON
  plugin.get('/api/health', () => HttpResponse.json({ status: 'ok' }));

  // Native route: same payload, served by Bun's router, bypassing the pipeline.
  plugin.native('/api/native-health', HttpResponse.json({ status: 'ok' }));

  // Param route
  plugin.get('/api/users/[id]', (ctx) => HttpResponse.json({ id: ctx.params?.id }));

  // POST route
  plugin.post('/api/echo', async (ctx) => {
    const body = await ctx.body();
    return HttpResponse.json(body);
  });

  const app = application().use(plugin);
  await app.start();
  const baseUrl = `http://localhost:${plugin.getServer()?.port}`;

  const N = 5000;

  results.push(
    await bench('GET /api/health (static, JSON)', N, async () => {
      const res = await fetch(`${baseUrl}/api/health`);
      await res.arrayBuffer();
    }),
  );

  results.push(
    await bench('GET /api/native-health (native, bypass)', N, async () => {
      const res = await fetch(`${baseUrl}/api/native-health`);
      await res.arrayBuffer();
    }),
  );

  results.push(
    await bench('GET /api/users/[id] (param, JSON)', N, async () => {
      const res = await fetch(`${baseUrl}/api/users/abc-123`);
      await res.arrayBuffer();
    }),
  );

  results.push(
    await bench('POST /api/echo (body parse)', N, async () => {
      const res = await fetch(`${baseUrl}/api/echo`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: '{"hello":"world"}',
      });
      await res.arrayBuffer();
    }),
  );

  results.push(
    await bench('GET /missing (404)', N, async () => {
      const res = await fetch(`${baseUrl}/not-found`);
      await res.arrayBuffer();
    }),
  );

  await app.stop();
  return results;
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

async function main() {
  const log = (msg: string) => process.stdout.write(`${msg}\n`);
  log('\n=== Putnami HTTP/API Performance Benchmark ===\n');

  const allResults: BenchResult[] = [];

  log('Running: Router micro-benchmarks...');
  allResults.push(...(await benchRouter()));

  log('Running: Middleware composition...');
  allResults.push(...(await benchMiddleware()));

  log('Running: Secure vs minimal chain...');
  allResults.push(...(await benchSecureVsMinimal()));

  log('Running: HttpResponse chaining...');
  allResults.push(...(await benchHttpResponse()));

  log('Running: Request-context construction...');
  allResults.push(...(await benchContext()));

  log('Running: DI request-scope...');
  allResults.push(...(await benchScope()));

  log('Running: AOT validation...');
  allResults.push(...(await benchValidation()));

  log('Running: JSON serialization...');
  allResults.push(...(await benchSerialization()));

  log('Running: URL parsing...');
  allResults.push(...(await benchUrlParsing()));

  log('Running: Accept header parsing...');
  allResults.push(...(await benchAcceptParsing()));

  log('Running: Param extraction...');
  allResults.push(...(await benchParamExtraction()));

  log('Running: Full HTTP integration...');
  allResults.push(...(await benchIntegration()));

  log('\n');
  printResults(allResults);
}

// biome-ignore lint/suspicious/noConsole: benchmark output tool
main().catch(console.error);
