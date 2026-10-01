#!/usr/bin/env bash
set -euo pipefail
ROOT="$(git rev-parse --show-toplevel)"
exec "$ROOT/.agents/skills/audit/scripts/scorecard.sh" "$@"
