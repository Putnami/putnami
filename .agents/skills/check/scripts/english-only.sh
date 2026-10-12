#!/usr/bin/env bash
# The installed audit helper invokes this canonical path. Keep its argument,
# stdin and exit-status contract while the native CLI owns the language rule.
set -euo pipefail

if [ -x ./putnamiw ]; then
  exec ./putnamiw contributor language "$@"
fi
exec putnami contributor language "$@"
