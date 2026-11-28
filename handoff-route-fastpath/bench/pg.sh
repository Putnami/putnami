#!/usr/bin/env bash
# The bench PostgreSQL: its own cluster on 127.0.0.1:55432, database "bench".
set -euo pipefail
PGB=$(ls -d /usr/lib/postgresql/*/bin | sort -V | tail -1)
D=/var/lib/postgresql/bench
PORT=${BENCH_PG_PORT:-55432}
case "${1:-start}" in
  start)
    if [ ! -f "$D/PG_VERSION" ]; then
      mkdir -p "$D" && chown postgres:postgres "$D"
      su postgres -c "$PGB/initdb -D $D -U postgres --auth=trust -E UTF8" >/dev/null
      cat >> "$D/postgresql.conf" <<CONF
port = $PORT
listen_addresses = '127.0.0.1'
unix_socket_directories = '/tmp'
max_connections = 200
shared_buffers = 256MB
fsync = off
synchronous_commit = off
full_page_writes = off
CONF
    fi
    if ! su postgres -c "$PGB/pg_ctl -D $D status" >/dev/null 2>&1; then
      su postgres -c "$PGB/pg_ctl -D $D -l $D/log.txt -w start" >/dev/null
    fi
    psql -h 127.0.0.1 -p "$PORT" -U postgres -tc "SELECT 1 FROM pg_database WHERE datname='bench'" | grep -q 1 \
      || psql -h 127.0.0.1 -p "$PORT" -U postgres -c "CREATE DATABASE bench" >/dev/null
    echo "bench postgres $($PGB/postgres --version) on 127.0.0.1:$PORT" ;;
  stop) su postgres -c "$PGB/pg_ctl -D $D stop" ;;
esac
