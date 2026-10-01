#!/usr/bin/env bash
# Post-release smoke check for the putnami CLI: the whole public golden path.
#
# It runs what a new user runs, in this order, against the real download
# channel: install with the public install URL, find the command the way a shell
# finds it, initialize the public TypeScript web starter, serve it, answer one real
# HTTP request, and terminate. Any leg that fails, fails the release and prints
# the evidence for that leg.
#
# It guards these regressions:
#   - the CLI was packaged every release but never uploaded, so
#     put.putnami.dev/putnami/cli/download returned 404 and `upgrade` could only
#     ever fetch a stale binary.
#   - a stale cross-compile cache hit shipped a binary whose embedded
#     --version did not match the release tag it was published under.
#   - init could report success after a required setup failure, while the
#     release smoke never exercised a real starter.
#   - the public installer printed "verified" next to a digest it had
#     compared to nothing, and the smoke never ran the installer at all. The
#     install leg below is install.sh itself, and the release fails if the
#     registry stops advertising a digest, if the advertised digest does not
#     describe the bytes served, if the installer stops checking it, or if the
#     installer tries to escalate privileges.
#
# With SMOKE_RUN_COMMAND set, a last leg runs the run form of the installer,
# `curl -fsSL "<installer URL>?run=<command>" | bash`, in a fresh `git init`
# directory that is not a workspace. It requires exit status 0 and a directory
# git reports as untouched. The command must be listed in the command map
# published next to the installer and must not wait for input.
#
# Usage:   smoke-check-release.sh [channel]   (default channel: latest)
# Env:
#   PUTNAMI_REGISTRY_URL  registry base   (default https://put.putnami.dev)
#   SMOKE_INSTALL_URL     installer URL (default: https://putnami.dev/install.sh)
#   SMOKE_RETRIES         GET attempts before giving up (default 5)
#   SMOKE_STARTUP_TIMEOUT seconds to wait for starter readiness (default 180)
#   SMOKE_DIAGNOSTICS_DIR copy bounded failure evidence under this directory
#   SMOKE_RUN_COMMAND     also run this command through the installer's run form
#                         (default: unset, no run leg)

set -euo pipefail

