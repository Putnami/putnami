# @putnami/web — SSR performance benchmarks

In-process SSR **render-to-completion** throughput for the real `pageRenderer`.
The harness makes render cost measurable instead of relying on a qualitative
"performance must not regress" claim.

It drives the actual `pageRenderer` (streaming SSR + hydration-data serialization)
and drains the full document body each iteration — no network, no server, so the
numbers are stable enough to gate CI against a committed baseline. Mirrors the
harness in `application/bench/bench.ts`.

## Run

```bash
cd typescript/framework/web
bun run bench/bench.ts
```

## Baseline

Indicative figures on **Bun 1.4.0** (linux/x86_64 dev container, Intel Xeon
@ 2.10 GHz, 4 vCPU; median of 3 runs) — **render cost is machine-dependent**, so
regression detection must compare runs on the *same* host (that is what the CI
gate will do). Treat the ratios between rows as the stable signal, not the
absolute numbers.

| Benchmark                 | ops/s  | avg      | p99      |
| ------------------------- | ------ | -------- | -------- |
| SSR render: minimal page  | 11,322 | 88 µs    | 246 µs   |
| SSR render: 100-row table | 1,261  | 793 µs   | 1.14 ms  |
| SSR render: 1000-row table| 158    | 6.32 ms  | 6.71 ms  |

### History

Previous captures, kept for the row *ratios* only — each block was measured on a
different host, so absolute numbers are not comparable across blocks:

Bun 1.3.13, darwin/arm64 dev host:

| Benchmark                 | ops/s  | avg      | p99      |
| ------------------------- | ------ | -------- | -------- |
| SSR render: minimal page  | 11,765 | 85 µs    | 614 µs   |
| SSR render: 100-row table | 1,410  | 709 µs   | 968 µs   |
| SSR render: 1000-row table| 160    | 6.24 ms  | 6.83 ms  |

The minimal : 100-row : 1000-row ops/s ratios are ~72 : 8 : 1 on both captures,
so the render-cost profile is unchanged across the Bun 1.3.13 → 1.4.0 migration.
Bun 1.4's headline runtime wins (regex, URL parsing) don't show up here because
this workload is dominated by React's render loop.

## Bun 1.4 build-flag A/B (`build-ab.ts`)

