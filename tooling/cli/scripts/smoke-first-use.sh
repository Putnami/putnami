#!/usr/bin/env bash
# First-use smoke for the putnami CLI: the documented first-use path on a
# machine that holds only what the installer needs.
#
# It reads the first `bash` block of the getting-started page and runs it
# exactly as printed, as a new non-root user, in an image that holds Bash, curl,
# tar and a SHA-256 tool and nothing else: no Bun, no git, no Node.js, no Go.
# It then runs what the page tells that user to run next, and the Go path:
#
#   <the documented block>                      every page of the starter answers 200
#   putnami lint,test,build <project>           exit status 0
#   putnami init --project api --extension go   exit status 0, in a second directory
#   putnami serve api                           / answers 200
#   putnami lint,test,build api                 exit status 0
#
# Everything Putnami installs for itself must be under the Putnami home
# (~/.putnami): the smoke fails when ~/.bun exists, when a toolchain is missing
# under ~/.putnami/toolchains, or when a workspace holds a copy of Go.
#
# No command of the block is edited, reordered or wrapped. The smoke adds two
# things around it. PUTNAMI_OUTPUT=jsonl makes `putnami serve` report its port
# in the typed ready event. The shell that runs the block stops at the first
# command that fails, so a starter served after a failed command does not pass.
#
# Modes:
#   image (default)  Build the bare image with Docker and run the legs in it.
#   host             Run the legs on this machine, with an empty home and PATH
#                    reduced to the system directories. It proves the path on a
#                    host that cannot run the image (macOS); the tools the host
#                    itself holds are printed, not refused.
#
# Usage:   smoke-first-use.sh [--print-block] [channel]   (default channel: latest)
# Env:
#   PUTNAMI_REGISTRY_URL   registry base (default https://put.putnami.dev)
#   SMOKE_INSTALL_URL      installer URL that replaces the documented one in the
#                          block (default: unset, the block runs unchanged)
#   SMOKE_STARTUP_TIMEOUT  seconds to wait for each server's ready event (default 900)
#   SMOKE_FIRST_USE_MODE   image | host (default image)
#   SMOKE_FIRST_USE_PAGE   page the block is read from (default: the
#                          getting-started page of this repository)
#   SMOKE_FIRST_USE_IMAGE  base image of the bare image, Debian family
#                          (default ubuntu:24.04)
#   SMOKE_FIRST_USE_PATH   PATH of the legs in host mode
#                          (default /usr/bin:/bin:/usr/sbin:/sbin)

set -euo pipefail

default_registry="https://put.putnami.dev"
documented_installer="https://putnami.dev/install.sh"
starter_pages="/ /about /guestbook"
go_init_command="putnami init --project api --extension go"
go_serve_command="putnami serve api"
go_check_command="putnami lint,test,build api"
stop_budget_tenths=200

print_block=0
channel=""
for argument in "$@"; do
  case "$argument" in
    --print-block) print_block=1 ;;
    -*)
      echo "::error::usage: smoke-first-use.sh [--print-block] [channel], got '${argument}'"
      exit 2
      ;;
    *)
      if [ -n "$channel" ]; then
        echo "::error::usage: smoke-first-use.sh [--print-block] [channel], got a second channel '${argument}'"
        exit 2
      fi
      channel="$argument"
      ;;
  esac
done
channel="${channel:-latest}"

base="${PUTNAMI_REGISTRY_URL:-$default_registry}"
base="${base%/}"
installer_url="${SMOKE_INSTALL_URL:-}"
startup_timeout="${SMOKE_STARTUP_TIMEOUT:-900}"
mode="${SMOKE_FIRST_USE_MODE:-image}"
base_image="${SMOKE_FIRST_USE_IMAGE:-ubuntu:24.04}"
host_path="${SMOKE_FIRST_USE_PATH:-/usr/bin:/bin:/usr/sbin:/sbin}"
inside="${SMOKE_FIRST_USE_INSIDE:-}"

script_path="${BASH_SOURCE[0]}"
script_dir="$(cd "$(dirname "$script_path")" && pwd -P)"
script_file="${script_dir}/$(basename "$script_path")"
page="${SMOKE_FIRST_USE_PAGE:-${script_dir}/../../../sites/putnami.dev/doc/01-getting-started/index.md}"

