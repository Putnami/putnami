#!/usr/bin/env bash
# Single-binary packaging bench for TypeScript servers.
#
# Compares `bun build --compile` (optionally with --asset asset embedding and
# --bytecode) against the deployment baseline this repo ships today
# (oven/bun:1.4-slim + `bun run`), on the @example/08-real-time sample.
#
# It measures, per variant: build time, artifact size, cold start (process and
# container), RSS at idle / under load, and image size. Compatibility probes
# (Bun.serve routes, WebSocket, SSE, --smol, env config, sourcemapped stack
# traces) live in `compat` below.
#
#   ./bench.sh all            # build + determinism + process + docker + compat + report
#   ./bench.sh build          # compile every variant, record time and size
#   ./bench.sh determinism    # is each compile flag set byte-reproducible?
#   ./bench.sh process        # bare-process cold start / memory / load
#   ./bench.sh docker         # image size + container cold start + container RSS
#   ./bench.sh compat         # compatibility checklist
#   ./bench.sh report         # render markdown tables from the recorded results
#
# Environment:
#   BENCH_BUN         bun binary to build and run with (default: bun; needs >= 1.4
#                     for --asset and bytecode-ESM)
#   BENCH_SAMPLE      sample project directory (default: typescript/samples/08-real-time)
#   BENCH_WORK        scratch directory for artifacts and results
#                     (default: $TMPDIR/putnami-single-binary-bench)
#   BENCH_BASE_PORT   first port to bind (default: 45080; busy ports are skipped)
#   BENCH_COLD_STARTS cold-start repetitions per process variant (default: 10)
#   BENCH_DOCKER_COLD_STARTS
#                     cold-start repetitions per container variant (default: 5)
#   BENCH_TARGET      bun --target for every compile, matching what
#                     buildCompileArgs passes (default: derived from the host;
#                     the docker phase requires bun-linux-x64)
#   BENCH_SHIPPED_BASE_IMAGE
#                     base image for the "ships today" row (default: the digest
#                     typescript/extension/internal/pkg/docker.go pins)
#   BENCH_SKIP_DOCKER set to 1 to skip every docker step
#
# The sample must be built first so `.gen` exists:
#   ./putnamiw build --projects @example/08-real-time
set -euo pipefail

BENCH_BUN="${BENCH_BUN:-bun}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../../../.." && pwd)"
SAMPLE_DIR="${BENCH_SAMPLE:-$REPO_ROOT/typescript/samples/08-real-time}"
WORK="${BENCH_WORK:-${TMPDIR:-/tmp}/putnami-single-binary-bench}"
BASE_PORT="${BENCH_BASE_PORT:-45080}"
COLD_STARTS="${BENCH_COLD_STARTS:-10}"
DOCKER_COLD_STARTS="${BENCH_DOCKER_COLD_STARTS:-5}"
ENTRYPOINT=".gen/src/serve.bundled.ts"
ASSET_DIR=".gen/public"
# Where `--asset .gen/public` lands inside the compiled executable's virtual FS.
EMBEDDED_ASSETS='/$bunfs/root/public'

ARTIFACTS="$WORK/artifacts"
CTX="$WORK/ctx"
IMAGE_PREFIX="putnami-bench-single-binary"
# The base `putnami package --docker` pins today (typescript/extension/internal/pkg/docker.go).
SHIPPED_BASE_IMAGE="${BENCH_SHIPPED_BASE_IMAGE:-gcr.io/distroless/cc-debian12:nonroot@sha256:b0ae8e989418b458e0f25489bc3be523718938a2b70864cc0f6a00af1ddbd985}"

log() { printf '\033[1m==> %s\033[0m\n' "$*" >&2; }

# The compile target buildCompileArgs always passes; the bench must match it or
# it measures an artifact shape `putnami package` never produces.
host_target() {
  local os="" arch=""
  case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; esac
  case "$(uname -m)" in x86_64 | amd64) arch=x64 ;; arm64 | aarch64) arch=arm64 ;; esac
  if [ -n "$os" ] && [ -n "$arch" ]; then printf 'bun-%s-%s\n' "$os" "$arch"; fi
}
BENCH_TARGET="${BENCH_TARGET:-$(host_target)}"