channel="${1:-latest}"
base="${PUTNAMI_REGISTRY_URL:-https://put.putnami.dev}"
base="${base%/}"
retries="${SMOKE_RETRIES:-5}"
startup_timeout="${SMOKE_STARTUP_TIMEOUT:-180}"
installer_url="${SMOKE_INSTALL_URL:-https://putnami.dev/install.sh}"
run_command="${SMOKE_RUN_COMMAND:-}"
diagnostics_dir="${SMOKE_DIAGNOSTICS_DIR:-}"
diagnostic_byte_limit=262144
launch_dir="$(pwd -P)"
case "$diagnostics_dir" in
  '' | /*) ;;
  *) diagnostics_dir="${launch_dir}/${diagnostics_dir}" ;;
esac

case "$startup_timeout" in
  '' | *[!0-9]*)
    echo "::error::SMOKE_STARTUP_TIMEOUT must be a non-negative integer, got '${startup_timeout}'"
    exit 1
    ;;
esac
case "$retries" in
  '' | *[!0-9]* | 0)
    echo "::error::SMOKE_RETRIES must be a positive integer, got '${retries}'"
    exit 1
    ;;
esac
# The installer and the site accept the same command names; the letters are
# spelled out so the check does not depend on the runner's locale.
run_command_pattern='^[abcdefghijklmnopqrstuvwxyz][abcdefghijklmnopqrstuvwxyz0123456789-]{0,63}$'
if [ -n "$run_command" ]; then
  if ! [[ "$run_command" =~ $run_command_pattern ]]; then
    echo "::error::SMOKE_RUN_COMMAND must match ^[a-z][a-z0-9-]{0,63}\$, got '${run_command}'"
    exit 1
  fi
  if ! command -v git >/dev/null 2>&1; then
    echo "::error::prerequisites leg: SMOKE_RUN_COMMAND needs git to prove the run leg leaves its directory untouched"
    exit 1
  fi
fi

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
esac
case "${os}/${arch}" in
  darwin/amd64 | darwin/arm64 | linux/amd64 | linux/arm64) ;;
  *)
    echo "::error::platform leg: unsupported smoke runner ${os}/${arch}; required matrix is darwin|linux x amd64|arm64"
    exit 1
    ;;
esac

url="${base}/putnami/cli/download?channel=${channel}&os=${os}&arch=${arch}"

workdir="$(mktemp -d)"
workspace=""
server_pid=""

terminate_server() {
  if [ -z "$server_pid" ]; then
    return
  fi

  forced=0
  if kill -0 "$server_pid" 2>/dev/null; then
    kill -TERM "$server_pid" 2>/dev/null || true
    # The starter's graceful shutdown budget is 10s; leave enough headroom for
    # the CLI to cancel and reap its job process before using the hard fallback.
    shutdown_attempt=0
    while [ "$shutdown_attempt" -lt 200 ]; do
      if ! kill -0 "$server_pid" 2>/dev/null; then
        break
      fi
      sleep 0.1
      shutdown_attempt=$((shutdown_attempt + 1))
    done
  fi
  if kill -0 "$server_pid" 2>/dev/null; then
    forced=1
    kill -KILL "$server_pid" 2>/dev/null || true
  fi
  wait "$server_pid" 2>/dev/null || true
  server_pid=""
  [ "$forced" -eq 0 ]
}

capture_failure_artifacts() {
  [ -n "$diagnostics_dir" ] || return 0

  write_bounded_head() {
    LC_ALL=C awk -v max_lines="$1" -v max_bytes="$diagnostic_byte_limit" '
      BEGIN { remaining = max_bytes }
      NR > max_lines || remaining <= 0 { exit }
      {
        line = $0 ORS
        if (length(line) <= remaining) {
          printf "%s", line
          remaining -= length(line)
          next
        }
        printf "%s", substr(line, 1, remaining)
        exit
      }
    ' "$2" >"$3"
  }

  safe_channel="$(printf '%s' "$channel" | tr -c 'A-Za-z0-9._-' '_')"
  [ -n "$safe_channel" ] || safe_channel=channel
  target="${diagnostics_dir%/}/${safe_channel}-${os}-${arch}"
  mkdir -p "$target"
  for log in install init inspect serve run; do
    if [ -f "$workdir/${log}.log" ]; then
      tail -n 200 "$workdir/${log}.log" | tail -c "$diagnostic_byte_limit" >"$target/${log}.log"
    elif [ -f "$workdir/${log}.jsonl" ]; then
      tail -n 200 "$workdir/${log}.jsonl" | tail -c "$diagnostic_byte_limit" >"$target/${log}.jsonl"
    fi
  done
  if [ -n "$workspace" ] && [ -d "$workspace" ]; then
    for path in \
      putnami.workspace.json putnami.lock.json package.json bun.lock \
      webapp/putnami.json webapp/package.json; do
      if [ -f "$workspace/$path" ]; then
        name="$(printf '%s' "$path" | tr '/' '_')"
        write_bounded_head 400 "$workspace/$path" "$target/$name"
      fi
    done
    # .npmrc is deliberately represented only by the bounded file list below:
    # even a rejected auth directive must never be copied into an artifact.
    workspace_file_list="$workdir/workspace-files.all"
    find "$workspace" \
      \( -type d \( -name node_modules -o -name .putnami -o -name .git \) -prune \) -o \
      -type f -print 2>/dev/null | sort >"$workspace_file_list" || true
    write_bounded_head 400 "$workspace_file_list" "$target/workspace-files.txt"
  fi
  echo "smoke: bounded failure diagnostics written to ${target}"
}

cleanup() {
  status=$?
  trap - EXIT
  terminate_server || true
  if [ "$status" -ne 0 ]; then
    capture_failure_artifacts || true
  fi
  if [ -n "$workdir" ] && [ -d "$workdir" ]; then
    if cd "$launch_dir" 2>/dev/null; then
      rm -rf "$workdir"
    else
      echo "::error::cleanup leg: cannot leave the smoke workdir; refusing to remove ${workdir}"
      [ "$status" -ne 0 ] || status=1
    fi
  fi
  exit "$status"
}

trap cleanup EXIT
trap 'exit 130' INT TERM

# Establish the neutral machine before the first network request. In particular,
# curl must not read the operator's HOME config or proxy settings during the
# channel preflight and then run the installer under a different environment.
workspace="$workdir/workspace"
smoke_home="$workdir/home"
mkdir -p "$workspace" "$smoke_home" "$workdir/store" "$workdir/artifacts" \
  "$workdir/config" "$workdir/cache" "$workdir/data" "$workdir/state"
cd "$workspace"
export HOME="$smoke_home"
export CURL_HOME="$workdir/config/curl"
# ~/.local/bin is deliberately absent in the neutral HOME: the installer must
# create it because the standard user path already names it, then make the next
# exact command discoverable.
export PATH="$smoke_home/.local/bin:$PATH"
export XDG_CONFIG_HOME="$workdir/config"
export XDG_CACHE_HOME="$workdir/cache"
export XDG_DATA_HOME="$workdir/data"
export XDG_STATE_HOME="$workdir/state"
bash_path="${BASH:-}"
if [ -z "$bash_path" ] || [ ! -x "$bash_path" ]; then
  echo "::error::prerequisites leg: the release smoke must run under an executable Bash"
  exit 1
fi
export SHELL="$bash_path"
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_COUNT=0
export GIT_CONFIG_PARAMETERS=""
# Package-manager routing is rebuilt from scratch below. In particular, an
# ambient registry override must not redirect the starter's dependency install.
for package_env_name in "${!BUN_@}" "${!NPM_CONFIG_@}"; do
  [ -n "$package_env_name" ] || continue
  unset "$package_env_name"
done
export NPM_CONFIG_USERCONFIG=/dev/null
export NPM_CONFIG_CACHE="$workdir/cache/npm"
export DOCKER_CONFIG="$workdir/config/docker"
export BUN_INSTALL="$workdir/data/bun"
export BUN_INSTALL_CACHE_DIR="$workdir/cache/bun"
# Fail closed against both current and future Putnami overrides. The harness
# captured its channel/registry inputs above; everything under this prefix is
# now rebuilt from the neutral workdir or an explicit disabled value.
for putnami_env_name in "${!PUTNAMI_@}"; do
  [ -n "$putnami_env_name" ] || continue
  unset "$putnami_env_name"
done
export PUTNAMI_BUN_CACHE_DIR="$workdir/cache/putnami-bun"
export PUTNAMI_STORE_DIR="$workdir/store"
export PUTNAMI_ARTIFACT_DIR="$workdir/artifacts"
export PUTNAMI_NO_RELAUNCH=1
export PUTNAMI_REGISTRY_URL="$base"
export PUTNAMI_VERSION="$channel"
export PUTNAMI_CACHE_URL=""
export PUTNAMI_CACHE_TOKEN=""
export PUTNAMI_CLOUD_TOKEN=""
export PUTNAMI_TELEMETRY=off
export PUTNAMI_TELEMETRY_ENDPOINT=""
export DO_NOT_TRACK=1
export CONFIG_SERVER_URL=""
export CONFIG_SERVER_TOKEN=""
export PUTNAMI_TOKEN=""
export GOOGLE_APPLICATION_CREDENTIALS=""
export AWS_ACCESS_KEY_ID=""
export AWS_SECRET_ACCESS_KEY=""
export AWS_SESSION_TOKEN=""
export AWS_PROFILE=""
export AWS_CONFIG_FILE=""
export AWS_SHARED_CREDENTIALS_FILE=""
export GITHUB_TOKEN=""
export GH_TOKEN=""
export NPM_TOKEN=""
export NODE_AUTH_TOKEN=""
export BUN_AUTH_TOKEN=""
export SSH_AUTH_SOCK=""
export GIT_ASKPASS=""
export SSH_ASKPASS=""
export BASH_ENV=""
export ENV=""
export HTTP_PROXY=""
export HTTPS_PROXY=""
export ALL_PROXY=""
export NO_PROXY=""
export http_proxy=""
export https_proxy=""
export all_proxy=""
export no_proxy=""

# The TypeScript extension installs dependencies and serves the starter through
# Bun. Name that prerequisite before touching the release channel instead of
# turning a missing runtime into a much later, less useful init failure.
bun_path="$(command -v bun || true)"
if [ -z "$bun_path" ]; then
  echo "::error::prerequisites leg: Bun v1.4.0 or later is required for the TypeScript web init/serve golden path"
  exit 1
fi
bun_version="$(bun --version 2>/dev/null | head -n 1 || true)"
bun_core="${bun_version%%[-+]*}"
IFS=. read -r bun_major bun_minor bun_patch _ <<<"$bun_core"
bun_minor="${bun_minor:-0}"
bun_patch="${bun_patch:-0}"
case "${bun_major}.${bun_minor}.${bun_patch}" in
  *[!0-9.]* | .* | *..* | *.)
    echo "::error::prerequisites leg: ${bun_path} reports unsupported version '${bun_version:-<none>}'; Bun v1.4.0 or later is required"
    exit 1
    ;;
esac
if [ "$bun_major" -lt 1 ] || \
  { [ "$bun_major" -eq 1 ] && [ "$bun_minor" -lt 4 ]; }; then
  echo "::error::prerequisites leg: ${bun_path} reports unsupported version '${bun_version}'; Bun v1.4.0 or later is required"
  exit 1
fi
echo "smoke: prerequisites include Bun ${bun_version} at ${bun_path}"
echo "smoke: GET ${url}"

# ── Leg 1: the channel resolves, and it advertises what the installer needs ──
#
# The installer is fail-closed, so a release that stops advertising a digest
# would break every new install. That is a release failure, not an install
# failure, and it is cheaper to name here than to read out of an install log.
#
# A freshly-published artifact may take a moment to propagate; retry transient
# non-200s before failing the release.
status="000"
resolved=""
advertised=""

# A header this response does not carry is a finding, not a failure to look.
# Under `set -o pipefail` an unmatched grep would abort the whole check with no
# ::error:: line at all — silently turning the one release failure this leg
# exists to name into an unexplained exit 1.
read_response_header() {
  grep -i "^$1:" "$workdir/headers" 2>/dev/null | tr -d '\r' | awk '{print $2}' || true
}

attempt=1
while [ "$attempt" -le "$retries" ]; do
  status="000"
  if ! status="$(curl -sS -o "$workdir/asset" -D "$workdir/headers" -w '%{http_code}' "$url")"; then
    status="000"
  fi
  if [ "$status" = "200" ]; then
    resolved="$(read_response_header x-resolved-version)"
    advertised="$(read_response_header x-integrity)"
    if [ -z "$advertised" ]; then
      advertised="$({ grep -i '^digest:' "$workdir/headers" 2>/dev/null || true; } |
        tr -d '\r' | sed -n 's/.*[Ss][Hh][Aa]-256=\([^,]*\).*/\1/p')"
    fi
    break
  fi
  if [ "$attempt" -ge "$retries" ]; then
    break
  fi
  echo "smoke: attempt ${attempt}/${retries} got HTTP ${status}; retrying in 10s..."
  sleep 10
  attempt=$((attempt + 1))
