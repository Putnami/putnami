#!/usr/bin/env bash
# putnami-jsonl.sh — JSONL event helpers for Putnami extensions.
#
# Source this file from your job scripts:
#   source "$(dirname "$0")/putnami-jsonl.sh"
#
# Then use the emit_* functions to produce structured events on stdout.
#
# STREAM VERSION: every line is stamped `"v":2`. One stream speaks one runtime
# event protocol version (protocols/runtime), and putnami advertises the version
# it accepts in PUTNAMI_RUNTIME_EVENTS — since CLI contract 3 that is v2, and
# nothing lower is accepted. These helpers emit only v2 vocabulary, so the
# version is a constant here rather than a value read back from the environment:
# answering with a version this file cannot actually speak would be worse than
# not answering at all.

# Timestamp in ISO 8601 format.
_putnami_now() {
  date -u +"%Y-%m-%dT%H:%M:%S.000Z" 2>/dev/null || date -u +"%Y-%m-%dT%H:%M:%SZ"
}

# Emit a raw JSON line.
_putnami_emit() {
  echo "$1"
}

# Escape a string for safe JSON embedding.
_putnami_escape() {
  local s="$1"
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  s="${s//$'\n'/\\n}"
  s="${s//$'\t'/\\t}"
  echo -n "$s"
}

# --- Public API ---

# emit_meta <extension> <job> [version]
emit_meta() {
  local ext="$1" job="$2" ver="${3:-}"
  local data="{\"extension\":\"$(_putnami_escape "$ext")\",\"job\":\"$(_putnami_escape "$job")\"}"
  if [ -n "$ver" ]; then
    data="{\"extension\":\"$(_putnami_escape "$ext")\",\"job\":\"$(_putnami_escape "$job")\",\"version\":\"$(_putnami_escape "$ver")\"}"
  fi
  _putnami_emit "{\"v\":2,\"type\":\"meta\",\"time\":\"$(_putnami_now)\",\"level\":\"info\",\"message\":\"Starting $(_putnami_escape "$job")\",\"data\":$data}"
}

# emit_log <level> <message>
# Levels: debug, info, warn, error
emit_log() {
  _putnami_emit "{\"v\":2,\"type\":\"log\",\"time\":\"$(_putnami_now)\",\"level\":\"$1\",\"message\":\"$(_putnami_escape "$2")\"}"
}

# emit_phase_start <name>
emit_phase_start() {
  _putnami_emit "{\"v\":2,\"type\":\"phase\",\"time\":\"$(_putnami_now)\",\"name\":\"$(_putnami_escape "$1")\",\"action\":\"start\"}"
}

# emit_phase_end <name> <status>
# Status: success, failed, skipped
emit_phase_end() {
  _putnami_emit "{\"v\":2,\"type\":\"phase\",\"time\":\"$(_putnami_now)\",\"name\":\"$(_putnami_escape "$1")\",\"action\":\"end\",\"status\":\"$2\"}"
}

# emit_progress <current> <total> <message>
emit_progress() {
  _putnami_emit "{\"v\":2,\"type\":\"progress\",\"time\":\"$(_putnami_now)\",\"current\":$1,\"total\":$2,\"message\":\"$(_putnami_escape "$3")\"}"
}

# emit_diagnostic <severity> <message> [file] [line]
# Severity: error, warning, info, hint
emit_diagnostic() {
  local sev="$1" msg="$2" file="${3:-}" line="${4:-0}"
  if [ -n "$file" ]; then
    _putnami_emit "{\"v\":2,\"type\":\"diagnostic\",\"time\":\"$(_putnami_now)\",\"severity\":\"$sev\",\"message\":\"$(_putnami_escape "$msg")\",\"location\":{\"file\":\"$(_putnami_escape "$file")\",\"line\":$line}}"
  else
    _putnami_emit "{\"v\":2,\"type\":\"diagnostic\",\"time\":\"$(_putnami_now)\",\"severity\":\"$sev\",\"message\":\"$(_putnami_escape "$msg")\"}"
  fi
}

# emit_metric <name> <value> <unit>
# Units: ms, bytes, count, percent, custom
emit_metric() {
  _putnami_emit "{\"v\":2,\"type\":\"metric\",\"time\":\"$(_putnami_now)\",\"name\":\"$(_putnami_escape "$1")\",\"value\":$2,\"unit\":\"$3\"}"
}

# emit_artifact <id> <name> <kind> <path>
# Kinds: file, directory, report, coverage, bundle, list, custom
emit_artifact() {
  _putnami_emit "{\"v\":2,\"type\":\"artifact\",\"time\":\"$(_putnami_now)\",\"id\":\"$(_putnami_escape "$1")\",\"name\":\"$(_putnami_escape "$2")\",\"kind\":\"$3\",\"path\":\"$(_putnami_escape "$4")\"}"
}

# emit_result <status> [data_json]
# Status: OK, FAILED, SKIP
emit_result() {
  local status="$1" data="${2:-}"
  if [ -n "$data" ]; then
    _putnami_emit "{\"v\":2,\"type\":\"result\",\"time\":\"$(_putnami_now)\",\"level\":\"info\",\"message\":\"Job $status\",\"data\":{\"status\":\"$status\",\"data\":$data}}"
  else
    _putnami_emit "{\"v\":2,\"type\":\"result\",\"time\":\"$(_putnami_now)\",\"level\":\"info\",\"message\":\"Job $status\",\"data\":{\"status\":\"$status\"}}"
  fi
}

# --- Context parsing helpers ---

# parse_context <context_file>
# The context-file argument is part of the canonical job invocation. This helper
# reads standard fields from orchestrator-projected PUTNAMI_* env vars rather
# than parsing that JSON, then exports PUTNAMI_CTX_*.
parse_context() {
  export PUTNAMI_CTX_WORKSPACE_ROOT="${PUTNAMI_WORKSPACE_ROOT:-}"
  export PUTNAMI_CTX_WORKSPACE_NAME="${PUTNAMI_WORKSPACE_NAME:-}"
  export PUTNAMI_CTX_PROJECT_NAME="${PUTNAMI_PROJECT_NAME:-}"
  export PUTNAMI_CTX_PROJECT_PATH="${PUTNAMI_PROJECT_PATH:-${PUTNAMI_PROJECT_ROOT:-}}"
  export PUTNAMI_CTX_EXTENSION_NAME="${PUTNAMI_EXTENSION_NAME:-}"
  export PUTNAMI_CTX_JOB_NAME="${PUTNAMI_JOB_NAME:-}"
  export PUTNAMI_CTX_OUTPUT_PATH="${PUTNAMI_OUTPUT_PATH:-}"
  export PUTNAMI_CTX_CACHE_ROOT="${PUTNAMI_CACHE_ROOT:-}"
}