require_inputs() {
  if [ ! -f "$SAMPLE_DIR/$ENTRYPOINT" ]; then
    echo "missing $SAMPLE_DIR/$ENTRYPOINT — run: ./putnamiw build --projects @example/08-real-time" >&2
    exit 2
  fi
  if ! "$BENCH_BUN" --version >/dev/null 2>&1; then
    echo "BENCH_BUN=$BENCH_BUN is not runnable" >&2
    exit 2
  fi
  local version major minor
  version="$("$BENCH_BUN" --version)"
  major="${version%%.*}"
  minor="${version#*.}"
  minor="${minor%%.*}"
  if [ "$major" -lt 1 ] || { [ "$major" -eq 1 ] && [ "$minor" -lt 4 ]; }; then
    echo "bun $version lacks --asset and bytecode-ESM; point BENCH_BUN at bun >= 1.4" >&2
    exit 2
  fi
  if [ -z "$BENCH_TARGET" ]; then
    echo "could not derive a bun --target for $(uname -s)/$(uname -m); set BENCH_TARGET" >&2
    exit 2
  fi
}

file_size() { stat -c %s "$1" 2>/dev/null || stat -f %z "$1"; }
# Millisecond wall clock. bash >= 5 has EPOCHREALTIME everywhere; the fallback
# needs GNU date (%N) — BSD date silently emits the literal "%3N", so validate
# once at startup rather than corrupting every timing arithmetic.
if [ -n "${EPOCHREALTIME:-}" ]; then
  now_ms() {
    local micros="${EPOCHREALTIME/./}"
    printf '%s\n' "$((micros / 1000))"
  }
else
  now_ms() { date +%s%3N; }
fi
require_timer() {
  case "$(now_ms)" in
    '' | *[!0-9]*)
      echo "now_ms produced '$(now_ms)' — the timed phases need bash >= 5 or GNU date" >&2
      exit 2
      ;;
  esac
}
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# ---------------------------------------------------------------- build ------

# build_variant <name> <outfile> <bun build flags...>
# Builds three times and keeps the fastest wall time (the first run pays for a
# cold page cache on the bun binary itself, which is not what we are measuring).
build_variant() {
  local name="$1" outfile="$2"
  shift 2
  local best="" start end elapsed
  for _ in 1 2 3; do
    rm -f "$outfile"
    start="$(now_ms)"
    (cd "$SAMPLE_DIR" && "$BENCH_BUN" "$@" >/dev/null)
    end="$(now_ms)"
    elapsed=$((end - start))
    if [ -z "$best" ] || [ "$elapsed" -lt "$best" ]; then best="$elapsed"; fi
  done
  printf '%s\t%s\t%s\t%s\n' "$name" "$(file_size "$outfile")" "$best" "$*" >>"$WORK/build.tsv"
  log "built $name: $(file_size "$outfile") bytes in ${best}ms"
}

