#!/usr/bin/env bash
# Runs putnami observe for the Putnami/observe-action composite action.
# Inputs arrive as PUTNAMI_OBSERVE_* variables; the step's own environment
# (RUNNER_OS, RUNNER_TEMP, GITHUB_OUTPUT, HOME) comes from the runner. The two
# tokens reach putnami observe only, never the installer or the extension
# installs.
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
run_token="${PUTNAMI_OBSERVE_GITHUB_TOKEN:-}"
if [[ -n "${PUTNAMI_CLOUD_TOKEN:-}" && -z "$token" ]]; then
  echo "::warning title=Putnami Observe::PUTNAMI_CLOUD_TOKEN in the job environment is not read; pass it as the token input." >&2
fi
unset PUTNAMI_OBSERVE_TOKEN PUTNAMI_OBSERVE_GITHUB_TOKEN GITHUB_TOKEN PUTNAMI_CLOUD_TOKEN

publish="${PUTNAMI_OBSERVE_PUBLISH:-}"
case "$publish" in
  '') if [[ -n "$token" ]]; then publish=true; else publish=false; fi ;;
  true | false) ;;
  *) fail "publish is \"$publish\": use true, false, or leave it empty." ;;
esac

# A channel follows the channel alphabet of putnami init; a version names no
# release set, since the release lines do not share one version. stable reads
# latest, as putnami upgrade and putnami init read it.
channel="${PUTNAMI_OBSERVE_CHANNEL:-}"
channel="${channel:-stable}"
if [[ "$channel" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+([.+-].*)?$ ]] || ! [[ "$channel" =~ ^[a-z0-9][a-z0-9._-]{0,63}$ ]]; then
  fail "channel is \"$channel\": name a release channel, such as stable or canary, not a version."
fi
registry_channel="$channel"
if [[ "$channel" == stable ]]; then
  registry_channel=latest
fi

curl -fsSL "$INSTALL_URL" | bash -s -- --version "$registry_channel"
export PATH="$HOME/.putnami/bin:$PATH"
putnami extensions install --user "@putnami/observe@$registry_channel"
if [[ -n "$token" ]]; then
  putnami extensions install --user "@putnami/cloud@$registry_channel"
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

if [[ -n "$token" ]]; then
  args+=(--upload)
fi
if [[ "$publish" == true ]]; then
  args+=(--publish)
fi

(
  if [[ -n "$token" ]]; then
    export PUTNAMI_CLOUD_TOKEN="$token"
  fi
  # --publish without --upload posts nothing, so the run token stays out.
  if [[ "$publish" == true && -n "$token" ]]; then
    export GITHUB_TOKEN="$run_token"
  fi
  exec putnami "${args[@]}"
)

if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  echo "evidence=$evidence" >>"$GITHUB_OUTPUT"
fi