fail() {
  echo "::error::$1"
  exit 1
}

case "$startup_timeout" in
  '' | *[!0-9]*) fail "SMOKE_STARTUP_TIMEOUT must be a non-negative integer, got '${startup_timeout}'" ;;
esac
case "$mode" in
  image | host) ;;
  *) fail "SMOKE_FIRST_USE_MODE must be image or host, got '${mode}'" ;;
esac

# ── Leg: block ───────────────────────────────────────────────────────────────
#
# The block is the first fenced `bash` block of the page: the lines a reader
# pastes. Its last line is the command that serves the starter, which names the
# project the later legs check.
if [ ! -f "$page" ]; then
  fail "block leg: ${page} does not exist; set SMOKE_FIRST_USE_PAGE to the getting-started page"
fi
block="$(awk '
  !found && /^```bash *$/ { found = 1; inside = 1; next }
  inside && /^```/ { exit }
  inside { print }
' "$page")"
if [ -z "$block" ]; then
  fail "block leg: ${page} has no fenced bash block"
fi
if ! printf '%s\n' "$block" | grep -Fq "curl -fsSL ${documented_installer} | bash"; then
  fail "block leg: the first bash block of ${page} does not install with 'curl -fsSL ${documented_installer} | bash'"
fi
serve_line="$(printf '%s\n' "$block" | tail -n 1)"
case "$serve_line" in
  "putnami serve "?*) project="${serve_line#putnami serve }" ;;
  *) fail "block leg: the first bash block of ${page} does not end with 'putnami serve <project>', got '${serve_line}'" ;;
esac
case "$project" in
  *[!abcdefghijklmnopqrstuvwxyz0123456789-]*)
    fail "block leg: the block serves '${project}', which is not a project name"
    ;;
esac

if [ "$print_block" -eq 1 ]; then
  printf '%s\n' "$block"
  exit 0
fi

# ── Outside: prepare the bare machine, then run the legs in it ───────────────
if [ -z "$inside" ]; then
  outer_workdir="$(mktemp -d)"
  image_id=""
  outer_cleanup() {
    status=$?
    trap - EXIT
    if [ -n "$image_id" ]; then
      docker rmi -f "$image_id" >/dev/null 2>&1 || true
    fi
    rm -rf "$outer_workdir"
    exit "$status"
  }
  trap outer_cleanup EXIT
  trap 'exit 130' INT TERM

  if [ "$mode" = "host" ]; then
    echo "smoke: host mode, empty home, PATH=${host_path}"
    mkdir -p "$outer_workdir/home"
    cp "$page" "$outer_workdir/page.md"
    env -i \
      HOME="$outer_workdir/home" \
      PATH="$host_path" \
      SMOKE_FIRST_USE_INSIDE=host \
      SMOKE_FIRST_USE_PAGE="$outer_workdir/page.md" \
      SMOKE_STARTUP_TIMEOUT="$startup_timeout" \
      SMOKE_INSTALL_URL="$installer_url" \
      PUTNAMI_REGISTRY_URL="$base" \
      "${BASH:-bash}" "$script_file" "$channel"
    exit $?
  fi

  if ! command -v docker >/dev/null 2>&1; then
    fail "image leg: Docker is required to build the bare image; on a host without it, SMOKE_FIRST_USE_MODE=host runs the legs with an empty home and a system PATH"
  fi
  cp "$script_file" "$outer_workdir/smoke-first-use.sh"
  cp "$page" "$outer_workdir/page.md"
  # The image holds what the installer needs and nothing else. The base image
  # brings Bash, tar and sha256sum; curl and the certificates it verifies with
  # are the only packages added.
  cat >"$outer_workdir/Dockerfile" <<EOF