cmd_build() {
  require_inputs
  require_timer
  rm -rf "$ARTIFACTS" "$WORK/build.tsv"
  mkdir -p "$ARTIFACTS"

  # Baseline: the JS bundle `bun run` executes on oven/bun:1.4-slim.
  build_variant "bun-bundle" "$ARTIFACTS/server.js" \
    build --target=bun --outfile "$ARTIFACTS/server.js" "$ENTRYPOINT"

  # Floor: a dependency-free script compiled the same way. Everything above this
  # size is the embedded bun runtime, not the application. Every --compile below
  # passes --target because buildCompileArgs always does.
  build_variant "runtime-floor" "$ARTIFACTS/srv-runtime-floor" \
    build --compile --target "$BENCH_TARGET" \
    --outfile "$ARTIFACTS/srv-runtime-floor" "$SCRIPT_DIR/native-routes-probe.ts"

  build_variant "binary" "$ARTIFACTS/srv-binary" \
    build --compile --target "$BENCH_TARGET" --outfile "$ARTIFACTS/srv-binary" "$ENTRYPOINT"

  build_variant "binary-bytecode" "$ARTIFACTS/srv-binary-bytecode" \
    build --compile --target "$BENCH_TARGET" --bytecode --format=esm \
    --outfile "$ARTIFACTS/srv-binary-bytecode" "$ENTRYPOINT"

  build_variant "binary-asset" "$ARTIFACTS/srv-binary-asset" \
    build --compile --target "$BENCH_TARGET" --asset "$ASSET_DIR" \
    --outfile "$ARTIFACTS/srv-binary-asset" "$ENTRYPOINT"

  build_variant "binary-asset-bytecode" "$ARTIFACTS/srv-binary-asset-bytecode" \
    build --compile --target "$BENCH_TARGET" --asset "$ASSET_DIR" --bytecode --format=esm \
    --outfile "$ARTIFACTS/srv-binary-asset-bytecode" "$ENTRYPOINT"

  build_variant "binary-asset-bytecode-smol" "$ARTIFACTS/srv-binary-asset-bytecode-smol" \
    build --compile --target "$BENCH_TARGET" --asset "$ASSET_DIR" --bytecode --format=esm \
    --compile-exec-argv=--smol \
    --outfile "$ARTIFACTS/srv-binary-asset-bytecode-smol" "$ENTRYPOINT"

  build_variant "binary-asset-sourcemap" "$ARTIFACTS/srv-binary-asset-sourcemap" \
    build --compile --target "$BENCH_TARGET" --asset "$ASSET_DIR" --sourcemap=inline \
    --outfile "$ARTIFACTS/srv-binary-asset-sourcemap" "$ENTRYPOINT"

  build_variant "binary-asset-bytecode-sourcemap" "$ARTIFACTS/srv-binary-asset-bytecode-sourcemap" \
    build --compile --target "$BENCH_TARGET" --asset "$ASSET_DIR" --bytecode --format=esm \
    --sourcemap=inline \
    --outfile "$ARTIFACTS/srv-binary-asset-bytecode-sourcemap" "$ENTRYPOINT"

  # Runtime layouts: assets on disk (what `putnami package --docker` ships today)
  # versus assets embedded in the executable (what --asset buys).
  rm -rf "$WORK/run-disk" "$WORK/run-embedded"
  mkdir -p "$WORK/run-disk/.gen" "$WORK/run-embedded"
  cp -R "$SAMPLE_DIR/$ASSET_DIR" "$WORK/run-disk/.gen/public"
  log "artifacts in $ARTIFACTS"
}

# ---------------------------------------------------- build determinism ------

# determinism_case <label> <extra bun build flags...>
# Compiles the same inputs twice into two directories under the SAME output
# basename (bun embeds the output path in the executable, so a differing
# basename is a legitimate difference, not nondeterminism) and compares digests.
# `putnami package --docker` content-addresses images, so a compile flag that
# is not reproducible changes the image identity on every rebuild.
determinism_case() {
  local label="$1"
  shift
  local first second
  rm -rf "$WORK/det/a" "$WORK/det/b"
  mkdir -p "$WORK/det/a" "$WORK/det/b"
  # ${1+"$@"}: macOS's bash 3.2 treats an empty "$@" as unbound under set -u.
  (cd "$SAMPLE_DIR" && "$BENCH_BUN" build --compile --target "$BENCH_TARGET" ${1+"$@"} \
    --outfile "$WORK/det/a/srv" "$ENTRYPOINT" >/dev/null)
  (cd "$SAMPLE_DIR" && "$BENCH_BUN" build --compile --target "$BENCH_TARGET" ${1+"$@"} \
    --outfile "$WORK/det/b/srv" "$ENTRYPOINT" >/dev/null)
  first="$(sha256_of "$WORK/det/a/srv")"
  second="$(sha256_of "$WORK/det/b/srv")"
  local differing=0
  if [ "$first" != "$second" ]; then
    # `cmp -l` exits non-zero precisely when the files differ, which is the case
    # being counted — shield it from `set -o pipefail`.
    differing="$( { cmp -l "$WORK/det/a/srv" "$WORK/det/b/srv" || true; } | wc -l | tr -d ' ')"
  fi
  printf '%s\t%s\t%s\t%s\n' "$label" "$([ "$first" = "$second" ] && echo yes || echo no)" "$differing" "$first" \
    >>"$WORK/determinism.tsv"
  log "determinism $label: $([ "$first" = "$second" ] && echo reproducible || echo "NOT reproducible ($differing bytes differ)")"
}

