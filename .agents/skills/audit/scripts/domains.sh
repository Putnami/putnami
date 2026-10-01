#!/usr/bin/env bash
# Audit domains, read from the workspace's top-level scopes.
#
# A domain is a top-level directory whose putnami.json declares the scope
# schema (https://putnami.dev/schemas/putnami-scope.json). Its projects are
# that scope's `includes`, printed as workspace paths (/<domain>/<include>).
#
# Usage:
#   domains.sh                 # JSON object: {"<domain>": ["/<domain>/<include>", ...], ...}
#   domains.sh list            # domain names, one per line
#   domains.sh paths <domain>  # the domain's project paths, one per line
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
SCOPE_SCHEMA="https://putnami.dev/schemas/putnami-scope.json"

command -v jq >/dev/null || { echo "domains: jq not found" >&2; exit 1; }

domains_json() {
  local f
  for f in "$ROOT"/*/putnami.json; do
    [ -f "$f" ] || continue
    jq -c --arg schema "$SCOPE_SCHEMA" --arg d "$(basename "$(dirname "$f")")" \
      'select(."$schema" == $schema) | {($d): [(.includes // [])[] | "/\($d)/\(.)"]}' "$f"
  done | jq -s 'add // {}'
}

case "${1:-}" in
  "") domains_json ;;
  list) domains_json | jq -r 'keys_unsorted[]' ;;
  paths)
    domain="${2:?domains: paths needs a domain}"
    domains_json | jq -r --arg d "$domain" 'if has($d) then .[$d][] else error("unknown") end' 2>/dev/null || {
      echo "domains: unknown domain '$domain'" >&2
      exit 2
    }
    ;;
  -h|--help) sed -n '2,11p' "$0" | sed 's/^# \{0,1\}//' ;;
  *) echo "domains: unknown argument '$1'" >&2; exit 2 ;;
esac