FROM ${base_image}
RUN apt-get update \\
 && apt-get install -y --no-install-recommends ca-certificates curl \\
 && rm -rf /var/lib/apt/lists/*
RUN useradd --create-home --shell /bin/bash dev
COPY smoke-first-use.sh page.md /smoke/
USER dev
WORKDIR /home/dev
EOF
  echo "smoke: building the bare image from ${base_image}"
  if ! image_id="$(docker build -q "$outer_workdir" 2>"$outer_workdir/build.log")"; then
    image_id=""
    tail -n 200 "$outer_workdir/build.log" || true
    fail "image leg: docker build failed for the bare image from ${base_image}"
  fi
  docker run --rm --init \
    -e SMOKE_FIRST_USE_INSIDE=image \
    -e SMOKE_FIRST_USE_PAGE=/smoke/page.md \
    -e SMOKE_STARTUP_TIMEOUT="$startup_timeout" \
    -e SMOKE_INSTALL_URL="$installer_url" \
    -e PUTNAMI_REGISTRY_URL="$base" \
    "$image_id" bash /smoke/smoke-first-use.sh "$channel"
  exit $?
fi

# ── Inside: the legs ─────────────────────────────────────────────────────────
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
workdir="$(mktemp -d)"
group_pid=""

# Stop the process group of a served starter the way a terminal does when its
# user leaves: one TERM to the group, then the graceful budget. The status says
# whether the group ended without the hard fallback.
stop_group() {
  if [ -z "$group_pid" ]; then
    return 0
  fi
  forced=0
  kill -TERM -- "-$group_pid" 2>/dev/null || true
  stop_attempt=0
  while kill -0 -- "-$group_pid" 2>/dev/null; do
    if [ "$stop_attempt" -ge "$stop_budget_tenths" ]; then
      forced=1
      kill -KILL -- "-$group_pid" 2>/dev/null || true
      break
    fi
    sleep 0.1
    stop_attempt=$((stop_attempt + 1))
  done
  wait "$group_pid" 2>/dev/null || true
  group_pid=""
  [ "$forced" -eq 0 ]
}

cleanup() {
  status=$?
  trap - EXIT
  stop_group || true
  cd / 2>/dev/null || true
  rm -rf "$workdir"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# Fail a leg with the tail of its log.
fail_with_log() {
  echo "::error::$1"
  tail -n 200 "$2" 2>/dev/null || true
  exit 1
}

# ── Leg: bare ────────────────────────────────────────────────────────────────
for tool in bash curl tar; do
  command -v "$tool" >/dev/null 2>&1 || fail "bare leg: the machine holds no ${tool}, which the installer needs"
done
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
  fail "bare leg: the machine holds no sha256sum or shasum, which the installer needs"
fi
held=""
for tool in bun git node go; do
  if command -v "$tool" >/dev/null 2>&1; then
    held="${held} ${tool}"
  fi
done
if [ "$inside" = "image" ]; then
  if [ -n "$held" ]; then
    fail "bare leg: the image holds${held}; the first-use path must be proven without them"
  fi
  if [ "$(id -u)" -eq 0 ]; then
    fail "bare leg: the legs run as root; a new user is not root"
  fi
  old_ifs="$IFS"
  IFS=:
  for path_dir in $PATH; do
    if [ -n "$path_dir" ] && [ -d "$path_dir" ] && [ -w "$path_dir" ]; then
      IFS="$old_ifs"
      fail "bare leg: ${path_dir} is on PATH and writable by the user; the block must work without such a directory"
    fi
  done
  IFS="$old_ifs"
  echo "smoke: the image holds Bash, curl, tar and a SHA-256 tool; no bun, git, node or go; no PATH directory is writable"
elif [ -n "$held" ]; then
  echo "smoke: this host holds${held} on PATH; the image mode proves the path without them"
else
  echo "smoke: this host holds no bun, git, node or go on PATH"
fi

# The candidate channel and registry reach the installer and the CLI through the
# environment, so the commands themselves carry no private flag. The public
# channel on the public registry adds nothing to a new user's environment.
if [ "$channel" != "latest" ]; then
  export PUTNAMI_VERSION="$channel"
  export PUTNAMI_NO_RELAUNCH=1
fi
if [ "$base" != "$default_registry" ]; then
  export PUTNAMI_REGISTRY_URL="$base"
else
  unset PUTNAMI_REGISTRY_URL
fi
export PUTNAMI_TELEMETRY=off
export DO_NOT_TRACK=1

if [ -n "$installer_url" ]; then
  block="$(printf '%s\n' "$block" | awk -v from="$documented_installer" -v to="$installer_url" '
    {
      at = index($0, from)
      if (at > 0) {
        $0 = substr($0, 1, at - 1) to substr($0, at + length(from))
      }
      print
    }
  ')"
  echo "smoke: the installer URL of the block is replaced by ${installer_url}"
fi

# Wait for the typed ready event of the server whose process group is
# $group_pid, and print its port. The CLI writes the keys of a record in sorted
# order, so "port" comes before "type" on the line: select the ready line first,
# then read its port.
ready_port=""
wait_for_ready() {
  leg="$1"
  log="$2"
  ready_port=""
  deadline=$((SECONDS + startup_timeout))
  while [ -z "$ready_port" ]; do
    ready_port="$(sed -n '/"type":"ready"/s/.*"port":\([0-9][0-9]*\).*/\1/p' "$log" | tail -n 1)"
    if [ -n "$ready_port" ]; then
      return 0
    fi
    if ! kill -0 -- "-$group_pid" 2>/dev/null; then
      wait "$group_pid" 2>/dev/null || true
      group_pid=""
      fail_with_log "${leg} leg: the commands ended before the starter was ready" "$log"
    fi
    if [ "$SECONDS" -ge "$deadline" ]; then
      fail_with_log "${leg} leg: the starter did not emit readiness within ${startup_timeout}s" "$log"
    fi
    sleep 0.25
  done
}