cmd_determinism() {
  require_inputs
  : >"$WORK/determinism.tsv"
  determinism_case "--compile"
  determinism_case "--compile --asset .gen/public" --asset "$ASSET_DIR"
  determinism_case "--compile --sourcemap=inline" --sourcemap=inline
  determinism_case "--compile --bytecode --format=esm" --bytecode --format=esm
  determinism_case "--compile --asset --bytecode --format=esm" --asset "$ASSET_DIR" --bytecode --format=esm
  rm -rf "$WORK/det"
}

# -------------------------------------------------------------- process ------

cmd_process() {
  require_inputs
  [ -d "$ARTIFACTS" ] || cmd_build
  local embedded_env
  embedded_env="{\"PUTNAMI_ASSETS_DIR\":\"$EMBEDDED_ASSETS\"}"
  cat >"$WORK/variants.json" <<JSON
{
  "basePort": $BASE_PORT,
  "coldStarts": $COLD_STARTS,
  "httpRequests": 4000,
  "httpConcurrency": 32,
  "wsClients": 25,
  "wsMessages": 10,
  "probePath": "/index.html",
  "variants": [
    { "name": "bun run (baseline)",        "cwd": "$WORK/run-disk",     "cmd": ["$BENCH_BUN", "run", "$ARTIFACTS/server.js"] },
    { "name": "bun run --smol (baseline)", "cwd": "$WORK/run-disk",     "cmd": ["$BENCH_BUN", "run", "--smol", "$ARTIFACTS/server.js"] },
    { "name": "binary",                    "cwd": "$WORK/run-disk",     "cmd": ["$ARTIFACTS/srv-binary"] },
    { "name": "binary + bytecode",         "cwd": "$WORK/run-disk",     "cmd": ["$ARTIFACTS/srv-binary-bytecode"] },
    { "name": "binary + asset",            "cwd": "$WORK/run-embedded", "cmd": ["$ARTIFACTS/srv-binary-asset"], "env": $embedded_env },
    { "name": "binary + asset + bytecode", "cwd": "$WORK/run-embedded", "cmd": ["$ARTIFACTS/srv-binary-asset-bytecode"], "env": $embedded_env },
    { "name": "binary + asset + bytecode + smol", "cwd": "$WORK/run-embedded", "cmd": ["$ARTIFACTS/srv-binary-asset-bytecode-smol"], "env": $embedded_env },
    { "name": "binary + asset + bytecode + sourcemap", "cwd": "$WORK/run-embedded", "cmd": ["$ARTIFACTS/srv-binary-asset-bytecode-sourcemap"], "env": $embedded_env }
  ]
}
JSON
  "$BENCH_BUN" "$SCRIPT_DIR/measure.ts" "$WORK/variants.json" --out "$WORK/process.json" >/dev/null
  log "process results in $WORK/process.json"
}

# --------------------------------------------------------------- docker ------

prepare_ctx() {
  rm -rf "$CTX"
  mkdir -p "$CTX"
  cp "$ARTIFACTS/server.js" "$CTX/server.js"
  cp "$ARTIFACTS/srv-binary" "$CTX/srv-binary"
  cp "$ARTIFACTS/srv-binary-asset-bytecode" "$CTX/srv-binary-asset-bytecode"
  cp -R "$SAMPLE_DIR/$ASSET_DIR" "$CTX/public"
}