done

if [ "$status" != "200" ]; then
  echo "::error::channel leg: CLI download channel '${channel}' returned HTTP ${status} for ${os}/${arch} — the CLI binary was not published"
  exit 1
fi
if [ -z "$resolved" ]; then
  echo "::error::channel leg: CLI download channel '${channel}' returned no X-Resolved-Version header"
  exit 1
fi
if [ -z "$advertised" ]; then
  echo "::error::channel leg: CLI download channel '${channel}' advertised no SHA-256 (X-Integrity or RFC 9530 Digest) — the fail-closed installer refuses this release"
  sed -n '1,40p' "$workdir/headers" || true
  exit 1
fi

# The registry must agree with itself. If the advertised digest does not describe
# the bytes it just streamed, every install fails the integrity check; naming it
# here costs one hash and saves reading it out of an install log.
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$workdir/asset" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "$workdir/asset" | awk '{print $1}')"
else
  echo "::error::channel leg: no sha256sum or shasum on this host, so the release cannot be verified"
  exit 1
fi
expected="$(printf '%s' "$advertised" | sed -e 's/^[Ss][Hh][Aa]-\{0,1\}256[:-]//' | tr '[:upper:]' '[:lower:]')"
if [ "$actual" != "$expected" ]; then
  echo "::error::channel leg: channel '${channel}' advertises ${expected} but streamed bytes hashing to ${actual} — the fail-closed installer refuses this release"
  exit 1
