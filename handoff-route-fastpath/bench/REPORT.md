# Where Putnami's CPU per request goes (epic Putnami/putnami#1671)

Status: **complete** (2026-11-28 12:00 UTC).

**Caution:** sections 1 and 2 ran with `BUN_OPTIONS=--smol`, which this
session's shell exports to every process (smaller heap, more GC). Every
`bun` started from an agent shell here runs with it, micro-benchmarks
included, unless it is unset. The harness now unsets it (`bench.py`,
`BENCH_BUN_OPTIONS` to set options on purpose); section 3 re-measures without it.
(this file is updated as each lands). Harness and exact commands: `README.md` in
this directory.

Method: wrk 4.1.0, 64 connections, 2 threads, 5 s warm-up + 20 s per route,
3 rounds in rotated subject order; CPU per request = delta utime+stime of the
service process from `/proc/<pid>/stat` / completed requests. Bun 1.4.5,
`NODE_ENV=production`, PostgreSQL 16.15 (17 not installable here: PGDG is
blocked by the egress proxy) on its own cluster 127.0.0.1:55432, `fsync=off`.
Host: 4 vCPU Intel Xeon @ 2.10GHz, KVM/Firecracker. Parity: 22 requests, 0
differences between Putnami, Elysia and plain Bun before every run.

## 0. Subjects and their alignment

The earlier bench was lost; its subjects were rebuilt here (`subjects/`).
The repository's `typescript/samples/06-database` is a users API, so the
Putnami subject is a tasks API written the same way (`Table` + `Repository`,
`endpoint().params/query/body`, `logger()`, `sql()`, `platform()`, `api()`),
with routes registered on `api({ autoScan: false })` instead of the file scan.
Aligned as the comment's "Fairness" section asks:
- one row conversion per row and one `toISOString()` on the driver's `Date`
  (Elysia/Bun `toTask`; Putnami's `DateIso` column does it in `toEntity` and
  the entity is returned as is); no `new Date` of a `Date`;
- the title rule is at most 200 UTF-16 units everywhere (`MaxLength(200)`
  counts `.length`, as TypeBox does); the parity set checks a 150-emoji title
  (300 units: 400 Bad Request everywhere) and a 200-`e`-acute title (201 Created everywhere);
- the same statement text as the Repository sends (`SELECT * FROM "tasks"
  WHERE "id" = $1 LIMIT $2`, `... ORDER BY "created_at" ASC LIMIT $1`, the
  upsert `INSERT ... ON CONFLICT ("id") DO UPDATE ... RETURNING *`), the same
  pool size (10).

The repository sample needs no change for these two points: it has no
`toTask` and no title rule. One optional fix, not a performance change:
`src/tables/users.ts` declares `createdAt: Column(String, ...)` on a
`TIMESTAMPTZ`, so the entity carries a `Date` typed as a string;
`Column(DateIso, { columnName: 'created_at', default: 'NOW()' })` makes it an
ISO string, as its type says.

## 1. Baseline: main ae10a610 vs Elysia vs plain Bun (us of CPU per request)

Run `results/baseline-main`, 2026-11-28 08:25 UTC.

| subject | route | r1 | r2 | r3 | median | load1 (min-max) | host busy % |
|---|---|---|---|---|---|---|---|
| Putnami main ae10a610 | health | 20.5 | 17.3 | 18.2 | **18.2** | 1.13-2.24 | 43 |
| Elysia 1.4.32 | health | 13.6 | 15.1 | 14.2 | **14.2** | 1.28-3.06 | 46 |
| plain Bun | health | 13.2 | 12.7 | 11.8 | **12.7** | 1.47-2.63 | 49 |
| Putnami main ae10a610 | get | 65.7 | 61.8 | 71.1 | **65.7** | 2.16-2.23 | 46 |
| Elysia 1.4.32 | get | 43.8 | 43.0 | 49.1 | **43.8** | 2.02-2.28 | 50 |
| plain Bun | get | 43.2 | 42.2 | 46.1 | **43.2** | 1.93-3.17 | 51 |

Reading: health is 4.0 us (28 %) over Elysia, the same as the epic's first
table. On this host a database round trip costs more than in the epic's run
(Elysia get 43.8 us here against 28 to 31 there; health is the same, so the
difference is in the driver/socket path, not in JS), and Putnami's get is
**21.9 us** over Elysia, not the ~7 us the epic expected after #1656: see the
database A/B below for where it goes.

