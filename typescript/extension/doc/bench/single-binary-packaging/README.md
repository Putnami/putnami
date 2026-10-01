# Single-binary packaging for TypeScript servers — measured

Does packaging a Putnami TypeScript server as one `bun build --compile`
executable (optionally with `--asset` asset embedding and `--bytecode`) beat the
`oven/bun:1.4-slim` + `bun run` deployment shape, and is it compatible with the
framework's runtime surfaces?

This is a **measurement**, not an adoption. Nothing in the extension, framework
or samples was changed to make a variant work; where something did not work, the
finding is recorded below instead of patched.

- **Subject:** `@example/08-real-time` (`typescript/samples/08-real-time`) — a
  WebSocket + SSE server with a static `public/` tree, so one subject covers the
  static-asset, native-route, WebSocket and streaming questions at once.
- **Baseline:** `oven/bun:1.4-slim` + `bun run`, the shape
  `typescript/framework/events/Dockerfile` ships and `putnami serve` reproduces
  in production (`bun run --no-install --smol <entry>`).
- **Compiled entrypoint:** `.gen/src/serve.bundled.ts` — the generated bundled
  serve entrypoint, which already uses **static** imports for the generated
  loaders precisely so "single-binary packagers include generated server
  modules" (`typescript/extension/internal/build/build.go`). It is the correct
  input to `bun build --compile`; `src/serve.ts` is not, because the plugin
  loaders would fall back to dynamic `import()` of `.gen/src/...` paths that do
  not exist inside an executable.

> **Already shipping.** `putnami package --docker` for TypeScript projects
> *already* assembles a distroless image around a `bun build --compile`
> executable with `.gen` copied in as a layer
> (`typescript/extension/internal/pkg/docker.go`). So the open question is not
> "should TS servers ship as a single binary" — they do — but **what the
> remaining bun-image path costs, and which extra compile flags are worth
> adopting**. The tables below answer both.

## Run

```bash
# 1. Build the sample through the normal pipeline so .gen exists.
./putnamiw build --projects @example/08-real-time

# 2. Run the bench (bun >= 1.4 is required: --asset and bytecode-ESM).
BENCH_BUN=/path/to/bun-1.4 typescript/extension/doc/bench/single-binary-packaging/bench.sh all
```

| Step | What it does |
| --- | --- |
| `bench.sh build` | Compiles every variant, records best-of-3 build wall time and artifact size |
| `bench.sh determinism` | Compiles each flag set twice and compares digests |
| `bench.sh process` | Bare-process cold start, RSS at idle / under load, HTTP + WebSocket load |
| `bench.sh docker` | Builds the image variants, records size, container cold start, container memory |
| `bench.sh compat` | Compatibility checklist — every row is an observed response |
| `bench.sh report` | Re-renders the markdown tables below from the recorded results |
| `bench.sh all` | All of the above |

| Environment | Default | Meaning |
| --- | --- | --- |
| `BENCH_BUN` | `bun` | bun binary used for every build and every `bun run` row |
| `BENCH_SAMPLE` | `typescript/samples/08-real-time` | Project under test |
| `BENCH_WORK` | `$TMPDIR/putnami-single-binary-bench` | Scratch root — artifacts and raw results (never in the tree) |
| `BENCH_BASE_PORT` | `45080` | First port; busy ports are skipped, not silently used |
| `BENCH_COLD_STARTS` | `10` | Cold-start repetitions per process variant |
| `BENCH_DOCKER_COLD_STARTS` | `5` | Cold-start repetitions per container variant |
| `BENCH_TARGET` | host-derived | bun `--target` for every compile (matches `buildCompileArgs`); docker phase requires `bun-linux-x64` |
| `BENCH_SHIPPED_BASE_IMAGE` | `docker.go` pin | Base image for the `cc-distroless-binary` ("ships today") row |
| `BENCH_SKIP_DOCKER` | unset | `1` skips every docker step |

## Methodology and caveats

- **Same-host comparable only.** Following the precedent in
  `typescript/framework/web/bench/README.md`: timings are machine-dependent, so
  **the ratios between rows are the stable signal**, not the absolute numbers.
  Everything below is one run on one ephemeral sandbox.