fi
echo "smoke: channel '${channel}' resolved ${resolved}, advertised ${advertised}, bytes match"

# ── Leg 2: install with the published installer ─────────────────────────────
#
# A curl-pipe installer that prompts for a password is one a user cannot audit
# before it runs, so the install leg runs with a `sudo` on PATH that records the
# attempt and fails. The marker must not exist afterwards. It is scoped to this
# leg: later legs run the CLI, which has its own privilege rules.
fake_bin="$workdir/fakebin"
sudo_marker="$workdir/sudo-was-invoked"
mkdir -p "$fake_bin"
for escalator in sudo doas pkexec run0; do
  cat >"$fake_bin/$escalator" <<EOF
#!/bin/sh
printf '%s %s\n' "$escalator" "\$*" >> "$sudo_marker"
exit 1
EOF
  chmod +x "$fake_bin/$escalator"
done

install_log="$workdir/install.log"
echo "smoke: curl -fsSL ${installer_url} | bash"
if [ "$installer_url" = "https://putnami.dev/install.sh" ]; then
  if ! (
    export PATH="$fake_bin:$PATH"
    curl -fsSL https://putnami.dev/install.sh | bash
  ) >"$install_log" 2>&1; then
    echo "::error::install leg: public installer failed against channel '${channel}'"
    tail -n 200 "$install_log" || true
    exit 1
  fi