# docker_build <image suffix> <dockerfile> [--build-arg ...]
docker_build() {
  local suffix="$1" dockerfile="$2"
  shift 2
  docker build --quiet --platform linux/amd64 -f "$SCRIPT_DIR/$dockerfile" -t "$IMAGE_PREFIX:$suffix" \
    ${1+"$@"} "$CTX" >/dev/null
}

# True when something on the host is already listening — the docker phase must
# honour the same "busy ports are skipped, not silently used" contract as
# takePort() in measure.ts/compat.ts.
port_busy() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }

# Advances the global `port` past busy ones.
next_free_port() {
  while port_busy "$port"; do
    log "port $port is busy, skipping"
    port=$((port + 1))
  done
}

# image_pull_bytes <image> -> sum of compressed layer sizes (what a Cloud Run
# cold start actually downloads). With the containerd image store this is what
# `docker image inspect .Size` reports.
image_pull_bytes() { docker image inspect -f '{{.Size}}' "$1"; }

# image_unpacked_bytes <image> -> flattened filesystem size. `docker history`
# layer sizes sum to the same number as `docker export | wc -c` (verified);
# `docker images` SIZE over-reports under the containerd snapshotter.
image_unpacked_bytes() {
  docker history --human=false --format '{{.Size}}' "$1" | awk '{total += $1} END {print total}'
}

# docker_cold_start <image> <port> -> prints "<total ms> <docker-run ms>".
# `docker run -d` returns once containerd has started the container, so the
# second number is the runtime-independent overhead every row pays and the
# difference is the workload's own boot.
docker_cold_start() {
  local image="$1" port="$2" name start started id code
  name="$IMAGE_PREFIX-$(date +%s%N)"
  start="$(now_ms)"
  id="$(docker run -d --name "$name" -p "127.0.0.1:$port:3000" "$image")"
  started="$(now_ms)"
  local ready=""
  while [ -z "$ready" ]; do
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://127.0.0.1:$port/index.html" || true)"
    if [ "$code" = "200" ]; then ready="$(now_ms)"; fi
    if [ "$(( $(now_ms) - start ))" -gt 30000 ]; then
      docker logs "$id" >&2 || true
      docker rm -f "$id" >/dev/null 2>&1 || true
      echo "container $image never answered" >&2
      return 1
    fi
  done
  echo "$((ready - start)) $((started - start))"
  docker rm -f "$id" >/dev/null 2>&1 || true
}