- **Host:** Linux 6.18.44 (Ubuntu 24.04), Intel Xeon @ 2.10 GHz, 4 vCPU, 16 GiB
  RAM, Docker 29.3.1 (containerd image store), Bun 1.4.0 (the workspace's
  `packageManager` pin). The bun executable itself is 77 MiB — that is the floor
  every compiled artifact carries.
- **Cold start** is the Cloud Run scale-from-zero proxy: wall time from process
  (or `docker run`) start to the **first successful HTTP response**, not to a
  log line. Process rows are the median of 10 fresh spawns on fresh ports;
  container rows are the median of 5 fresh containers.
- **Container cold start includes ~190 ms of `docker run` + containerd
  overhead** that every row pays equally. That column is reported separately so
  the workload's own boot can be read as the difference.
- **Memory** is reported twice because the two numbers mean different things:
  process rows use `VmRSS` of the whole process tree (includes file-backed pages
  of the mmapped executable), container rows use `docker stats` (cgroup
  accounting, which excludes most reclaimable page cache). A 79 MiB executable
  inflates RSS without inflating the number a container runtime bills or
  OOM-kills on.
- **The load phase is not a throughput claim.** It exists to make memory
  measurable under work: 4 000 static-file GETs at concurrency 32, then 25
  concurrent WebSocket chat clients sending 10 messages each (every message is
  broadcast to all 25, so ~6 600 frames come back). Client and server share the
  host and 4 vCPUs.

## Results

### Build time and artifact size

| Variant | Artifact | vs bundle | Build (best of 3) |
| --- | --- | --- | --- |
| `bun-bundle` (baseline `--target=bun`) | 0.5 MiB | — | 31 ms |
| `runtime-floor` (dependency-free script) | 78.7 MiB | 158× | 147 ms |
| `binary` (`--compile`) | 79.2 MiB | 159× | 175 ms |
| `binary-bytecode` | 81.7 MiB | 164× | 246 ms |
| `binary-asset` | 79.2 MiB | 159× | 172 ms |
| `binary-asset-bytecode` | 81.7 MiB | 164× | 549 ms |
| `binary-asset-bytecode-smol` | 81.7 MiB | 164× | 251 ms |
| `binary-asset-sourcemap` | 79.8 MiB | 161× | 197 ms |
| `binary-asset-bytecode-sourcemap` | 82.3 MiB | 166× | 302 ms |

Reading it:

- **The application is 0.5 MiB of a 79.2 MiB artifact.** The `runtime-floor` row
  compiles a dependency-free script the same way and lands at 78.7 MiB, so
  ~99.4 % of every compiled binary is the embedded bun runtime. Artifact size is
  therefore essentially constant across TS services — a leaner sample would not
  move it, which is why one subject is enough for this row.
- **`--asset` of the whole `.gen/public` tree costs 4 096 bytes** (83 055 816 →
  83 059 912) and ~0 ms of build time. Embedding is cheap; the reasons not to do
  it are semantic, not size (see findings).
- **`--bytecode` costs 2.5 MiB and ~70–100 ms of build time.** Build wall times
  are noisy on a shared 4-vCPU host (`binary-asset-bytecode` shows 549 ms
  against 251 ms for the same flags plus `--compile-exec-argv`); treat build time
  as "a few hundred ms either way", i.e. never the deciding factor.
- **`--sourcemap=inline` costs 0.6 MiB.**

### Build reproducibility (two compiles of identical inputs)

| Compile flags | Byte-identical | Differing bytes |
| --- | --- | --- |
| `--compile` | yes | 0 |
| `--compile --asset .gen/public` | yes | 0 |
| `--compile --sourcemap=inline` | yes | 0 |
| `--compile --bytecode --format=esm` | **no** | 3 138 |
| `--compile --asset --bytecode --format=esm` | **no** | 854 |

(The differing-byte count is itself unstable — the two `--bytecode` rows above
landed at 3 138 and 854 for the same kind of nondeterminism, and repeat runs of
`bench.sh determinism` move both numbers — which is the clearest statement of
the problem.)