# Require HTTP 200 and a non-empty body from each page of a served starter.
require_pages() {
  leg="$1"
  log="$2"
  shift 2
  for page_path in "$@"; do
    page_url="http://127.0.0.1:${ready_port}${page_path}"
    page_status="000"
    if ! page_status="$(curl -sS --connect-timeout 5 --max-time 30 \
      -o "$workdir/response" -w '%{http_code}' "$page_url")"; then
      fail_with_log "${leg} leg: ${page_url} did not answer after the ready event" "$log"
    fi
    if [ "$page_status" != "200" ]; then
      fail_with_log "${leg} leg: ${page_url} answered HTTP ${page_status}; expected 200" "$log"
    fi
    if [ ! -s "$workdir/response" ]; then
      fail_with_log "${leg} leg: ${page_url} answered 200 with an empty body" "$log"
    fi
    echo "smoke: GET ${page_url} answered 200"
  done
}

require_stop() {
  leg="$1"
  log="$2"
  if ! stop_group; then
    fail_with_log "${leg} leg: the starter did not stop within the graceful shutdown budget" "$log"
  fi
  if curl -sS --connect-timeout 1 --max-time 1 -o /dev/null \
    "http://127.0.0.1:${ready_port}/" >/dev/null 2>&1; then
    fail_with_log "${leg} leg: the HTTP listener still answers after the commands stopped" "$log"
  fi
}

# ── Leg: block, run as printed ───────────────────────────────────────────────
#
# The block runs in its own shell, from an empty directory, as one pasted text.
# That shell stops at the first command that fails: every command before the
# last one must exit 0 for the starter to be served. The last command serves
# until it is stopped, so the shell runs in a process group of its own that the
# smoke stops once the pages answered.
first_use_dir="$HOME/first-use"
mkdir -p "$first_use_dir"
if [ -n "$(ls -A "$first_use_dir")" ]; then
  fail "block leg: ${first_use_dir} is not empty"
fi
printf '%s\n' "$block" >"$workdir/block.sh"
block_log="$workdir/block.log"
: >"$block_log"
echo "smoke: running the documented block from ${first_use_dir}:"
printf '%s\n' "$block" | sed 's/^/smoke:   /'
set -m
(
  cd "$first_use_dir"
  export PUTNAMI_OUTPUT=jsonl
  exec bash -e "$workdir/block.sh"
) </dev/null >"$block_log" 2>&1 &
group_pid=$!
set +m
wait_for_ready block "$block_log"
if ! command -v "$HOME/.putnami/bin/putnami" >/dev/null 2>&1; then
  fail_with_log "block leg: the block served a starter, but ${HOME}/.putnami/bin/putnami is not executable" "$block_log"