## 2. Branches: main, #1670, #1659, and main + both (us of CPU per request)

Run `results/branches`, 2026-11-28 08:33 UTC. Heads: main ae10a610,
perf/application-logger 981b13d5 (PR #1670), perf/database-awaits 4e7a8764
(#1659; both branches are based on 4a60b60, before #1669), "combined" =
main + both branches merged locally (716685d2, not pushed).

| subject | route | r1 | r2 | r3 | median | load1 (min-max) | host busy % |
|---|---|---|---|---|---|---|---|
| main | health | 21.0 | 17.2 | 18.1 | **18.1** | 1.60-1.98 | 43 |
| #1670 logger | health | 20.1 | 17.7 | 17.6 | **17.7** | 1.88-2.49 | 42 |
| #1659 awaits | health | 18.7 | 18.8 | 17.3 | **18.7** | 1.86-2.32 | 42 |
| main + both | health | 16.7 | 19.0 | 17.3 | **17.3** | 1.91-2.37 | 48 |
| Elysia | health | 13.8 | 14.5 | 12.7 | **13.8** | 2.00-2.56 | 47 |
| plain Bun | health | 10.7 | 12.5 | 11.9 | **11.9** | 1.98-2.75 | 57 |
| main | get | 65.3 | 60.5 | 61.0 | **61.0** | 2.26-3.43 | 53 |
| #1670 logger | get | 67.6 | 66.0 | 61.5 | **66.0** | 2.06-3.42 | 48 |
| #1659 awaits | get | 67.0 | 62.1 | 66.2 | **66.2** | 1.93-2.73 | 45 |
| main + both | get | 65.0 | 58.0 | 61.0 | **61.0** | 1.88-2.60 | 46 |
| Elysia | get | 43.8 | 42.9 | 39.0 | **42.9** | 2.10-2.46 | 54 |
| plain Bun | get | 41.8 | 40.8 | 40.8 | **40.8** | 2.12-2.78 | 51 |

Reading: the full bench does not separate the two branches from main (their
measured gains, 1.2 us on health and 0.9 us in process on a read, are under
this bench's 3 to 7 us spread on get). Main + both: health 17.3 us (+3.5 us,
+25 % over Elysia), get 61.0 us (+18.1 us, +42 %).

## 3. Database layer A/B and the baseline again, without `--smol` (us of CPU per request)

Run `results/nosmol-main`, 2026-11-28 09:53 UTC, all subjects in one run,
rounds rotated. "combined" = main ae10a610 + #1670 + #1659. Variants of the
Putnami subject, the same pipeline and routes:
`repo` = `Repository(Tasks).get({ id })` (what the sample does);
`txa` = `useTxConnection('default','read')` + `abortableQuery(sql\`SELECT * FROM "tasks" WHERE "id" = ${id} LIMIT ${1}\`)`, the statement text the Repository sends;
`tx` = the same without `abortableQuery`;
`raw` = Bun.SQL's own pool (max 10), no `@putnami/database`, no `sql()` plugin.

| subject | route | r1 | r2 | r3 | median | load1 (min-max) | host busy % |
|---|---|---|---|---|---|---|---|
| main | health | 20.7 | 17.7 | 19.4 | **19.4** | 1.86-2.12 | 42 |
| combined (repo) | health | 17.0 | 16.0 | 18.9 | **17.0** | 1.88-2.31 | 43 |
| combined tx | health | 19.9 | 15.4 | 17.3 | **17.3** | 1.76-2.14 | 42 |
| combined txa | health | 16.3 | 16.8 | 18.2 | **16.8** | 1.84-1.90 | 42 |
| combined raw | health | 15.8 | 16.2 | 19.1 | **16.2** | 1.70-1.90 | 43 |
| combined with `--smol` | health | 16.6 | 18.9 | 17.8 | **17.8** | 1.78-1.87 | 44 |
| Elysia | health | 12.4 | 12.7 | 12.8 | **12.7** | 1.73-2.07 | 45 |
| plain Bun | health | 9.7 | 9.4 | 10.0 | **9.7** | 1.70-1.98 | 46 |
| main | get | 75.7 | 64.2 | 66.6 | **66.6** | 1.73-2.40 | 45 |
| combined (repo) | get | 71.4 | 65.2 | 62.6 | **65.2** | 1.83-2.11 | 46 |
| combined txa | get | 52.5 | 57.7 | 51.4 | **52.5** | 1.68-2.35 | 48 |
| combined tx | get | 49.6 | 46.6 | 47.9 | **47.9** | 1.76-1.87 | 49 |
| combined raw | get | 44.1 | 42.9 | 43.9 | **43.9** | 1.72-2.61 | 49 |
| combined with `--smol` | get | 64.3 | 61.6 | 62.6 | **62.6** | 1.74-2.32 | 46 |
| Elysia | get | 41.2 | 48.4 | 41.9 | **41.9** | 1.84-2.02 | 50 |
| plain Bun | get | 37.9 | 44.3 | 39.8 | **39.8** | 1.81-2.15 | 50 |

What the database layer adds on a point read (combined, medians):

| step | get us | what it is |
|---|---|---|
| raw Bun.SQL under Putnami's pipeline | 43.9 | +2.0 over Elysia: the HTTP pipeline share on get (health: +3.5) |
| + `@putnami/database` client and pool (`tx`) | 47.9 (**+4.0**) | `useTxConnection`, captured query replayed on a leased member, camelCase row copy |
| + `abortableQuery` (`txa`) | 52.5 (**+4.6**) | per statement: `onAbort` hook (first one creates a `Set` and reads `req.signal`, which makes Bun build an `AbortSignal`, and adds a listener), a `new Promise`, `Promise.race`, two async frames |
| + `Repository.get` (`repo`) | 65.2 (**+12.7**) | `get` -> `findOne` -> `find` -> `observe` (two `performance.now`, `recordQuery` metrics), `buildWhere`, five nested `sql\`\`` fragments replayed into the driver, `toEntity` second row copy with `Object.entries` |
| total over raw | **+21.3** | the database layer is 21 of the 23 us get gap to Elysia |

Reading:
- `--smol` does not change the picture (get 62.6 vs 65.2, health 17.8 vs
  17.0, within the spread). Sections 1 and 2 stand.
- **The Repository path is the largest single cost on get: about 13 us over
  the same statement through the same client.** The epic's estimate (about
  5 us for the whole database layer) is off by 4x. Then `abortableQuery`
  (about 4.6 us, a lead nobody named), then the client and pool (about 4 us).
- Putnami's pipeline with raw Bun.SQL is within 2 us (5 %) of Elysia on get
  and 3.5 us on health: the HTTP side alone is at the target on get.

## 4. Ablation of each default on health (main + both branches, no `--smol`)

Run `results/ablation`, 2026-11-28 09:09 UTC, 3 rounds, `BENCH_OFF=` switches
of the Putnami subject. Delta = the variant minus the full Putnami subject of
the same round (negative = cheaper without it).

| variant | r1 | r2 | r3 | median | delta per round | mean delta |
|---|---|---|---|---|---|---|
| Putnami, all defaults | 17.5 | 18.5 | 19.9 | **18.5** | - | - |
| no logger() | 16.3 | 17.6 | 19.4 | 17.6 | -1.2, -0.9, -0.4 | **-0.8** |
| no platform() | 16.2 | 17.7 | 20.1 | 17.7 | -1.3, -0.8, +0.2 | -0.6 |
| no security headers | 17.0 | 19.3 | 18.0 | 18.0 | -0.5, +0.8, -1.9 | -0.5 |
| no origin guard | 16.6 | 20.0 | 17.6 | 17.6 | -0.9, +1.5, -2.2 | -0.5 |
| no request deadline (`requestTimeoutMs: 0`) | 16.6 | 19.5 | 17.6 | 17.6 | -0.9, +1.0, -2.2 | -0.7 |
| no automatic HEAD/OPTIONS | 16.9 | 20.9 | 17.7 | 17.7 | -0.6, +2.4, -2.2 | -0.1 |
| all six off | 17.4 | 19.1 | 16.9 | 17.4 | -0.1, +0.6, -2.9 | **-0.8** |
| Elysia (same protections) | 12.6 | 13.6 | 13.2 | **13.2** | -4.9, -4.9, -6.7 | -5.5 |

Load 1.67-2.10, host busy 41-47 %.

Reading: on this host the full bench resolves about 1 us on health only as a
mean of rounds, not per round (round-to-round drift is 1.5 to 2.5 us). Within
that: **all six defaults together cost under 1 us** (all off: -0.8 us mean);
the logger (-0.8, the steadiest sign) and the deadline (-0.7, #1661's 0.8 us)
are the only ones with a consistent sign. **Putnami without any default still
costs about 4 to 5 us more than Elysia with all of them**: the health gap is
in the core pipeline (context, chain, dispatch, endpoint, response), not in
the middlewares. The profiles (section 5) attribute it.

## 5. CPU profiles (main + both branches)

`prof.sh` (`bun --cpu-prof`, 200 us sampling, 5 s warm-up + 20 s of wrk), then
`groups.py`/`attribute.py`/`compare.py` on the `.cpuprofile` (all in this
directory, profiles under `profiles/`). The profiler inflates CPU (by 1.0x to
1.75x), so every us below is the sample share times the **unprofiled** median
of section 3 (Putnami health 17.0, Elysia health 12.7, Putnami get 65.2, raw
get 43.9, Elysia get 41.9). A sample counts for the nearest framework frame
above it (natives it calls count for it: `new Response`, `JSON.stringify`,
promises), except Bun.SQL's own code, which is its own group. Profiles taken
at load1 0.8 to 2.0, except Putnami health (6.35: shares only).

### Health: Putnami (17.0) against Elysia (12.7)

| group | Putnami | Elysia | note |
|---|---:|---:|---|
| response build (`HttpResponse.json`, `get()`, `new Response`, `JSON.stringify`) | 4.91 | ~5.0 | Elysia's is inside its handler/`protect.ts`: `Response` 4.04 + `stringify` 1.00. **At parity**; in isolation Putnami's header template builds a `Response` in 0.30 us against 0.68 to 0.90 us with a plain object (`micro/response.ts`) |
| native route entry (`http.plugin.ts:1032`: `req.url`, `req.params`), `serveRequest`, `executeRequest`, chain, `finishRequest` | 5.23 | ~4.3 | Elysia: Bun route + compiled handler (`map` 2.86 + `handle` 1.44). Of Putnami's, 2.66 is the native entry (Bun's request materialization lands in that frame) and 2.57 the generic pipeline |
| context build (`HttpContext` constructor 1.07, `UrlScanner`, `secured`, `ownerOf`, `requestOf`) | 1.84 | 0 | **#1662** |
| dispatcher and endpoint answer (`runHandler`, `handle`, `acceptScore` 0.24 with one candidate, `endpoint-helpers` answer) | 1.71 | 0 | new lead: single-candidate fast path |
| deadline (`ServedRequest`, `answer`, `enqueue`/`remove`) | 1.45 | ~1.9 (its hand-written one) | **#1661**; ablation: 0.7 |
| logger middleware (closure, `deferHttpLogContext`, `logger.debug()` with debug off) | 0.79 | 0 | ablation 0.8: new lead |
| security headers | 0.57 | in `protect.ts` | ablation 0.5 |
| AsyncLocalStorage context | 0.17 | 0 | not a lead (the epic already measured it under 1 us) |

### Get: the database layer, Putnami Repository (65.2) against Putnami raw Bun.SQL (43.9)

| group | repo | raw | repo - raw | lead |
|---|---:|---:|---:|---|
| Bun.SQL's own JS and natives (`template`, `identifier`, `createQuery`, `Query`, `run`, pool) | 14.73 | 1.77 | +12.96 | gross; the event loop group below drops by 7.45 because `execute()` now runs synchronously inside `runOn`: **net about +5.5**, from nested fragments and `sql(identifier)` the driver normalizes on every call |
| statement build in Putnami (`CapturedQuery.build`/`runOn` replay, `buildWhere`, five nested fragments) | 4.80 | 0 | +4.80 | new: statement cache (below) |
| client, pool lease (`dispatch`, `leaseMember`, `useTxConnection`, `database()`) | 3.40 | 0 | +3.39 | A/B `tx - raw` = 4.0 |
| row copies (`clientResult` camelCase copy, then `toEntity` with `Object.entries`, `DateIso` `toISOString`) | 2.97 | 0 | +2.97 | lead 3 (row copy), two copies per row |
| Repository `observe` (two `performance.now`, `recordQuery`, `databaseLogger`), `find`/`findOne`/`get` frames | 2.68 | 0 | +2.68 | new |
| `abortableQuery` hooks (`hookedQuery`, `onAbort` -> first one reads `req.signal` and `addEventListener`, `Promise.race`) | 2.67 | 0 | +2.67 | new; A/B `txa - tx` = 4.6 (its promises and continuations land in other groups) |
| HTTP pipeline (all `http:` groups) | 8.17 | 8.73 | -0.56 | same on both |
| event loop, sockets, GC | 24.21 | 31.66 | -7.45 | see the Bun.SQL row |

Putnami raw (43.9) against Elysia (41.9) on get: the HTTP pipeline groups sum
to 9.3 us (response 3.47, entry/chain 1.22, dispatcher 1.20, deadline 0.95,
security headers 0.80, validation 0.66, context 0.43, logger 0.41) against
Elysia's handler, validation and protections at about 5.6 (its `subject code`
group, which includes its Response); Bun.SQL and the event loop are 2.8 us
cheaper in the Putnami raw run (noise of the method). Net +2.0.