This is the finding that constrains the verdict. `putnami package --docker`
content-addresses images (`oci.ContentHash`) so "identical inputs produce a
byte-identical image and the digest can travel across commits"
(`typescript/extension/internal/pkg/docker.go`). Plain `--compile` upholds that;
`--bytecode` does not — two compiles of the same source differ by ~1–3 KB, which
changes the layer digest, the image digest and every cache decision built on it.

One subtlety worth recording so a future run is not misread: bun embeds the
**output path** in the executable, so compiling to `srv-1` and `srv-2` differs by
exactly the bytes of the filename. The harness compiles into two directories
under the same basename to isolate real nondeterminism from that.

### Bare-process cold start and memory

| Variant | Cold start median | min | max | vs baseline | RSS idle | RSS peak under load |
| --- | --- | --- | --- | --- | --- | --- |
| `bun run` (baseline) | 100.5 ms | 93.9 ms | 149.1 ms | — | 49.6 MiB | 67.8 MiB |
| `bun run --smol` (baseline) | 101.1 ms | 98.3 ms | 115.0 ms | +1% | 49.6 MiB | 63.5 MiB |
| binary | 56.6 ms | 53.7 ms | 64.9 ms | **-44%** | 46.1 MiB | 63.5 MiB |
| binary + bytecode | 36.9 ms | 35.0 ms | 56.3 ms | **-63%** | 46.6 MiB | 65.8 MiB |
| binary + asset | 53.2 ms | 52.4 ms | 57.5 ms | -47% | 45.6 MiB | 62.8 MiB |
| binary + asset + bytecode | 35.0 ms | 33.3 ms | 37.2 ms | **-65%** | 46.8 MiB | 63.9 MiB |
| binary + asset + bytecode + smol | 37.1 ms | 35.0 ms | 41.8 ms | -63% | 46.4 MiB | 61.0 MiB |
| binary + asset + bytecode + sourcemap | 35.5 ms | 33.9 ms | 39.3 ms | -65% | 45.9 MiB | 62.9 MiB |

- **Compiling halves cold start** (100.5 → 56.6 ms). **Bytecode takes another
  35% off what is left** (56.6 → 36.9 ms; 35.0 ms with `--asset` as well).
  Together: **-65%**, and the *slowest* sample of every compiled variant
  (37–65 ms) is faster than the *fastest* baseline sample (94 ms).
- `--asset` is worth ~3 ms of cold start (one fewer directory of `open`/`stat`
  syscalls at route registration); noise-adjacent, not a reason on its own.
- **Memory barely moves.** Idle RSS 49.6 → 45.6–46.8 MiB (-6% to -8%). `--smol` is the
  only lever with a real memory effect and it acts on the *peak* rather than on
  idle: like-for-like it is 63.9 → 61.0 MiB (-4.5%) on the compiled variant and
  67.8 → 63.5 MiB (-6%) on the `bun run` baseline, at a ~2 ms cold-start cost.
- Sourcemaps are **free at runtime** (35.5 vs 35.0 ms — inside the noise band).

### Load phase (same run as the memory numbers)

| Variant | HTTP ok | HTTP errors | HTTP req/s | WS frames received | WS errors |
| --- | --- | --- | --- | --- | --- |
| `bun run` (baseline) | 4000 | 0 | 17 953 | 6 603 | 0 |
| `bun run --smol` (baseline) | 4000 | 0 | 17 780 | 6 597 | 0 |
| binary | 4000 | 0 | 18 441 | 6 590 | 0 |
| binary + bytecode | 4000 | 0 | 18 461 | 6 594 | 0 |
| binary + asset | 4000 | 0 | 30 260 | 6 593 | 0 |
| binary + asset + bytecode | 4000 | 0 | 27 928 | 6 596 | 0 |
| binary + asset + bytecode + smol | 4000 | 0 | 28 105 | 6 596 | 0 |
| binary + asset + bytecode + sourcemap | 4000 | 0 | 26 097 | 6 596 | 0 |

Zero errors everywhere, and WebSocket fan-out is identical across variants
(6 590–6 603 frames) — the compiled binary is not dropping work. The one real
signal is that **serving a static file out of the embedded virtual FS is ~1.5×
faster than out of the on-disk `.gen/public`** (18 k → 26–30 k req/s, consistent
across all three embedded rows). Same-host client, static route only: this is a
"no syscalls per read" effect, not a framework throughput claim.