elif ! (
  export PATH="$fake_bin:$PATH"
  curl -fsSL "$installer_url" | bash
) >"$install_log" 2>&1; then
  echo "::error::install leg: ${installer_url} failed against channel '${channel}'"
  tail -n 200 "$install_log" || true
  exit 1
fi

if [ -f "$sudo_marker" ]; then
  echo "::error::install leg: the installer escalated privileges — a curl-pipe install must never prompt for a password"
  cat "$sudo_marker" || true
  exit 1
fi

# The installer prints "Integrity verified" only after comparing the download to
# the digest the registry advertised. A regression that drops the comparison —
# or silently takes the PUTNAMI_UNSAFE_INSTALL escape hatch — fails here.
if ! grep -q 'Integrity verified' "$install_log"; then
  echo "::error::install leg: the installer did not report a verified digest — the public install path may no longer verify what it downloads"
  tail -n 200 "$install_log" || true
  exit 1
fi
if grep -q 'without integrity verification' "$install_log"; then
  echo "::error::install leg: the installer took the unverified path (PUTNAMI_UNSAFE_INSTALL) — a release must never need it"
  tail -n 200 "$install_log" || true
  exit 1
fi
echo "smoke: installer verified the download against the advertised digest"

# ── Leg 3: same-shell discovery ─────────────────────────────────────────────
bin="$(command -v putnami || true)"
if [ -z "$bin" ]; then
  echo "::error::discovery leg: 'putnami' is not runnable in the shell that ran the installer"
  tail -n 200 "$install_log" || true
  exit 1
