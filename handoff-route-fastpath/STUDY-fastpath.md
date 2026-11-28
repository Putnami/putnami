# Study: leads 5 and 6 of epic Putnami/putnami#1671

Leads from comment 7236384127 ("Leads toward a gap under 5 %"):

- Lead 5: a JSON result written straight into a native `Response` with headers
  computed at start (slice 4 of #1415): what landed.
- Lead 6: a handler compiled per route that skips the generic pipeline
  (context, chain, dispatcher) for a simple route Bun matches natively.

Base: `origin/main` at `ae10a610` (after #1656 and #1669; #1670 and #1659 not
merged yet). Bun 1.4.5, one 4-CPU host shared with other work (load quoted
per run). Prototype: worktree `/home/user/wt-fastpath`, branch
`scratch/route-fastpath` (never pushed). Its diff is
`scratch-fastpath.diff` next to this file.

## Summary

- Lead 5: the "headers computed at start" half of #1415's slice 4 landed (a
  security header set resolved at construction and a prebuilt `Headers`
  template for JSON answers); the "straight into a native `Response`" half
  did not (#1418 measured `Response.json` slower and dropped it). What is
  left, building the `Response` from the value without the `HttpResponse`,
  measures 0.1 to 0.2 us per request in process: not a PR of its own; fold
  it into lead 6.
- Lead 6: a prototype that compiles each route Bun matches natively into a
  chain-free handler (middlewares' synchronous forms, one continuation, a
  synchronous `Response` when the handler is synchronous) saves on health
  3.8 us of 17.7 on the shared harness with the real database (-21 %, to
  13.9 against Elysia's 12.7) and 3 to 4.5 us with a fake driver; on get
  1.5 to 2 us with a fake driver (-7 to 9 %), not resolved on the real
  database. Compiling the match and chain alone (P1) gains nothing measurable.
  The application's tests pass unchanged; parity 22/22.

## Method

- In process (`fpbench/inproc.ts`): one Bun process loads main's sources
  (a copy), the prototype's, and a plain-Bun reference; each request goes
  through the Bun route handler the framework registered (captured from
  `Bun.serve`'s options, `req.params` set as Bun sets it), 64 requests in
  flight, CPU from `process.cpuUsage()`, 12 rounds of 40 000 requests, order
  alternated each round. It covers the framework's JavaScript, not Bun's own
  HTTP work.
- Full run, fake driver (`fpbench/full.sh`): one server process per
  variant, wrk 4.1 with 2 threads and 64 connections, 8 s per measure after a
  3 s warm-up, rounds in alternated order, CPU per request from
  `/proc/<pid>/task/*/schedstat`. The subject is the HTTP shape of the
  `06-database` tasks API (`application().use(http()).use(logger()).use(platform()).use(api())`,
  `GET /health` and `GET /tasks/[id]` with `params({ id: Uuid })`) with a
  fake driver: `getTask` resolves a row through one promise. It isolates the
  HTTP layer, which is what lead 6 changes.
- Full run, real database: the shared harness `bench/run.sh` (wrk, 20 s per
  route, 3 rounds, Elysia and plain Bun with the same protections).
- Bun request costs (`fpbench/ablate.ts`): one plain `Bun.serve`, one route
  per thing a framework touches, to price each one in a real server.


## Lead 5: a JSON result straight into a native `Response`

### What #1415 planned and what landed

#1415 slice 4: "A JSON result is written into a native `Response` with headers
computed at start". #1418 (merged) reports: "Slice 4 (`Response.json`)
measured slower for one object, not kept". #1423 closed #1415 with the
context built on first use and plain `http()` routes on Bun's routes table.

On main today, from a handler's value to Bun's `Response`:

1. `HttpDispatcher.runHandler` -> `wrapResponse(value, ctx)` reads `Accept`
   again (`ctx.headers.get`), then `negotiateResponse`: no `Accept`, `*/*`
   or `application/json` takes the fast path to `HttpResponse.json` (landed).
2. `HttpResponse.json`: `JSON.stringify`, one `HttpResponse`, its header list
   `[['Content-Type', 'application/json']]` set directly (landed).
3. `flushContext`: `useContext()` (an `AsyncLocalStorage` read) for the
   response callbacks.
4. Security headers middleware: the header set is resolved once at
   construction (`PrecomputedHeader`), and the commonest answer, JSON whose
   only header is its `Content-Type`, takes a prebuilt `Headers` template
   (`headerTemplate`, plain and HTTPS variants) through
   `HttpResponse.useHeaderTemplate` (landed: this is "headers computed at
   start").
5. `finishRequest` -> `HttpResponse.get()`:
   `new Response(body, { headers: template, status, statusText })`.

So the "headers computed at start" half landed; the "written straight into a
native `Response`" half did not: the value still goes through an
`HttpResponse` (one object and one array) and `get()`.

### What the rest would save, measured in process

`fpbench/lead5.ts`: 200 000 calls x 10 rounds, alternated, `process.cpuUsage`,
ns per response, Bun 1.4.5, load 1.5 to 2.0:

| Path | health body | get body |
|---|---|---|
| today (negotiate, `HttpResponse.json`, template, `get()`) | 711 (min 593) | 881 (min 747) |
| `new Response(JSON.stringify(v), { headers: template, status, statusText })` | 630 (472) | 742 (662) |
| same without `status`/`statusText` | 554 (439) | 745 (643) |
| `Response.json(v, { headers: template })` | 500 (402) | 783 (608) |
| `new Response(JSON.stringify(v), { headers: plainObject })` | 1027 (776) | 1204 (926) |

What remains of lead 5 is worth 0.1 to 0.2 us per request: under the 1 to
2 us the comment estimates, and under what a full run resolves (its rounds
spread by 2 to 4 us on this host). A prebuilt `Headers` must stay: a plain
header object costs 0.3 to 0.45 us more. Not worth a pull request alone;
fold it into lead 6's compiled finish (the compiled route knows its header
template at start), where it costs no extra structure.

## Lead 6: a handler compiled per route

### Today's per-request path (main `ae10a610`)

Both routes are `api()` endpoints Bun matches natively (`buildNativeRoutes`
since #1418/#1423). The chain on them, after `actsOnRoutes`: security headers
-> logger -> dispatch (origin guard and HTTP methods skip GET, the platform
probes cannot match these paths, the cookie session store runs no session
middleware). `GET /health` returns `{ status: 'ok' }` synchronously;
`GET /tasks/[id]` validates `params({ id: Uuid })` and awaits the driver.

| Step | Where | Work and allocations |
|---|---|---|
| 1 | `nativeEndpointHandler` (http.plugin.ts) | `req.method`, `req.url` (string), path slice; get: `includes('%')`, `req.params`, `matchedPath` string rebuild; `req.headers.get('Accept')` + `acceptScore`; a new `HandlerResult` (11 fields) in a new array |
| 2 | `serveRequest` | `new ServedRequest(req)`; `new HttpContext` (about 40 slots) + `new UrlScanner`; `Date.now()`; 4 closures (`answerChainError`, `finishRequest`, `failRequest`, `executeRequest`) |
| 3 | `ServedRequest.answer` | `new Promise` + executor; `deadlines.enqueue` (`performance.now()`); `run().then(2 closures)` |
| 4 | `executeRequest` | `useMatch` (5 writes); `routeChains.select` (2 `Map.get` + bits); `dispatch` closure; `attachRequestScope` |
| 5 | `runInContext` | `AsyncLocalStorage.run` + closure; `Promise.resolve(chain(...))` |
| 6 | chain | layer closure `() => next(ctx, dispatch)`; security headers: `skipsSecurityHeaders` (`every`), `inner.then(apply)` |
| 7 | logger (async function) | async frame + its promise; `deferHttpLogContext` (`ownerOf` walk); `Date.now()`; `await next()` |
| 8 | `HttpDispatcher.handle` | `tryContext()` (ALS read); `useCandidate` (4 writes); `runHandler`: `answerError` closure |
| 9 | endpoint handler | `answeringNoContent` -> (get) `validatingInput` -> `applyInputValidation` (`validateObjectFields`, `Object.entries`, Uuid check) -> user handler (get: async, + driver promise); `Promise.resolve(result).then(answer)` |
| 10 | `runHandler` | `Promise.resolve(result).then(wrap)`; `.catch(skipNotAcceptable)` |
| 11 | value -> response | `wrapResponse` (`Accept` again) -> `HttpResponse.json`; `flushContext` (ALS read) |
| 12 | back up | logger resumes: status, `httpLogger()` (`useLogger`, ALS), `isEnabled`, `debug()`; security headers `apply`: `ctx.secured()` (closure + scan), `useHeaderTemplate` |
| 13 | settle | `chained.then(undefined, answerChainError)`; `answered.then(finishRequest)`: `res.get()` -> `new Response`; `closeRequestScope`; `answer`'s then: `deadlines.remove`, `deadlineAt = undefined`, `resolve` |

Per request: about 9 promises on health and 11 on get (plain Bun: 0 and 2),
about 15 closures, 5 objects of the framework's own (`HandlerResult` + array,
`ServedRequest`, `HttpContext`, `UrlScanner`, `HttpResponse` + header array)
besides the `Response`, 3 ALS reads, and Bun always receives a pending
`Promise`, even for a synchronous handler.

What these cost in a real server (`fpbench/ablate.ts`, plain `Bun.serve`,
wrk -c64, 3 rounds of 4 s, load 1.9 to 2.4, us per request, noisy): a sync
`Response` 9.0 to 10.5; answering through a promise +1.1 to 1.4; 10 more
promise hops +1.1 to 4.1; the same 10 hops inside `AsyncLocalStorage.run`
+1.3 to 1.8 more than without it; `req.url`, `req.headers`, `Accept`,
`req.signal` 0 to 1 each, within noise. The pipeline's cost in a server is
mostly its promise hops under ALS and the asynchronous answer, which an
in-process run under-counts (in process the whole pipeline costs 2 us on
health, 4.5 on get; in a server main is 6 to 9 us over plain Bun).

### The prototype

Branch `scratch/route-fastpath` (worktree `/home/user/wt-fastpath`, not
pushed), `scratch-fastpath.diff`: 8 files, +533 -43, in
`typescript/framework/application/src`. Three steps, measured apart:

- P1, compiled match and chain: each endpoint Bun matches gets, at start, a
  frozen one-candidate match, its route chain (`RouteChains.forRoute`) and its
  handler; the request skips `useMatch`, `select` and `HttpDispatcher.handle`
  (`runRoute`).
- P2, chain-free synchronous route: a middleware may declare a synchronous
  form `{ enter?, leave?, fail? }` (`withCompiledForm`, `compiledFormOf` in
  `route-scope.ts`); security headers (leave), origin guard (enter) and the
  logger (enter, leave, fail) declare one. A route whose every middleware has
  one runs `serveCompiled`: the same context, deadline, DI scope and error
  answers as `serveRequest`, no chain layer, `runInContext` once, and a
  `Response` returned synchronously when the handler answered synchronously
  (`answeringNoContent` answers at once for a non-promise result; the
  deadline race wraps only an asynchronous answer). Any route with a
  middleware without a form keeps P1.
- P3, fused continuation: `HttpDispatcher.runRouteThen` calls the endpoint's
  inner handler and gives its empty answer, wraps, flushes the response
  callbacks and runs the leave forms in one continuation (health: 0 promises;
  get: 5 instead of 11).

Kept, unchanged: security headers (same templates), origin guard, 1 MiB body
limit (`maxBodySizeBytes` on the context), the 30 s deadline (`deadlineAt`,
the shared sweeper, 408), HEAD, OPTIONS, 405 with Allow and 404 (the
`nativeEndpointHandler` guards still send HEAD, encoded params,
non-normalized paths and refused `Accept` to `serveRequest`; 405 and OPTIONS
stay on `fetch`), the logger's record and deferred log context, DI scopes,
raw streams, the user `onError`.

`./putnamiw test --projects @putnami/application` passes on P3 unchanged
(types and tests, 22 s); no test was modified, no `specTest` touched. Not run:
the impacted gate (web, samples), lint.

### Measured

In process (12 rounds x 40 000, 64 in flight, `process.cpuUsage`, medians,
us per request):

| Route | main | P2 | P3 | plain Bun |
|---|---|---|---|---|
| health, run 1 (load 1.6) | 3.35 | 2.50 | - | 1.36 |
| health, run 2 (load 1.3) | 3.50 | 2.26 | 2.30 | 1.47 |
| get, run 1 | 7.17 | 5.34 | - | 2.75 |
| get, run 2 | 7.44 | 5.72 | 5.86 | 2.65 |

(P1 alone, run 1: health 3.74, get 6.08: within noise of main.)

Full run, fake driver (`fpbench/full.sh`, wrk -c64, 8 s per measure, rounds
alternated, server CPU from schedstat, us per request):

| Route | run | main | P1 | P2 | P3 | plain Bun |
|---|---|---|---|---|---|---|
| health | 1 (3 rounds, load 1.5 to 1.8) | 17.83 / 16.05 / 15.06 | 16.74 / 15.24 / 14.76 | 16.16 / 12.84 / 12.91 | - | 9.75 / 9.31 / 8.78 |
| health | 2 (4 rounds, load 1.7 to 2.4) | 17.99 / 20.43 / 16.02 / 19.54 | 20.03 / 18.94 / 16.65 / 18.56 | 14.42 / 14.74 / 13.80 / 13.98 | 16.61 / 14.38 / 13.67 / 14.71 | 9.47 / 9.55 / 10.42 / 10.82 |
| get | 1 | 25.06 / 20.05 / 20.97 | 24.36 / 20.40 / 20.84 | 20.12 / 18.92 / 19.50 | - | 12.32 / 11.50 / 11.99 |
| get | 2 | 22.97 / 21.88 / 22.84 / 23.70 | 21.40 / 23.07 / 23.05 / 23.30 | 22.62 / 20.69 / 22.41 / 23.24 | 19.60 / 21.38 / 20.94 / 20.75 | 12.72 / 13.37 / 15.01 / 15.02 |

Medians: health main 16.1 / 18.8, P2 12.9 / 14.2, P3 14.5 (run 2): -3 to
-4.5 us (20 to 25 %). get main 21.0 / 22.9, P2 19.5 / 22.5, P3 20.9 (run 2):
-1.5 to -2 us (7 to 9 %), P3 steadier than P2. P1 alone: no gain within noise.

Full run, real database (shared harness `bench/run.sh`, Bun.SQL on the bench
PostgreSQL 16, wrk -c64, 20 s per route after a 5 s warm-up, rotated order;
parity 22 requests, 0 differences against Elysia and plain Bun). The run was
stopped by its time limit in round 3, before main's and the prototype's get:

| Route | round | main | prototype (P3) | Elysia | plain Bun | load1 |
|---|---|---|---|---|---|---|
| health | 1 | 15.73 | 12.98 | 11.91 | 10.40 | 1.5 to 1.7 |
| health | 2 | 18.27 | 14.33 | 13.00 | 9.97 | 1.8 to 2.0 |
| health | 3 | 17.68 | 13.93 | 12.66 | 9.34 | 2.1 to 2.3 |
| get | 1 | 62.63 | 63.86 | 43.08 | 40.77 | 1.7 to 1.9 |
| get | 2 | 64.72 | 61.34 | 44.77 | 40.18 | 1.8 to 2.5 |

Health: main 17.7, prototype 13.9 (-3.8 us, -21 %), Elysia 12.7: the
prototype closes about 75 % of health's gap to Elysia (5.0 to 1.2 us). Get on
the real database: no difference the run resolves (rounds spread 2 to 3 us;
the 1.5 to 2 us seen with the fake driver is under it), and the gap to
Elysia there (about 19 us on this harness) is the database layer's, not
HTTP's.

### What the gain is made of

- P1 (no per-request match lookup, `select`, `handle`) saves nothing that a
  full run resolves: the lookups were already cheap after #1418.
- The gain is P2/P3's: no chain layer, no async frame for the logger, and on
  health no promise at all, so Bun gets a `Response` it can write at once
  (the ablation prices an asynchronous answer at 1.1 to 1.4 us in a server,
  and each promise hop under ALS at 0.25 to 0.35 us).
- On get the handler is asynchronous, so the request keeps the user's two
  promises, one continuation and the deadline's promise and `then` (lead 2,
  #1661, removes that one). Health gains the comment's 2 to 4 us estimate
  (3 to 4.5); get gains 1.5 to 2 us with a fake driver, which a full run on
  the real database does not resolve.

### Proposed design

1. A middleware may carry a synchronous form `{ enter?, leave?, fail? }`
   (`withCompiledForm`, beside `actsOnRoutes` in `route-scope.ts`). Each
   built-in derives its `(ctx, next)` middleware from its form, so the two
   cannot drift: security headers, origin guard, logger, telemetry, the
   platform probes (`enter` only), HTTP methods.
2. At start, `buildNativeRoutes` compiles each endpoint Bun matches: its
   frozen one-candidate match, its route chain, and its forms when every
   middleware acting on it has one.
3. A compiled route's Bun handler builds the context with Bun's params, runs
   the enter forms outermost first, calls the endpoint's inner handler, gives
   the empty answer, wraps, flushes and runs the leave forms innermost first
   in one continuation, and returns the `Response` itself when the handler
   answered synchronously; the deadline race wraps only an asynchronous
   answer. The native `Response` is built from the route's header template
   (lead 5's remainder).
4. Everything else keeps today's path: a route with a middleware that has no
   form (a user middleware, CSRF, rate limit, a session store), HEAD, OPTIONS,
   405, 404, rewrites, encoded params, a refused `Accept`, streams, pages.

### Slicing into pull requests

1. `refactor(application)`: the synchronous-form contract, and each built-in
   middleware derived from its form. No behavior change; a parity test per
   form (async middleware against form, on answers, refusals and errors).
2. `perf(application)`: the compiled route for native endpoints whose
   middlewares all have forms (`serveCompiled`, `runRouteThen`, the endpoint's
   empty answer at once). New tests: a synchronous route answers Bun with a
   `Response`, not a promise; the logger record, the 5xx record, the origin
   guard's 403 with security headers, a thrown `HttpException`, `onError`, the
   408 of an asynchronous handler, DI scope closing, a raw stream, on a
   compiled route. Measured health and get, in process and full run.
3. `perf(application)`: the telemetry middleware's form, so a service with
   `telemetry()` (or `OTEL_EXPORTER_OTLP_ENDPOINT`) is compiled too; without
   it, such a service keeps today's path and gains nothing.
4. Optional, folded into 2 if it measures: the native `Response` built from
   the route's template without the `HttpResponse` (0.1 to 0.2 us).

Order with the other leads: lead 1 (#1662) is independent and adds to it;
lead 2 (#1661) still matters for asynchronous answers. Lead 6's PR 2
touches the same `serveRequest`/`ServedRequest` code as lead 2: land lead 2
first or in the same series.

### Declarations under `specs/` it would touch

`typescript/framework/application/specs/` (no text should change if parity
holds; each is a proof the compiled path must keep):

- `http-services.json`: `routing` ("Middleware wraps the handler in
  registration order, the first registered outermost": enter outermost first,
  leave innermost first), `secure-by-default`, `method-fallbacks` (HEAD,
  OPTIONS, 405 stay on the generic path), `request-bounds` and
  `request-timeout-cancels-work` (deadline, 408, the deadline released once the
  response is sent), `error-answers`, `request-log-and-trace`.
- `api-contracts.json`: `empty-result` (204 from an undefined result, inside
  the endpoint's middleware).
- `service-telemetry.json` (PR 3): `outermost-request-instrumentation`,
  `bounded-http-metrics`, `trace-propagation`, `request-step-spans`.
- A new requirement in `http-services.json` if the synchronous form becomes a
  public extension point (a user middleware could declare one): what a form
  must do to be equivalent, and that a route with any middleware without one
  runs the chain.

Their proving tests (`specTest` in `test/http/logger.middleware.test.ts`,
`security-headers.middleware.test.ts`, `native-routes.test.ts`,
`error-handler.test.ts`, `http-methods.middleware.test.ts`, ...) pass on the
prototype unchanged; a real PR adds tests in new files and modifies none.

### Risks

- Behavior parity: two code paths per request until the generic one is the
  exception. Mitigation: built-ins derived from their forms (PR 1), parity
  tests, and `bench/parity.py` (22 requests, 0 differences on the prototype).
  Edge semantics the prototype approximates and a PR must pin with tests: an
  error thrown by a leave form (it reaches the outer forms' `fail`), an error
  in a fail form (it replaces the error, as a rethrow from a catch does), a
  `NotAcceptableException` from a response callback (no answer, 404).
- A synchronous answer is never put on the deadline queue: identical in
  effect (a synchronous handler cannot be pre-empted by the sweeper timer
  today either), but `deadlines.size` and the `deadlineAt` window differ.
- Observability: the gain needs every middleware on the route to have a form.
  A production service with telemetry gains nothing until PR 3; a user
  middleware anywhere (`http().use`, `api().use(pattern)`) silently returns a
  route to the slow path. Expose which routes are compiled (a debug log at
  start, or the generated route inventory) so the fallback is visible.
- Extension points: `HttpMiddleware` stays the public contract; the form is
  an optional addition. `withCompiledForm` must stay internal until it is
  declared (above). `HttpResponse` callbacks (`__callbacks`) and
  `copyRequestContext` keep working; a handler that reads
  `ctx.__matchedHandlers[0].params` would see `undefined` (the match is shared
  and frozen; params live on `ctx.params`): nothing in the framework reads
  it.
- Scope: Bun's routes table only. `fetch`-path routes (rewrites, overlapping
  native routes, pages) keep the chain, as today.

## Files

- Prototype diff: `scratch-fastpath.diff` (branch `scratch/route-fastpath`,
  `/home/user/wt-fastpath`, not pushed). The env switch
  `PUTNAMI_COMPILED_ROUTES=0` (bench only) selects P1.
- Harness: `fpbench/` (in-process A/B `inproc.ts`, full run `full.sh` +
  `serve.ts` + `subject.ts`, Bun ablation `ablate.ts`, lead 5 `lead5.ts`;
  `main/src` is main's copy, `p2/src` the P2 snapshot).
- Raw outputs: `fastpath-measure1.log`, `fastpath-measure2.log`,
  `ablate.out`, `bench/results/fastpath-p3*`.