### Image size, container cold start, container idle memory

| Image | Pull (compressed) | vs baseline | Unpacked | Cold start min / median | `docker run` overhead (median) | Idle memory |
| --- | --- | --- | --- | --- | --- | --- |
| `bun-slim` (baseline) | 65.5 MiB | — | 165.5 MiB | 268 ms / 276 ms | 192 ms | 11.94 MiB |
| `cc-distroless-binary` (ships today) | 44.6 MiB | -32% | 105.9 MiB | 242 ms / 254 ms | 197 ms | 10.43 MiB |
| `distroless-binary` (`base` not `cc`) | 43.7 MiB | -33% | 103.0 MiB | 232 ms / 237 ms | 185 ms | 10.40 MiB |
| `distroless-asset` (embedded + bytecode) | 44.4 MiB | -32% | 105.4 MiB | 221 ms / 225 ms | 192 ms | 10.17 MiB |
| `scratch-asset` (scratch + glibc) | 38.2 MiB | **-42%** | 85.2 MiB | 214 ms / 241 ms | 203 ms | 9.92 MiB |

Subtracting the shared `docker run` overhead gives the in-container boot:
**84 ms** (bun-slim) → **52–57 ms** (compiled binary) → **33 ms**
(compiled + bytecode) — the same ratios as the bare-process table, which is the
cross-check that the container numbers are not measuring the daemon.

Size reporting note: with the containerd image store `docker image inspect
.Size` is the **compressed** (pull) size, and `docker images` SIZE over-reports
(it showed 129 MB for the scratch image). The unpacked column is the sum of
`docker history` layer sizes, cross-checked against `docker export | wc -c` on
an equivalent build: 89 203 200 vs 89 300 992 bytes, i.e. within tar/metadata
overhead, while `docker images` claimed 129 MB for the same image.

### Compatibility checklist

| Check | Works | Observed |
| --- | --- | --- |
| Static routes (compiled binary, `--asset` embedded) | yes | `GET / /index /index.html` 200, conditional GET 304; gzip body 5143B; ETag `"8952bae0669988eb"`; HEAD Content-Length 1690, **Last-Modified `Thu, 01 Jan 1970 00:00:00 GMT`** |
| Static routes (compiled binary, assets on disk) | yes | same, Last-Modified `Sat, 22 Aug 2026 22:16:13 GMT` |
| Static routes (`bun run` bundle, baseline) | yes | same, Last-Modified `Sat, 22 Aug 2026 22:16:13 GMT` |
| Embedded assets **without** `PUTNAMI_ASSETS_DIR` | conditional | `GET /index.html` → 500; embedding alone does not redirect StaticPlugin's file reads |
| WebSocket (`/chat`, typed `Stream` endpoint) | yes | frames received: `join,users,message` — join, presence and broadcast all round-trip |
| SSE (`/notifications`, typed `Stream` endpoint) | yes | 200 `text/event-stream`; first frame `data: {"id":0,"message":"Connected",...}` |
| `--smol` via `--compile-exec-argv=--smol` | yes | boots and serves; measurable effect on peak RSS (table above) |
| `Bun.serve` `routes` in a compiled binary | yes | exact route, `:param` route, `fetch` fallthrough and `server.upgrade()` all answered |
| Startup-failure trace, `bun run` from source (baseline) | yes | exit 1, one `‼️ startup failed` line, frames `http/http.plugin.ts application/application.ts …` |
| Startup-failure trace, `bun run` bundle | conditional | exit 1, one line, frames point at `server.js` (bundle positions) |
| Startup-failure trace, compiled binary | conditional | exit 1, one line, frames point at `/$bunfs/root/<binary>:10736:24` — function names survive, source positions do not |
| Startup-failure trace, compiled binary + bytecode + `--sourcemap=inline` | yes | exit 1, one line, frames `http/http.plugin.ts application/application.ts app-bootstrap.ts` — **original sources restored** |
| `--bytecode` without `--format=esm` | no | rejected: `"await" can only be used inside an "async" function` at `.gen/src/serve.bundled.ts:12` |