fi
case "$bin" in
  "$workdir"/*) ;;
  *)
    echo "::error::discovery leg: 'putnami' resolved to ${bin}, outside the smoke workdir — the smoke would be testing an ambient install"
    exit 1
    ;;
esac
echo "smoke: discovered ${bin}"
if [ ! -x "$bin" ]; then
  echo "::error::platform leg: installed ${bin} is not executable"
  exit 1
fi

# The independently fetched candidate bytes are for this normalized host
# target. Requiring the installed executable to have the same digest proves the
# installer selected that target rather than an ambient or wrong-architecture
# artifact; running --version below proves the selected bytes execute here.
if command -v sha256sum >/dev/null 2>&1; then
  installed_digest="$(sha256sum "$bin" | awk '{print $1}')"
else
  installed_digest="$(shasum -a 256 "$bin" | awk '{print $1}')"
fi
if [ "$installed_digest" != "$actual" ]; then
  echo "::error::platform leg: installed ${os}/${arch} executable hashes to ${installed_digest}, candidate channel asset hashes to ${actual}"
  exit 1
fi
echo "smoke: selected executable ${os}/${arch}, mode executable, digest ${installed_digest}"

# ── Leg 4: the installed binary is the version the channel resolved ──
# `|| true` for the same reason read_response_header carries it: under
# `set -o pipefail` a binary that exits non-zero — or takes SIGPIPE from head —
# fails the pipeline and `set -e` kills the run with no ::error:: line, making
# the ${reported:-<none>} fallback below unreachable in exactly the case it was
# written for.
reported="$("$bin" --version 2>/dev/null | head -n1 | awk '{print $NF}' || true)"
echo "smoke: installed binary reports ${reported:-<none>}"

# Compare versions, tolerating a leading v on either side.
if [ "${reported#v}" != "${resolved#v}" ]; then
  echo "::error::stamp leg: channel '${channel}' resolves ${resolved} but the installed binary reports ${reported:-<none>}"
  exit 1
fi

# ── Leg 5: the exact public TypeScript starter initializes ──────────────────
init_log="$workdir/init.log"
echo "smoke: putnami init --project webapp --extension ts"
if ! (
  putnami init --project webapp --extension ts </dev/null
) >"$init_log" 2>&1; then
  echo "::error::init leg: published CLI failed the exact public TypeScript init command"
  tail -n 200 "$init_log" || true
  exit 1
fi

require_generated_file() {
  if [ ! -s "$workspace/$1" ]; then
    echo "::error::init leg: generated workspace is missing non-empty $1"
    tail -n 200 "$init_log" || true
    exit 1
  fi
}

require_generated_text() {
  path="$1"
  text="$2"
  if ! grep -Fq "$text" "$workspace/$path"; then
    echo "::error::init leg: generated $path does not contain required state: $text"
    if [ "$path" != ".npmrc" ]; then
      sed -n '1,200p' "$workspace/$path" || true
    fi
    exit 1
  fi
}

for path in \
  putnami.workspace.json putnami.lock.json package.json bun.lock .npmrc \
  CLAUDE.md AGENTS.md .mcp.json webapp/putnami.json webapp/package.json; do
  require_generated_file "$path"
done
require_generated_text putnami.workspace.json '"@putnami/typescript"'
require_generated_text putnami.workspace.json '"typescript-web"'
require_generated_text putnami.workspace.json '"webapp"'
require_generated_text putnami.lock.json '"@putnami/typescript"'
require_generated_text putnami.lock.json '"typescript-web"'
require_generated_text webapp/putnami.json '"name": "webapp"'
require_generated_text webapp/putnami.json '"@putnami/typescript"'
require_generated_text AGENTS.md 'This is a Putnami workspace.'
require_generated_text .mcp.json '"putnami"'
npm_auth_key="$(awk -F= '
  {
    key = $1
    lowered = tolower(key)
    if (lowered ~ /(_authtoken|_auth)[[:space:]]*$/) {
      sub(/^.*:/, "", key)
      gsub(/[[:space:]]/, "", key)
      print key
      exit
    }
  }
' "$workspace/.npmrc")"
if [ -n "$npm_auth_key" ]; then
  echo "::error::init leg: generated .npmrc contains authentication directive ${npm_auth_key}; the public starter must install without credentials"
  exit 1
fi
require_generated_text .npmrc '@putnami:registry=https://npm.putnami.dev'
if grep -Fq '@putnami/cloud' \
  "$workspace/putnami.workspace.json" "$workspace/putnami.lock.json" \
  "$workspace/package.json" "$workspace/bun.lock" "$workspace/.npmrc" \
  "$workspace/webapp/putnami.json" "$workspace/webapp/package.json"; then
  echo "::error::init leg: public starter unexpectedly depends on private @putnami/cloud state"
  exit 1
fi

inspect_log="$workdir/inspect.jsonl"
if ! (
  putnami workspace describe --output=jsonl
  putnami projects describe webapp --output=jsonl
  putnami extensions list --output=jsonl
  putnami build --projects webapp --plan --output=jsonl
) >"$inspect_log" 2>&1; then
  echo "::error::init leg: generated workspace/project/extension state did not validate through the installed CLI"
  tail -n 200 "$inspect_log" || true
  exit 1
fi
if ! grep -Fq '@putnami/typescript' "$inspect_log"; then
  echo "::error::init leg: machine-readable inspection did not report @putnami/typescript"
  tail -n 200 "$inspect_log" || true
  exit 1
fi
echo "smoke: generated workspace, webapp, locks, extension and MCP registration verified; build plan validates"

serve_log="$workdir/serve.jsonl"
# Create the log before the server starts: the redirection below opens it only
# once the background subshell runs, and polling a missing file makes sed exit
# 2, which set -e and pipefail turn into a silent exit 2 of the whole script.
: >"$serve_log"
echo "smoke: putnami serve webapp"
(
  export PORT=0
  export PUTNAMI_OUTPUT=jsonl
  exec putnami serve webapp
) >"$serve_log" 2>&1 &
server_pid=$!

# The framework reports the listener's actual OS-assigned port through the
# typed ready event. Waiting on that event avoids both fixed-port collisions and
# a check-then-bind race, while the deadline keeps release failures bounded.
ready_port=""
deadline=$((SECONDS + startup_timeout))
while [ -z "$ready_port" ]; do
  ready_port="$(sed -n 's/.*"type":"ready".*"port":\([0-9][0-9]*\).*/\1/p' "$serve_log" | tail -n 1)"
  if [ -n "$ready_port" ]; then
    break
  fi
  # `serve` watches by default and remains resident after a failed run so it can
  # retry on changes. A completed session before readiness is nevertheless a
  # terminal smoke failure; do not spend the rest of the deadline waiting on a
  # watcher that has already reported its verdict.
  if grep -q '"record":"session:end"' "$serve_log"; then
    echo "::error::serve leg: webapp's initial serve session ended before readiness"
    tail -n 200 "$serve_log" || true
    exit 1
  fi
  if ! kill -0 "$server_pid" 2>/dev/null; then
    set +e
    wait "$server_pid"
    server_status=$?
    set -e
    server_pid=""
    echo "::error::serve leg: webapp exited with status ${server_status} before readiness"
    tail -n 200 "$serve_log" || true
    exit 1
  fi
  if [ "$SECONDS" -ge "$deadline" ]; then
    echo "::error::serve leg: webapp did not emit readiness within ${startup_timeout}s"
    tail -n 200 "$serve_log" || true
    exit 1
  fi
  sleep 0.25
