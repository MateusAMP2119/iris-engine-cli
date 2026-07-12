#!/bin/sh
# system_snapshot - an Iris catalog starter: collect uname and df facts about
# the machine the engine runs on and upsert them into demo.machine_facts over
# the engine-injected IRIS_DB_URL. The fact names are fixed primary keys, so
# re-running refreshes the same rows and layers a new provenance stamp; a fact
# this machine cannot answer degrades to 'unavailable' rather than failing.
set -eu

# Find psql: PATH first, else the managed Postgres of the nearest workspace
# (walk upward to the closest .iris/pg/bin/psql).
if command -v psql >/dev/null 2>&1; then
  psql=psql
else
  psql=""
  dir=$(pwd)
  while [ "$dir" != "/" ]; do
    if [ -x "$dir/.iris/pg/bin/psql" ]; then
      psql="$dir/.iris/pg/bin/psql"
      break
    fi
    dir=$(dirname "$dir")
  done
  if [ -z "$psql" ]; then
    echo "system_snapshot: psql not found on PATH or under a workspace .iris/pg" >&2
    exit 1
  fi
fi

# Collect the facts, degrading to 'unavailable' when a probe has no answer.
os=$(uname -s 2>/dev/null) || os=""
arch=$(uname -m 2>/dev/null) || arch=""
hostname=$(uname -n 2>/dev/null) || hostname=""
disk=$(df -P / 2>/dev/null | awk 'NR==2 {print $5}') || disk=""
[ -n "$os" ] || os=unavailable
[ -n "$arch" ] || arch=unavailable
[ -n "$hostname" ] || hostname=unavailable
[ -n "$disk" ] || disk=unavailable

"$psql" "$IRIS_DB_URL" -v ON_ERROR_STOP=1 \
  -v os="$os" -v arch="$arch" -v hostname="$hostname" -v disk="$disk" <<'SQL'
INSERT INTO demo.machine_facts (fact, value) VALUES
  ('os',                :'os'),
  ('arch',              :'arch'),
  ('hostname',          :'hostname'),
  ('disk_used_percent', :'disk')
ON CONFLICT (fact) DO UPDATE
  SET value = EXCLUDED.value,
      collected_at = now();
SQL