Env / config loading is not its own row because every row above already
exercises it: `PORT` (`Env('PORT')`) decides where each spawned executable
binds — no check would answer without it — and `PUTNAMI_ASSETS_DIR`
(`Env('PUTNAMI_ASSETS_DIR')` on `PutnamiConfig.assetsDir`) is exactly the
difference between the embedded-assets row passing and the
no-`PUTNAMI_ASSETS_DIR` row 500ing.

## Findings

**1. `--bytecode` requires `--format=esm`.** `--bytecode` defaults the output
format to CJS, and the generated entrypoint ends in a top-level
`await bootstrapServe(...)`, which CJS cannot represent — the build fails with
`"await" can only be used inside an "async" function`. Adding `--format=esm`
fixes it on bun 1.4.0. Measure with bun ≥ 1.4: bun 1.3.11 has no `--asset` at
all (it parses the value as an entry point and fails with `ModuleNotFound
resolving ".gen/public"`).

**2. `--bytecode` is not reproducible.** See the reproducibility table. This is
the one hard blocker against adopting it in the packaging pipeline as it stands.

**3. `--asset` works with StaticPlugin, but only via `assetsDir`.**
`--asset .gen/public` mounts the tree at `/$bunfs/root/public` — the `.gen/`
prefix is **not** preserved, so the mount path is not simply the argument. It
falls out of bun's build-root resolution, so probe it for your own layout rather
than assuming it. Inside the executable, `readFileSync`,
`statSync`, `readdirSync`, `existsSync`, `mkdirSync` (a no-op) and `Bun.file()`
all work on that path, so `StaticPlugin.routeStatic` → `generateETag` →
`serveStatic` needs no change. What it *does* need is
`PUTNAMI_ASSETS_DIR=/$bunfs/root/public`: `publicFolder()` resolves
`assetsDir ?? <projectRoot>/.gen/<publicFolder>`, and without the override the
routes register but every request 500s on a missing file. The generated
route-loader path itself (`.gen/src/static/.static.gen.ts`) is unaffected because
the bundled entrypoint imports it statically and registers it through
`registerModuleLoader('static-loader', …)`; the dynamic-import fallback in
`resolveRouteLoaders()` is never reached.

**4. Embedded assets have no mtime.** Files in `/$bunfs` stat as mtime 0, so the
HEAD response carries `Last-Modified: Thu, 01 Jan 1970 00:00:00 GMT`. ETags and
conditional requests still work (the ETag is a content hash), so caching degrades
rather than breaks — but a proxy doing `Last-Modified` heuristics sees every
asset as ancient.

**5. `--asset` cannot carry the whole `.gen` tree.** The packager writes
`/app/.gen/version.json` (the content stamp `getBuildInfo()` reads) *after* the
compile, from the image content hash — it cannot exist at compile time. Any
`--asset` adoption therefore keeps an image layer for `.gen` anyway, which
removes most of the motivation. It would also add `.gen/public` to the compile
task's cache-key inputs: a compile cached before an asset change would otherwise
bake stale bytes into a "fresh" binary.

**6. Compiled binaries lose source positions in stack traces — and
`--sourcemap=inline` gets them back, for free at runtime and 0.6 MiB on disk.**
This is a real regression in what `putnami package --docker` ships today
(measured, not theoretical): a startup failure through `bootstrapServe` logs its
single structured `ERROR` line either way, but the embedded stack reads
`at start (/$bunfs/root/<binary>:10736:24)` instead of
`at start (…/framework/application/src/application/application.ts:…)`.

**7. bun 1.4.0's linux-x64 executable does not need libstdc++.** `ldd` reports
only `ld-linux`, `libc`, `libm`, `libpthread`, `libdl` — so the
`gcr.io/distroless/cc-debian12` pin (whose comment says bun-compiled binaries
need "glibc and libstdc++") is one image tier heavier than required on this
target. `base-debian12` runs it (measured), and even `scratch` plus five copied
`.so` files runs it. The saving is small in pull bytes (44.6 → 43.7 → 38.2 MiB)
and the `cc` pin buys headroom for other bun targets, so this is an observation
for a future bump, not an ask.

