#!/usr/bin/env bash
# Runs putnami observe for the Putnami/observe-action composite action.
# Inputs arrive as PUTNAMI_OBSERVE_* variables; the step's own environment
# (RUNNER_OS, RUNNER_TEMP, GITHUB_OUTPUT, HOME) comes from the runner.
set -euo pipefail

INSTALL_URL="https://putnami.dev/install.sh"

fail() {
  echo "::error title=Putnami Observe::$1" >&2
  exit 1
}

case "${RUNNER_OS:-}" in
  Windows) fail "Putnami/observe-action runs on Linux and macOS runners, such as ubuntu-latest or macos-latest." ;;
esac

token="${PUTNAMI_OBSERVE_TOKEN:-}"
channel="${PUTNAMI_OBSERVE_CHANNEL:-}"
channel="${channel:-stable}"

publish="${PUTNAMI_OBSERVE_PUBLISH:-}"
case "$publish" in
  '') if [[ -n "$token" ]]; then publish=true; else publish=false; fi ;;
  true | false) ;;
  *) fail "publish is \"$publish\": use true, false, or leave it empty." ;;
esac

curl -fsSL "$INSTALL_URL" | bash -s -- --version "$channel"
export PATH="$HOME/.putnami/bin:$PATH"
putnami extensions install --user "@putnami/observe@$channel"
if [[ -n "$token" ]]; then
  putnami extensions install --user "@putnami/cloud@$channel"
fi

evidence="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/observe-evidence.json"
args=(observe --evidence "$evidence")
add_flag() {
  if [[ -n "$2" ]]; then
    args+=("$1" "$2")
  fi
}
add_flag --junit "${PUTNAMI_OBSERVE_JUNIT:-}"
add_flag --coverage "${PUTNAMI_OBSERVE_COVERAGE:-}"
add_flag --sbom "${PUTNAMI_OBSERVE_SBOM:-}"
add_flag --base-sbom "${PUTNAMI_OBSERVE_BASE_SBOM:-}"
add_flag --base "${PUTNAMI_OBSERVE_BASE:-}"

unset GITHUB_TOKEN PUTNAMI_CLOUD_TOKEN
if [[ -n "$token" ]]; then
  args+=(--upload)
  export PUTNAMI_CLOUD_TOKEN="$token"
fi
if [[ "$publish" == true ]]; then
  args+=(--publish)
  export GITHUB_TOKEN="${PUTNAMI_OBSERVE_GITHUB_TOKEN:-}"
fi
unset PUTNAMI_OBSERVE_TOKEN PUTNAMI_OBSERVE_GITHUB_TOKEN

putnami "${args[@]}"

if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  echo "evidence=$evidence" >>"$GITHUB_OUTPUT"
fi