# docker_idle_mem <image> <port> -> prints the container's `docker stats`
# MemUsage figure (e.g. `11.94MiB`), or `n/a` when the container never became
# ready — a dead container's number would silently pass for an idle one.
docker_idle_mem() {
  local image="$1" port="$2" name id usage ready=""
  name="$IMAGE_PREFIX-mem-$(date +%s%N)"
  id="$(docker run -d --name "$name" -p "127.0.0.1:$port:3000" "$image")"
  local start
  start="$(now_ms)"
  while [ -z "$ready" ]; do
    if [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://127.0.0.1:$port/index.html" || true)" = "200" ]; then
      ready=1
    elif [ "$(( $(now_ms) - start ))" -gt 30000 ]; then break; fi
  done
  if [ -n "$ready" ]; then
    sleep 2
    usage="$(docker stats --no-stream --format '{{.MemUsage}}' "$id" | awk '{print $1}')"
  else
    docker logs "$id" >&2 || true
    usage="n/a"
  fi
  docker rm -f "$id" >/dev/null 2>&1 || true
  echo "$usage"
}

cmd_docker() {
  if [ "${BENCH_SKIP_DOCKER:-0}" = "1" ]; then
    log "skipping docker (BENCH_SKIP_DOCKER=1)"
    return 0
  fi
  require_timer
  if [ "$BENCH_TARGET" != "bun-linux-x64" ]; then
    # The images are built --platform linux/amd64; a binary for any other
    # target would either fail the readiness loop or measure a mismatched
    # artifact. Skipping loudly beats publishing numbers for the wrong shape.
    log "skipping docker: it needs bun-linux-x64 artifacts, BENCH_TARGET=$BENCH_TARGET"
    return 0
  fi
  [ -d "$ARTIFACTS" ] || cmd_build
  prepare_ctx
  : >"$WORK/docker.tsv"
  # An aborted run must not leave measurement containers holding the ports the
  # next invocation will want.
  trap 'docker ps -aq --filter "name=$IMAGE_PREFIX-" | xargs -r docker rm -f >/dev/null 2>&1 || true' EXIT

  log "building images"
  docker_build "bun-slim" "Dockerfile.bun-slim"
  docker_build "cc-distroless-binary" "Dockerfile.distroless" --build-arg "BASE_IMAGE=$SHIPPED_BASE_IMAGE"
  docker_build "distroless-binary" "Dockerfile.distroless"
  docker_build "distroless-asset" "Dockerfile.distroless-asset"
  docker_build "scratch-asset" "Dockerfile.scratch-asset"

  local port=$((BASE_PORT + 500))
  for suffix in bun-slim cc-distroless-binary distroless-binary distroless-asset scratch-asset; do
    local image="$IMAGE_PREFIX:$suffix"
    local pull unpacked totals overheads sample mem iteration
    pull="$(image_pull_bytes "$image")"
    unpacked="$(image_unpacked_bytes "$image")"
    totals=""
    overheads=""
    iteration=0
    while [ "$iteration" -lt "$DOCKER_COLD_STARTS" ]; do
      iteration=$((iteration + 1))
      port=$((port + 1))
      next_free_port
      sample="$(docker_cold_start "$image" "$port")"
      totals="$totals ${sample% *}"
      overheads="$overheads ${sample#* }"
    done
    port=$((port + 1))
    next_free_port
    mem="$(docker_idle_mem "$image" "$port")"
    printf '%s\t%s\t%s\t%s\t%s\t%s\n' \
      "$suffix" "$pull" "$unpacked" "${totals# }" "${overheads# }" "$mem" >>"$WORK/docker.tsv"
    log "$suffix: pull ${pull}B, unpacked ${unpacked}B, cold starts:${totals}, docker-run overhead:${overheads}, idle mem $mem"
  done
}

# --------------------------------------------------------------- compat ------

cmd_compat() {
  [ -d "$ARTIFACTS" ] || cmd_build
  "$BENCH_BUN" "$SCRIPT_DIR/compat.ts" \
    --binary "$ARTIFACTS/srv-binary-asset-bytecode" \
    --binary-disk "$ARTIFACTS/srv-binary" \
    --binary-smol "$ARTIFACTS/srv-binary-asset-bytecode-smol" \
    --binary-sourcemap "$ARTIFACTS/srv-binary-asset-bytecode-sourcemap" \
    --bundle "$ARTIFACTS/server.js" \
    --bun "$BENCH_BUN" \
    --disk-cwd "$WORK/run-disk" \
    --embedded-cwd "$WORK/run-embedded" \
    --assets-dir "$EMBEDDED_ASSETS" \
    --port $((BASE_PORT + 800)) \
    --native-probe "$SCRIPT_DIR/native-routes-probe.ts" \
    --sample "$SAMPLE_DIR" \
    --entry "$ENTRYPOINT" \
    --out "$WORK/compat.json" >/dev/null
  log "compat results in $WORK/compat.json"
}

# --------------------------------------------------------------- report ------

cmd_report() { "$BENCH_BUN" "$SCRIPT_DIR/report.ts" "$WORK"; }

cmd_all() {
  cmd_build
  cmd_determinism
  cmd_process
  cmd_docker
  cmd_compat
  cmd_report
}

mkdir -p "$WORK"
case "${1:-all}" in
  build) cmd_build ;;
  determinism) cmd_determinism ;;
  process) cmd_process ;;
  docker) cmd_docker ;;
  compat) cmd_compat ;;
  report) cmd_report ;;
  all) cmd_all ;;
  *)
    echo "usage: $0 [build|determinism|process|docker|compat|report|all]" >&2
    exit 2
    ;;
esac