**8. `--smol` needs `--compile-exec-argv`.** A compiled binary has no CLI surface
for runtime flags: `--compile-exec-argv=--smol` bakes it in (verified — the two
binaries are the same size but differ in content, and peak RSS drops ~5%
like-for-like, 63.9 → 61.0 MiB).

## Verdict

**Keep the compiled single binary — it is already the shipping shape, and it
wins on every axis measured (-33% image pull, -44% cold start, -7% idle RSS).
The remaining `oven/bun:1.4-slim + bun run` path in
`typescript/framework/events/Dockerfile` is worth converting. Do *not* adopt
`--asset`; do adopt `--sourcemap=inline`; hold `--bytecode` until it is
reproducible.**

Concretely, in priority order:

1. **Add `--sourcemap=inline` to the compile step** (`buildCompileArgs` in
   `typescript/extension/internal/build/compile.go`). Cost: +0.6 MiB on an
   79 MiB artifact (+0.8%), 0 ms of cold start, still byte-reproducible. Benefit:
   packaged services stop reporting `/$bunfs/root/<binary>:10736` in production
   stack traces. Highest value-to-risk ratio in this bench.
2. **Convert `typescript/framework/events/Dockerfile`** (or replace it with a
   `putnami package --docker` channel) — it is the last TS workload on the
   bun-image path. Measured cost of staying: +21.8 MiB per pull (65.5 vs 43.7),
   +39 ms of container cold start (84 ms vs 54 ms of in-container boot once the
   shared `docker run` overhead is subtracted), +1.5 MiB of container memory.
3. **Do not wire `putnami package --single-binary`.** There is nothing left to
   wire: `PackageDocker` already compiles one target for the image platform and
   assembles a distroless image around it. A `--single-binary` flag would be a
   second name for the default.
4. **`--bytecode`: blocked on reproducibility.** It is the largest single
   cold-start win in this bench (-35% on top of `--compile`, 56.6 → 36.9 ms) and
   costs only 2.5 MiB, but two compiles of identical inputs differ, which breaks
   the content-addressed image identity the packager depends on. Revisit when
   bun's bytecode emission is deterministic, or gate it behind an explicit
   opt-out of digest stability (e.g. a latency-sensitive service that accepts a
   fresh digest per build). Worth an upstream bun issue.
5. **Skip `--asset`.** It works, it is cheap (4 096 bytes) and it makes static reads
   ~1.5× faster, but it needs a `/$bunfs/...` literal in `PUTNAMI_ASSETS_DIR`,
   it zeroes `Last-Modified`, it cannot carry the post-compile version stamp
   (so the `.gen` layer stays anyway), and it would pull `.gen/public` into the
   compile cache key. The image-layer path costs nothing measurable and keeps one
   source of truth for assets.
6. **`--smol`, `cc`→`base`, `scratch`: leave as is.** `--smol` trades 2 ms of
   cold start for ~5% of peak RSS — right for memory-capped revisions, wrong as a
   default. `scratch` saves 5.5 MiB of pull over distroless but gives up the CA
   bundle, `/etc/passwd`, timezone data and the base's CVE stream; not worth it
   for 12% of an image that is 78 MiB of bun runtime either way.

## Coverage

Measured here:

- [x] Image size — distroless and scratch single-binary images vs `oven/bun:1.4-slim`
- [x] Cold start — process and container, with and without `--bytecode`
- [x] Memory — idle and under HTTP + WebSocket load, process RSS and container cgroup
- [x] Build time and artifact size, including `--asset` embedding of the `.gen` tree
- [x] StaticPlugin's generated route loader against assets under `/$bunfs/`
- [x] Compatibility — `Bun.serve` routes, WebSocket, SSE, `--smol`, env/config, `bootstrapServe` stack traces
- [x] Build reproducibility per compile flag set

Not measured:

- [ ] Cross-compilation targets other than `bun-linux-x64` (the `cc`→`base` base-image
      question needs `ldd` on each target before it can be answered generally)
- [ ] Real Cloud Run scale-from-zero, which adds image *pull* time on top of the
      boot measured here — the axis where the -33% pull size pays off and the one
      a local docker daemon cannot reproduce
- [ ] Steady-state throughput of non-static routes (the load phase here exists to
      make memory measurable, not to rank runtimes)