fi
# shellcheck disable=SC2086 # starter_pages is a list of paths.
require_pages pages "$block_log" $starter_pages
require_stop stop "$block_log"
echo "smoke: the documented block installed, initialized and served ${project}; the starter stopped cleanly"

# The shell that pasted the block holds what the block exported. The later
# commands run in that same shell, so the smoke applies the block's own export
# lines and nothing else.
while IFS= read -r block_line; do
  case "$block_line" in
    "export "*) eval "$block_line" ;;
  esac
done <<EOF
$block
EOF
if ! command -v putnami >/dev/null 2>&1; then
  fail "check leg: 'putnami' is not reachable after the export lines of the block"
fi

# ── Leg: check ───────────────────────────────────────────────────────────────
check_command="putnami lint,test,build ${project}"
check_log="$workdir/check.log"
echo "smoke: ${check_command}"
if ! (
  cd "$first_use_dir"
  putnami lint,test,build "$project"
) </dev/null >"$check_log" 2>&1; then
  fail_with_log "check leg: '${check_command}' failed on the starter the block created" "$check_log"
fi

# ── Legs: the Go path ────────────────────────────────────────────────────────
go_dir="$HOME/first-use-go"
mkdir -p "$go_dir"
go_init_log="$workdir/go-init.log"
echo "smoke: ${go_init_command}"
if ! (
  cd "$go_dir"
  putnami init --project api --extension go
) </dev/null >"$go_init_log" 2>&1; then
  fail_with_log "go-init leg: '${go_init_command}' failed" "$go_init_log"
fi

go_serve_log="$workdir/go-serve.log"
: >"$go_serve_log"
echo "smoke: ${go_serve_command}"
set -m
(
  cd "$go_dir"
  export PUTNAMI_OUTPUT=jsonl
  exec putnami serve api
) </dev/null >"$go_serve_log" 2>&1 &
group_pid=$!
set +m
wait_for_ready go-serve "$go_serve_log"
require_pages go-pages "$go_serve_log" /
require_stop go-stop "$go_serve_log"

go_check_log="$workdir/go-check.log"
echo "smoke: ${go_check_command}"
if ! (
  cd "$go_dir"
  putnami lint,test,build api
) </dev/null >"$go_check_log" 2>&1; then
  fail_with_log "go-check leg: '${go_check_command}' failed on the Go starter" "$go_check_log"
fi

# ── Leg: home ────────────────────────────────────────────────────────────────
#
# What Putnami installs for itself is private to the Putnami home. A toolchain
# the host already offers is used where it is, so the two "installed" checks
# apply only where the machine held none.
putnami_home="${PUTNAMI_HOME:-$HOME/.putnami}"
if [ -e "$HOME/.bun" ]; then
  fail "home leg: ${HOME}/.bun exists; Bun must be installed under ${putnami_home}/toolchains/bun"
fi
installed_under_home() {
  for candidate in "$putnami_home"/toolchains/$1; do
    if [ -x "$candidate" ]; then
      echo "smoke: ${candidate} is installed under the Putnami home"
      return 0
    fi
  done
  return 1
}
case " ${held} " in
  *" bun "*) ;;
  *)
    installed_under_home "bun/bun-*/bin/bun" ||
      fail "home leg: no Bun under ${putnami_home}/toolchains/bun after the TypeScript path ran without one on PATH"
    ;;
esac
case " ${held} " in
  *" go "*) ;;
  *)
    installed_under_home "go/go-*/go/bin/go" ||
      fail "home leg: no Go under ${putnami_home}/toolchains/go after the Go path ran without one on PATH"
    ;;
esac
for workspace_dir in "$first_use_dir" "$go_dir"; do
  for workspace_go in "$workspace_dir"/.putnami/extensions/@putnami-go/libs/go-*; do
    if [ -e "$workspace_go" ]; then
      fail "home leg: ${workspace_go} is a copy of Go inside a workspace; Go must be installed once under ${putnami_home}/toolchains/go"
    fi
  done
done

echo "smoke: OK — channel '${channel}' passes the documented block → pages → clean stop → ${check_command} → Go init → serve → ${go_check_command} on ${os}/${arch} (${inside})"
