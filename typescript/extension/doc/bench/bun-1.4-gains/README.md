# Bun 1.4.1 to 1.4.5 gains — measured

What the TypeScript stack gets from Bun 1.4.1 to 1.4.5 without a code change, and
whether two new flags, `--bytecode-order` and `bun test --isolate`, are worth
adopting.

This is a **measurement**, not an adoption. Nothing in the extension, framework
or samples changed. "Before" is Bun 1.4.0, the last release before 1.4.1;
"after" is Bun 1.4.5, the version `package.json` pins. A flag is measured on
1.4.5 with the flag off, then on.

## Machine and method

- Linux x86_64 cloud container, Intel Xeon @ 2.10 GHz, 4 vCPUs, 15 GiB RAM,
  kernel 6.18. PostgreSQL 16.15 on the same host, over TCP on 127.0.0.1.
- Both Bun releases come from the `@oven/bun-linux-x64` npm package.
- Runs of the two sides are interleaved, so drift hits both equally. Every cell
  reads `median (sd; min–max)` over the runs its section names.
- Raw results are JSON lines under `$BENCH_WORK`; nothing is written to the tree.

## Results (2026-11-22)

### SQL: CPU per point SELECT

`SELECT id, name, score FROM point WHERE id = $1` on a 1,000-row table, one
connection, sequential, 2,000 warm-up queries, then 20,000 measured. CPU is the
process's user + system time (`process.cpuUsage()`). 10 runs per row.

| Driver, µs per query | Bun 1.4.0 | Bun 1.4.5 | Change |
| --- | --- | --- | --- |
| `Bun.SQL`, CPU | 63.7 (4.05; 58.5–73.3) | 34.2 (1.99; 31.8–39.0) | **−46 %** |
| postgres.js 3.4.8, CPU | 64.6 (2.33; 61.9–68.0) | 48.0 (2.73; 43.9–51.7) | **−26 %** |
| `Bun.SQL`, wall | 69.8 (3.97; 65.5–79.3) | 58.9 (2.77; 54.6–62.9) | −16 % |
| postgres.js, wall | 70.3 (2.54; 65.7–73.8) | 67.0 (2.99; 61.4–71.7) | −5 % |

`@putnami/database` runs on postgres.js, not `Bun.SQL`
(`typescript/framework/database/src/sql-client.ts`), so the framework gets the
second row: 26 % less CPU per query. The 1.4.4 notes' "up to 48 % fewer
instructions" holds for `Bun.SQL` itself (−46 % CPU).

### Idle CPU of a framework service

`typescript/samples/02-rest-api`, started as `bun <entry>` on its bundled serve
entry (not `bun run --no-install --smol`, as `putnami serve` does), idle for
120 s after its ready line plus a 2 s settle. CPU from `/proc/<pid>/stat`, whose
10 ms ticks make every value a multiple of 10. 5 runs per version, the two
versions side by side.

| | Bun 1.4.0 | Bun 1.4.5 | Change |
| --- | --- | --- | --- |
| CPU over 120 s idle | 80 ms (8.9; 60–80) | 60 ms (4.5; 50–60) | −25 % |

Both are below 0.07 % of one core, and the difference is two clock ticks per
run. A Putnami service was not burning idle CPU to begin with, so the 1.4.3
claim (−67 % to −89 %) has nothing to act on here.

### `bun install`, isolated linker

The root `bunfig.toml` (`linker = "isolated"`) on a fresh worktree of the
repository, `--frozen-lockfile`. Cold: empty cache, no `node_modules`. Warm:
cache kept, every `node_modules` removed before the clock starts. No-op:
everything in place. 10 runs per version.

| | Bun 1.4.0 | Bun 1.4.5 |
| --- | --- | --- |
| Cold | 4210 ms (504; 3450–4560) | 4190 ms (227; 3510–4290) |
| Warm | 290 ms (72; 109–360) | 288 ms (56; 200–353) |
| No-op | 19 ms (3.6; 16–26) | 10 ms (0.7; 10–12) |

Cold is network-bound and unchanged, warm is unchanged; only the no-op check
got faster.

### `--bytecode-order` on a compiled service

The sample compiled as `typescript/extension/internal/build/compile.go` does
(`--compile --bytecode --format=esm`, entry `.gen/src/serve.bundled.ts`). One
run of that executable with `BUN_BYTECODE_ORDER_OUT` writes the order file;
the service is then recompiled with `--bytecode-order=<file>`. Start-up is the
time from spawn to the workload's ready line. Cold drops the page cache
(`/proc/sys/vm/drop_caches`) before each start. 30 runs per row.

