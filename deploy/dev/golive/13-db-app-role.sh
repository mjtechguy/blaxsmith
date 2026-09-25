#!/usr/bin/env bash
# NODE. Move the preview app off the Postgres bootstrap superuser onto a
# NOSUPERUSER NOBYPASSRLS owner role, so row-level security (0140/0190/0200)
# actually applies. Idempotent. The new password is generated here, written
# only into the preview-app-db Secret, and never printed.
# Usage: S=<sha7> bash 13-db-app-role.sh   (take 07-db-backup.sh first)
source "$(dirname "$0")/env.sh"
NS=blaxsmith-preview ROLE=blaxsmith_app
k() { kubectl -n "$NS" "$@"; }
psql_su() { k exec -i preview-postgres-0 -- sh -c 'psql -v ON_ERROR_STOP=1 -qAt -U "$POSTGRES_USER" -d "${POSTGRES_DB:-$POSTGRES_USER}"'; }

url=$(k get secret preview-app-db -o jsonpath='{.data.url}' | base64 -d)
current=$(sed -E 's#^postgres(ql)?://([^:]+):.*#\2#' <<<"$url")
if [ "$current" = "$ROLE" ]; then
  echo "preview-app-db already uses $ROLE"
else
  umask 077; t=$(mktemp -d); trap 'rm -rf "$t"' EXIT
  openssl rand -hex 32 > "$t/pw"
  replicas=$(k get deploy preview-app -o jsonpath='{.spec.replicas}')
  k scale deploy preview-app --replicas=0 && k rollout status deploy preview-app --timeout=120s
  { printf "\\set pw '%s'\n" "$(<"$t/pw")"; cat <<'SQL'
DO $$BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='blaxsmith_app') THEN
    CREATE ROLE blaxsmith_app LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB;
  END IF;
END$$;
ALTER ROLE blaxsmith_app WITH LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD :'pw';
SELECT format('ALTER DATABASE %I OWNER TO blaxsmith_app', current_database()) \gexec
ALTER SCHEMA public OWNER TO blaxsmith_app;
DO $$DECLARE r record; BEGIN
  FOR r IN SELECT c.oid::regclass AS o, c.relkind FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
           WHERE n.nspname='public' AND c.relkind IN ('r','p','v','m','S','f')
           AND NOT (c.relkind='S' AND EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid=c.oid AND d.deptype IN ('a','i'))) LOOP
    EXECUTE format(CASE r.relkind WHEN 'S' THEN 'ALTER SEQUENCE %s OWNER TO blaxsmith_app'
      WHEN 'v' THEN 'ALTER VIEW %s OWNER TO blaxsmith_app' WHEN 'm' THEN 'ALTER MATERIALIZED VIEW %s OWNER TO blaxsmith_app'
      WHEN 'f' THEN 'ALTER FOREIGN TABLE %s OWNER TO blaxsmith_app' ELSE 'ALTER TABLE %s OWNER TO blaxsmith_app' END, r.o);
  END LOOP;
  FOR r IN SELECT p.oid::regprocedure AS f FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public' LOOP
    EXECUTE format('ALTER ROUTINE %s OWNER TO blaxsmith_app', r.f);
  END LOOP;
  FOR r IN SELECT t.oid::regtype AS ty FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace
           WHERE n.nspname='public' AND t.typtype IN ('e','d','c') AND NOT EXISTS (SELECT 1 FROM pg_class c WHERE c.reltype=t.oid) LOOP
    EXECUTE format('ALTER TYPE %s OWNER TO blaxsmith_app', r.ty);
  END LOOP;
END$$;
SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname='public' AND c.relkind IN ('r','p') AND pg_get_userbyid(c.relowner)<>'blaxsmith_app';
SQL
  } | psql_su | tail -1 | grep -qx 0 || { echo "some tables are not owned by $ROLE" >&2; exit 2; }
  # The password only travels through the owner-only temp file, never argv.
  k get secret preview-app-db -o json | python3 -c '
import base64, json, pathlib, re, sys
role, pw = sys.argv[1], pathlib.Path(sys.argv[2]).read_text().strip()
d = json.load(sys.stdin)
url = base64.b64decode(d["data"]["url"]).decode()
new, n = re.subn(r"^(postgres(?:ql)?://)[^@]+@", lambda m: m.group(1) + role + ":" + pw + "@", url)
assert n == 1
d["data"]["url"] = base64.b64encode(new.encode()).decode()
for k in ("resourceVersion", "uid", "creationTimestamp"): d["metadata"].pop(k, None)
json.dump(d, sys.stdout)' "$ROLE" "$t/pw" | k replace -f - >/dev/null
  k scale deploy preview-app --replicas="${replicas:-1}"
  echo "preview-app-db now uses $ROLE"
fi
echo "role check: $(psql_su <<<"SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname='$ROLE'")  (want f|f)"