Bun 1.4 shipped two build-time features this bench measures before either is
adopted: `--react-compiler` (Meta's auto-memoization compiler, in Rust) and
`optimizeImports` (barrel-import optimization). Decisions come from the bench,
not from release notes — `bench/build-ab.ts` rebuilds a real client entrypoint
with the exact flags `buildEntrypoint` (src/ssr/generator/build.ts) uses today,
once per arm:

```bash
# Requires the sample built first: ./putnamiw build --projects @example/03-web
cd typescript/framework/web
bun run bench/build-ab.ts                    # hydrate entry of 03-web
bun run bench/build-ab.ts --entry <path>     # any other entrypoint
```

Figures below: Bun 1.4.0, same container as the baseline above, median of 7
builds per arm. Bundle bytes are deterministic; build times are
machine-dependent (same-host comparison only).

### `--react-compiler`

| Measure                            | off       | on        | delta   |
| ---------------------------------- | --------- | --------- | ------- |
| hydrate bundle (03-web, gzip)      | 99,980 B  | 102,171 B | +2.2 %  |
| islands bundle (03-web, gzip)      | 76,872 B  | 77,518 B  | +0.8 %  |
| hydrate build time (median)        | 23.5 ms   | 25.9 ms   | +10 %   |
| SSR render throughput (all 3 rows) | —         | —         | ±4 % (noise) |

SSR throughput was A/B'd by bundling `bench.ts` with `--target=bun --production`
off vs on and running both bundles twice: every delta was within run-to-run
noise, in both directions. That is the expected result — for `bun`/`node`
targets the compiler uses its `ssr` output mode, which skips memoization
(`useMemoCache`) entirely. Hydration cost remains unmeasured (no browser/DOM
harness in the repo yet; see Coverage).

**Verdict: adopt as an opt-in build param** (same pattern as `metafile`),
default **off**. The transform is SSR-neutral and build-time-cheap, so
apps with re-render-heavy interactive components can opt in — but on the
reference app it only costs (+2.2 % client JS against the per-route budget
gate, +10 % client build time) with no measurable win, so there is no data to
justify default-on. The extension change that would wire this into the build
config ships separately from this bench.

### `optimizeImports`

The 03-web hydrate entry is genuinely barrel-heavy — it pulls 9 names through
the `@putnami/web` barrel, which re-exports ~28 modules — so the arm passes
`optimizeImports: ['@putnami/web', '@putnami/application', '@putnami/runtime',
'@putnami/utils']`.

| Measure                       | off       | on        | delta        |
| ----------------------------- | --------- | --------- | ------------ |
| hydrate bundle (03-web, gzip) | 99,980 B  | 99,980 B  | byte-identical |
| islands bundle (03-web, gzip) | 76,872 B  | 76,872 B  | byte-identical |
| hydrate build time (median)   | 23.5 ms   | 22.4 ms   | −5 % (noise) |

**Verdict: reject for now, with numbers.** The bundler already tree-shakes the
barrels — output is byte-identical — and the promised win (skipping the parse
of unused re-exports) is invisible on a client build that completes in ~23 ms.
Two notes for whenever build times grow enough to revisit: `optimizeImports` is
a `Bun.build()` API option with no CLI flag, and `buildEntrypoint` currently
shells out to the CLI; Bun also enables the optimization automatically for any
package with `"sideEffects": false` in its `package.json`, which the workspace
packages don't set.

## Client-JS budget gate

The render bench above measures **time**, which is machine-dependent — so it stays
a local/same-host tool, not a hard CI gate. The enforceable CI gate is on the
**per-route client-JS budget**, which the web build emits into the determinism
manifest (`.gen/putnami-web-manifest.json`). Byte sizes are deterministic build
outputs, so a committed baseline plus a growth tolerance is a stable,
machine-independent regression gate.

The gate lives in `bin/manifest-budget.ts` (exposed as the
`putnami-web-manifest-budget` bin) and is wired into the `ts-build` CI job. It
compares a freshly built manifest against a committed baseline and fails when any
route's client JS grows past the threshold or breaches an absolute cap.

```bash
# Compare a build's manifest against the committed baseline (the 03-web sample).
bun typescript/framework/web/bin/manifest-budget.ts \
  typescript/samples/03-web/.gen/putnami-web-manifest.json \
  --baseline typescript/samples/03-web/web-manifest.baseline.json \
  --max-growth-percent 10 \
  --max-bytes 153600
```

| Flag | Meaning |
| --- | --- |
| `<current.json>` | Path to the freshly built manifest. Absent ⇒ gate is **skipped** (web not rebuilt). |
| `--baseline <path>` | Committed baseline to compare against (regression check). |
| `--max-growth-percent N` | Allowed per-route growth vs baseline before failing. Decreases always pass. |
| `--max-bytes N` | Absolute per-route ceiling, in bytes — a backstop independent of the baseline. |

**Exit codes:** `0` within budget (or skipped), `1` budget exceeded, `2` usage error.

### Refreshing the baseline

When a client-JS change is intentional, rebuild the sample and re-commit the
baseline so the gate tracks the new floor:

```bash
./putnamiw build --projects @example/03-web
cp typescript/samples/03-web/.gen/putnami-web-manifest.json \
   typescript/samples/03-web/web-manifest.baseline.json
```

## Coverage

Done:
- [x] SSR render-throughput bench (`bench.ts`) + captured baseline (above)
- [x] Per-route client-JS budget gate (`bin/manifest-budget.ts`) + committed baseline
- [x] CI regression gate with a configurable threshold (`ts-build` job, mirroring the coverage gate)
- [x] Bun 1.4 re-baseline + `--react-compiler` / `optimizeImports` A/B
  (`build-ab.ts`, verdicts above)

Not yet measured:
- [ ] Hydration-cost measurement (full hydration vs static + island) — needs a
  browser/DOM harness not yet present in the repo; also blocks measuring the
  React Compiler's client-side re-render win