## 6. Attribution: where the gap is, mapped to the leads

Gaps to Elysia, main + both branches, section 3 medians: **health +4.3 us
(17.0 vs 12.7), get +23.3 us (65.2 vs 41.9)**. Targets (< 5 %): health
<= 13.3 (cut 3.7), get <= 44.0 (cut 21.2).

| # | where | health us | get us | measured by | lead |
|---|---|---:|---:|---|---|
| 1 | Repository statement: nested `sql` fragments and `sql(identifier)` built per call, replayed by `CapturedQuery.build`, normalized by Bun.SQL each time | 0 | ~10 (4.8 JS + ~5.5 driver) | A/B + profile | **new**: build each Repository statement once per (table, operation, filter keys, options shape) as fixed text and run it with values only |
| 2 | `abortableQuery` per statement | 0 | **4.6** | A/B `txa - tx` | **new**: one abort hook per request instead of one per statement; no `Promise.race` and `new Promise` per statement; do not read `req.signal` (it makes Bun build an `AbortSignal`) unless a listener is needed |
| 3 | `@putnami/database` client and pool over Bun.SQL's pool | 0 | **4.0** | A/B `tx - raw` | partly lead 3 (the camelCase copy, ~1.5 of it); the rest is `CapturedQuery` + lease/release per statement |
| 4 | Repository `observe` (timing, metrics, logging) and its frames | 0 | ~2.7 | profile | **new**: skip the clock and the record when no metrics exporter and no slow-query threshold are set; fewer async frames between `get` and the statement (#1659 measured 0.9) |
| 5 | second row copy (`toEntity` with `Object.entries` per row, after the client's camelCase copy) | 0 | ~1.5 (of the 3.0 copies) | profile | lead 3: one copy per row, with a precompiled column map, or none when the names already match |
| 6 | context build | 1.8 | ~0.4 | profile (ablation n/a) | **#1662** (the comment's 1 to 2 us: confirmed) |
| 7 | dispatcher and endpoint answer: `Accept` parsed and scored at the native entry for a route with one candidate (`requestAcceptOf` + `acceptScore`), then two promise hops between the handler and the response (the endpoint's `answer` `then`, and `runHandler`'s `then` + `wrapResponse` + `flushContext`, with an `answerError` closure per call) | 1.7 | ~1.2 | profile | **new**: one-candidate fast path: no negotiation when the route has one handler, one continuation from handler value to `HttpResponse` |
| 8 | generic pipeline (`serveRequest`, `executeRequest`, chain, `finishRequest`, scope close) | ~2.6 | ~1.2 | profile | lead 6 (compiled per-route handler) |
| 9 | logger middleware with debug off | 0.8 | 0.4 | ablation + profile | **new**: when `debug` is off, no per-request closure/`then`; only the error path needs the request |
| 10 | request deadline | 0.7 (ablation) | ~0.9 | ablation + profile | **#1661** (the comment's 0.5: confirmed at 0.7) |
| 11 | security headers | 0.5 | 0.8 | ablation + profile | not a lead (Elysia pays the same by hand) |
| 12 | parameter validation | 0 | ~0.6 | profile | lead 4 (compiled validation): at most 0.6 on get |
| 13 | JSON Response | 0 | 0 | profile + micro | lead 5: **done**; Putnami's header template is already cheaper than a plain-object `Response` |

The database layer is 21.3 of the 23.3 us get gap (items 1 to 5); the epic's
"about 5 us" estimate for it was low by 4x. The HTTP side on get (Putnami
with raw Bun.SQL) is already 2.0 us (4.8 %) over Elysia.

## 7. What to cut to reach < 5 % of Elysia, ranked by gain

| rank | cut | gain | routes | confidence |
|---|---|---|---|---|
| 1 | Repository statement cache: fixed text per (table, operation, shape), values only; no nested fragments or identifiers per call | up to ~10 us get (repo -> txa is 12.7, minus items 4 and 5) | all Repository reads/writes | good: A/B 65.2 -> 52.5 with the same statement as text |
| 2 | `abortableQuery`: one request-level hook, no per-statement promise/race, no `req.signal` read | ~4.6 us get | every statement | good: A/B txa 52.5 -> tx 47.9 |
| 3 | Client: hand Bun.SQL's rows out without the camelCase copy when names match (`AS "createdAt"` or snake-free names), and cut the `CapturedQuery` replay for a plain statement | up to ~4.0 us get | every statement | medium: A/B tx 47.9 -> raw 43.9 is the ceiling |
| 4 | Repository `observe` and `toEntity`: no clock/record without an exporter or threshold, one precompiled row map | ~2 to 4 us get | Repository | medium: profile only, measure first |
| 5 | #1662 context on demand | ~1.8 us health | all | medium: profile |
| 6 | One-candidate fast path: no `Accept` scoring for a one-handler route, one continuation from the handler's value to the `HttpResponse` | ~1 to 1.7 us health | all | low: profile only |
| 7 | Logger with debug off: no per-request work | ~0.8 us health | all | good: ablation |
| 8 | #1661 deadline without a promise per request | ~0.7 us health | all | good: ablation |
| 9 | Lead 6, compiled per-route handler | 1 to 2.6 us | all | low: structural |
| 10 | Lead 4, compiled validation | <= 0.6 us | params/body | low |
| - | Lead 5, native JSON Response | 0 | - | done (measured) |

Reading: get reaches the target only with the database cuts 1 to 3 (they
bring the A/B variants from 65.2 to 43.9, +4.8 % over Elysia's 41.9) plus
part of the HTTP cuts (5 to 8 also apply to get, about 2 to 3 us). Health
reaches it with 5 + 6 + 7 + 8 (about 5 us for a needed 3.7). No lead on the
HTTP side alone moves get to the target: the database layer must move first.

## 8. List (20 rows) and insert (main + both branches, no `--smol`)

Run `results/list-insert`, 2026-11-28 11:40 UTC. **Host load 2.8 to 6.5**
(other work on the machine during the run): read these as relative only.

| subject | route | r1 | r2 | r3 | median | load1 (min-max) | host busy % |
|---|---|---|---|---|---|---|---|
| Putnami (repo) | list | 134.2 | 134.8 | 137.8 | **134.8** | 2.90-6.55 | 45 |
| Putnami raw Bun.SQL | list | 87.3 | 79.1 | 91.2 | **87.3** | 3.09-5.08 | 50 |
| Elysia | list | 90.6 | 93.2 | 92.1 | **92.1** | 3.67-4.18 | 49 |
| plain Bun | list | 74.3 | 66.2 | 77.6 | **74.3** | 3.37-3.96 | 50 |
| Putnami (repo) | insert | 123.9 | 116.7 | 126.4 | **123.9** | 3.42-4.56 | 48 |
| Putnami raw Bun.SQL | insert | 59.2 | 53.0 | 54.9 | **54.9** | 2.81-3.76 | 62 |
| Elysia | insert | 74.5 | 65.5 | 70.9 | **70.9** | 2.80-3.32 | 58 |
| plain Bun | insert | 40.1 | 42.7 | 40.4 | **40.4** | 2.90-3.46 | 69 |

Reading: the same shape as get, larger. With raw Bun.SQL, Putnami's pipeline
is at or under Elysia on list (87.3 vs 92.1) and insert (54.9 vs 70.9: Elysia's
POST path, its `onRequest` guard, body parse and TypeBox check, costs it 30 us over plain Bun here). Everything
over Elysia is the database layer: **list +47.5 us over raw** (two row copies
of 20 rows, `toEntity` with `Object.entries` per row, the `ORDER BY` fragment
built from the `orderBy` string), **insert +69 us over raw**
(`Repository.save`: `validateEntity` on top of the endpoint's body check,
`extractKeys`, `Object.entries` twice, `sql(row, ...columns)`, an
`upsertSetClause` that nests one fragment per column, `identList`, then the
same replay and copies). Cut 1 of section 7 (statement cache) and cut 4
(observe, `toEntity`) matter most on these two routes; the save path also
validates the entity a second time after the endpoint did (new lead: skip the
Repository's validation when the values come from a validated body, or
compile it once per table).
