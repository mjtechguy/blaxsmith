#!/usr/bin/env bash
# NODE. Owner-only preview DB dump before migrations 0029+ (not reversible by old binaries).
source "$(dirname "$0")/env.sh"
umask 077
out=$STATE/preview-db-pre-$S.dump
kubectl -n blaxsmith-preview exec preview-postgres-0 -- sh -c 'pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc' > "$out"
test "$(head -c5 "$out")" = PGDMP && ls -l "$out"
