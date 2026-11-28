# Full benchmark harness (epic Putnami/putnami#1671)

CPU time per request of the same tasks API on Putnami (any worktree), Elysia
and plain Bun, with the same Bun.SQL statements and the same protections.
Everything here lives outside the repository; nothing in it is committed.

## What is measured

- `subjects/putnami/`: a tasks API written the way the `06-database` sample is
  (`application().use(http()).use(logger()).use(sql()).use(platform()).use(api())`,
  `Table` + `Repository`, `endpoint().params/query/body` validation). It is
  copied to `run/<label>/` and its `node_modules/@putnami/*` are symlinked to
  `<worktree>/typescript/framework/*`, so any branch is measured by passing its
  worktree (the worktree needs `./putnamiw install` once).
  Env switches, after `@` in the `--putnami` argument:
  - `BENCH_DB=repo` (default: `Repository`), `tx` (`useTxConnection` + the same
    SQL text, no Repository), `txa` (`tx` + `abortableQuery`, as the Repository
    runs each statement), `raw` (Bun.SQL's own pool, no `@putnami/database`,
    no `sql()` plugin);
  - `BENCH_OFF=logger+platform+securityHeaders+originGuard+deadline+httpMethods` (`+`-separated)
    turns those defaults off one by one (ablation).
- `subjects/elysia/` (Elysia 1.4.32) and `subjects/bun/` (plain `Bun.serve`
  routes): the same routes with Putnami's protections hand-written in
  `subjects/common/protect.ts` (security headers with Putnami's JSON and full
  sets, origin guard, 1 MiB body limit, one-sweeper 30 s deadline, HEAD,
  OPTIONS, 405 with Allow) and the same statements in `subjects/common/db.ts`
  (Bun.SQL pool of 10, as Putnami's `poolSize` default).
- Aligned subjects: one `toTask` per row that calls `toISOString()` on the
  driver's `Date` (the Putnami Repository variant returns the entity, whose
  `DateIso` column already did the same); the title rule is at most 200 UTF-16
  units everywhere (`MaxLength(200)`, TypeBox `maxLength`, `title.length`).
- `parity.py <port> <port>...`: 22 requests (HEAD, OPTIONS, 405, 400, 404,
  413, 403 from the origin guard, the UTF-16 title edge) compared on status,
  Allow, Content-Type, security headers and the JSON body of successes. A run
  refuses to measure when the subjects differ (`--no-parity` to override).
- Routes: `health` (`GET /health`, no database), `get` (`GET /tasks/<random
  one of 1000 ids>`), `list` (`GET /tasks?limit=20`), `insert` (`POST /tasks`,
  Repository upsert statement).
- Method: wrk 4.1.0 (Ubuntu package), 64 connections, 2 threads, a 5 s warm-up
  then 20 s per route, every round in rotated subject order. CPU per request =
  delta of utime+stime of the service process (`/proc/<pid>/stat`) divided by
  the requests wrk completed. Each row also records `load1` and the host's
  busy % during the 20 s window.
- `BUN_OPTIONS` is unset for the services (this session's shell exports
  `--smol`); `BENCH_BUN_OPTIONS=...` sets options for every subject, and
  `--putnami <wt>@BUN_OPTIONS=--smol` for one subject.
- Bun 1.4.5 (`packageManager` in the repository's `package.json`), in
  `bin/bun` (set `BENCH_BUN` to use another binary). `NODE_ENV=production`.
- PostgreSQL: its own cluster (`pg.sh`) on 127.0.0.1:55432, database `bench`,
  data in `/var/lib/postgresql/bench`, `fsync=off`. PostgreSQL 16.15: the
  PGDG repository (17) is not reachable from this machine. The table is reset
  to 1000 seeded rows at the start of each run.

## Commands

Both scripts take the machine lock (`../machine.lock`) for the whole run and
start the bench PostgreSQL if needed.

    # compare a branch with main, Elysia and Bun (3 rounds, 20 s, health and get)
    ./run.sh --putnami /home/user/wt-bench-main --putnami /home/user/wt-mybranch \
             --elysia --bun --routes health,get --rounds 3

    # all routes
    ./run.sh --putnami /home/user/wt-bench-main --elysia --routes health,get,list,insert

    # database layer A/B under Putnami's pipeline
    ./run.sh --putnami /home/user/wt-bench-main --putnami /home/user/wt-bench-main@BENCH_DB=tx \
             --putnami /home/user/wt-bench-main@BENCH_DB=raw --routes get --no-parity

    # ablation of one default on health (parity differs by design)
    ./run.sh --putnami /home/user/wt-bench-main --putnami /home/user/wt-bench-main@BENCH_OFF=logger \
             --routes health --no-parity

    # CPU profile (bun --cpu-prof, 200 us sampling) and top self-time functions
    ./prof.sh --putnami /home/user/wt-bench-main --route get

Profile analysis (the profiler inflates CPU; `--scale`/the `us` arguments
rescale to an unprofiled median from `run.sh`):

    python3 -I groups.py  <A.cpuprofile> <A requests> <A us> [<B.cpuprofile> <B requests> <B us>]
    python3 -I attribute.py <profile> <requests> --scale <us> [--file] [--top N]
    python3 -I compare.py <A> <A req> <A us> <B> <B req> <B us> [--owner] [--top N]
    python3 -I leaves.py  <profile> <requests> '<owner substring>' --scale <us>

The request count of a profile is the warm-up plus the measured run (the
`profile ...: N requests` line of `prof.sh`'s output). In-process
micro-benchmark: `flock ../machine.lock env -u BUN_OPTIONS bin/bun micro/response.ts`.

Options of `run.sh`: `--rounds N --duration S --warmup S --connections N
--threads N --out DIR --no-parity`. Output: `results/<stamp>/rows.jsonl`
(one row per measurement), `summary.md` (per-round us/request, median, load),
`parity.txt`, `meta.json` (Bun version, worktree heads), service logs.

A worktree for a branch:

    git -C /home/user/putnami worktree add --detach /home/user/wt-mybranch origin/<branch>
    (cd /home/user/wt-mybranch && flock <scratchpad>/machine.lock ./putnamiw install)

## Reading the numbers

The service is saturated by wrk (about 1.1 cores), so us/request is the cost
of a request at full load. Between rounds of one variant the full bench
spreads by 3 to 7 us on get and 1.5 to 2.5 us on health (measured today on
this shared host): a change under 5 us needs the mean of several rounds on
health, an in-process micro-benchmark, or an A/B variant in the same run,
then a full run to confirm. Compare within one run, never across runs. Absolute numbers on this host are higher than the epic's
first table on database routes (Firecracker VM: syscalls cost more); compare
subjects within one run.