| Executable | Warm start | Cold start |
| --- | --- | --- |
| no bytecode (reference) | 64.0 ms (8.4; 57.4–88.9) | 123 ms (13.1; 103–164) |
| `--bytecode` (today) | 23.6 ms (3.3; 20.7–33.3) | 83.1 ms (10.0; 67.4–106) |
| `--bytecode --bytecode-order` | 23.1 ms (2.6; 20.7–31.0) | 77.8 ms (9.0; 62.1–96.5) |

A second batch of 15 cold starts gave 80.9 ms (13.9; 69.9–122) against
72.5 ms (11.4; 62.4–99.1). The order file was byte-identical across two
training runs, and two compiles with the same order file were byte-identical.

### `bun test --isolate`

`bun test` with and without `--isolate`, in the two framework packages with the
most test files. Peak RSS comes from the kernel's rusage of the `bun test`
process, wall time from the harness clock around it. 5 runs per row; every run
passed every test. `putnami test` also passes `--coverage` and a junit
reporter; these runs did not.

| Package | Peak RSS off | Peak RSS on | Wall off | Wall on |
| --- | --- | --- | --- | --- |
| `application`, 168 files | 583 MB (29; 528–585) | 500 MB (55; 437–591) | 19.4 s (0.26; 18.9–19.4) | 22.6 s (0.49; 22.2–23.5) |
| `web`, 83 files | 289 MB (0.4; 288–289) | 296 MB (1.7; 293–297) | 5.47 s (0.06; 5.40–5.55) | 7.34 s (0.13; 7.15–7.50) |

## Verdicts

- **`--bytecode-order`: not adopted.** The gain is real but small: 5 to 8 ms
  cold (about 2 to 3 standard errors over 30 runs) and 0.5 ms warm, on a service
  that already starts in 23 ms warm and 83 ms cold. That is not worth a training
  run of the executable in every compile. The 1.01 s to 0.53 s case in the notes
  is a large CLI; a Putnami service bundle is not.
- **`bun test --isolate`: not adopted.** Wall time grows 17 % (`application`)
  and 34 % (`web`); memory falls 14 % on `application` with overlapping ranges
  and rises 2 % on `web`. These suites do not leak across files the way the
  notes' 1,591 MB case does. A project that needs isolation already gets it
  from `parallel`, which implies `--isolate`.

## Run

Linux x64 only: the harness reads `/proc`, compiles for `bun-linux-x64`, and
drops the page cache through `/proc/sys/vm/drop_caches`. It needs `psql` for
the SQL step and `git` for the install step.

```bash
# 1. Build the sample so .gen/src/serve.bundled.ts exists.
./putnamiw build --projects @example/02-rest-api

# 2. Run every step, or one of sql|idle|startup|isolate|install.
BENCH_BUN_BEFORE=/path/to/bun-1.4.0 BENCH_BUN_AFTER=/path/to/bun-1.4.5 \
  typescript/extension/doc/bench/bun-1.4-gains/bench.sh all
```

| Environment | Default | Meaning |
| --- | --- | --- |
| `BENCH_BUN_BEFORE` | required | Bun before 1.4.1 |
| `BENCH_BUN_AFTER` | required | Bun 1.4.5 or later; also runs `measure.ts` and every flag row |
| `BENCH_WORK` | `$TMPDIR/putnami-bun-1.4-bench` | Raw results and executables; the install worktree is removed on exit |
| `BENCH_SAMPLE` | `typescript/samples/02-rest-api` | Service for the idle and start-up steps |
| `DATABASE_URL` | `postgres://postgres:postgres@127.0.0.1:5432/bench` | Database the SQL step seeds and queries |
| `BENCH_POSTGRES_JS` | postgres.js of `@putnami/database` | Entry file of the postgres.js the SQL step loads |
| `BENCH_RUNS` | `10` | SQL runs per version and driver |
| `BENCH_IDLE_RUNS` | `5` | Idle runs per version |
| `BENCH_WARM_STARTS`, `BENCH_COLD_STARTS` | `30` | Start-ups per executable; cold needs root to drop the page cache |
| `BENCH_TEST_RUNS`, `BENCH_TEST_PROJECTS` | `5`, `application web` | `bun test` runs per flag, and the framework packages to test |
| `BENCH_INSTALL_RUNS` | `5` | Install runs per version |
