#!/usr/bin/env bash
# The installed audit helper invokes this canonical path. Keep its argument,
# stdin and exit-status contract while the native CLI owns the language rule.
# The wrapper is found at the Git root, so the script works from any directory.
set -euo pipefail

root="$(git rev-parse --show-toplevel 2>/dev/null || true)"
if [ -n "$root" ] && [ -x "$root/putnamiw" ]; then
  exec "$root/putnamiw" contributor language "$@"
fi
exec putnami contributor language "$@"
