#!/bin/sh
# Generate deploy/credentials.json with random secrets for each counterparty.
# Usage: scripts/gen-credentials.sh [CLIENT1 DEMO ...]
set -eu
out="deploy/credentials.json"
[ -e "$out" ] && { echo "$out already exists; remove it first" >&2; exit 1; }
[ $# -eq 0 ] && set -- CLIENT1 DEMO

umask 077
first=1
{
  echo "{"
  for cp in "$@"; do
    [ $first -eq 1 ] || echo ","
    first=0
    printf '  "%s": {"username": "%s", "secret": "%s", "password": "%s"}' \
      "$cp" "$(openssl rand -hex 8)" "$(openssl rand -hex 32)" "$(openssl rand -hex 12)"
  done
  echo
  echo "}"
} > "$out"
echo "wrote $out (mode 600). Give each counterparty its own entry; it is not shown again."