done

response_file="$workdir/response.html"
echo "smoke: GET http://127.0.0.1:${ready_port}/"
http_status="000"
if ! http_status="$(curl -sS --connect-timeout 5 --max-time 10 \
  -o "$response_file" -w '%{http_code}' "http://127.0.0.1:${ready_port}/")"; then
  echo "::error::http leg: webapp did not answer after its ready event (HTTP ${http_status:-000})"
  tail -n 200 "$serve_log" || true
  exit 1
fi
case "$http_status" in
  2??) ;;
  *)
    echo "::error::http leg: webapp returned HTTP ${http_status}; expected an explicit 2xx response"
    tail -n 200 "$serve_log" || true
    exit 1
    ;;
esac

if [ ! -s "$response_file" ]; then
  echo "::error::http leg: webapp returned an empty successful response"
  tail -n 200 "$serve_log" || true
  exit 1
fi

if ! terminate_server; then
  echo "::error::shutdown leg: webapp did not stop within the graceful shutdown budget"
  tail -n 200 "$serve_log" || true
  exit 1
fi
listener_stopped=0
shutdown_probe=0
while [ "$shutdown_probe" -lt 20 ]; do
  if ! curl -sS --connect-timeout 1 --max-time 1 \
    -o /dev/null "http://127.0.0.1:${ready_port}/" >/dev/null 2>&1; then
    listener_stopped=1
    break
  fi
  sleep 0.1
  shutdown_probe=$((shutdown_probe + 1))
done
if [ "$listener_stopped" -ne 1 ]; then
  echo "::error::shutdown leg: HTTP listener still answers after the CLI exited; the starter may be orphaned"
  tail -n 200 "$serve_log" || true
  exit 1
fi
echo "smoke: HTTP listener is unreachable after clean stop"

# ── Optional leg: run one command without a workspace ───────────────────────
#
# The run form installs the CLI, pins the extension the command map names for
# the user, and runs the command in the caller's directory, which it must never
# write to. The caller here is a fresh `git init` directory outside the starter
# workspace: git's view of it (changes, untracked and ignored files) and its
# listing (git does not report an empty directory) must both be unchanged.
if [ -n "$run_command" ]; then
  run_dir="$workdir/run-scenario"
  run_log="$workdir/run.log"
  mkdir -p "$run_dir"
  if ! git -C "$run_dir" init -q >"$run_log" 2>&1; then
    echo "::error::run leg: git init failed in ${run_dir}"
    tail -n 200 "$run_log" || true
    exit 1
  fi
  run_status=0
  echo "smoke: curl -fsSL \"${installer_url}?run=${run_command}\" | bash"
  if [ "$installer_url" = "https://putnami.dev/install.sh" ]; then
    (
      cd "$run_dir"
      export PATH="$fake_bin:$PATH"
      curl -fsSL "https://putnami.dev/install.sh?run=${run_command}" | bash
    ) >>"$run_log" 2>&1 || run_status=$?
  else
    # The installer reads the command map published next to it; an installer
    # served from elsewhere is paired with the map served beside it.
    (
      cd "$run_dir"
      export PATH="$fake_bin:$PATH"
      export PUTNAMI_COMMAND_MAP_URL="${installer_url%/*}/install-commands.txt"
      curl -fsSL "${installer_url}?run=${run_command}" | bash
    ) >>"$run_log" 2>&1 || run_status=$?
  fi
  if [ "$run_status" -ne 0 ]; then
    echo "::error::run leg: putnami ${run_command} through ${installer_url}?run=${run_command} exited ${run_status}"
    tail -n 200 "$run_log" || true
    exit 1
  fi
  if [ -f "$sudo_marker" ]; then
    echo "::error::run leg: the installer escalated privileges — a curl-pipe install must never prompt for a password"
    cat "$sudo_marker" || true
    exit 1
  fi
  run_changes="$(git -C "$run_dir" status --porcelain --untracked-files=all --ignored 2>&1)" || {
    echo "::error::run leg: git status failed in ${run_dir}"
    printf '%s\n' "$run_changes"
    exit 1
  }
  run_entries="$(ls -A "$run_dir")"
  if [ -n "$run_changes" ] || [ "$run_entries" != ".git" ]; then
    echo "::error::run leg: putnami ${run_command} changed the directory it ran in; the run form must leave the caller's directory untouched"
    printf '%s\n' "$run_changes" | head -n 50 || true
    printf '%s\n' "$run_entries" | head -n 50 || true
    tail -n 200 "$run_log" || true
    exit 1
  fi
  echo "smoke: putnami ${run_command} exited 0 and left ${run_dir} untouched"
  echo "smoke: OK — channel '${channel}' passes curl install → TypeScript init → serve → HTTP → clean stop → run ${run_command} on ${os}/${arch}"
  exit 0
fi
echo "smoke: OK — channel '${channel}' passes curl install → TypeScript init → serve → HTTP → clean stop on ${os}/${arch}"
