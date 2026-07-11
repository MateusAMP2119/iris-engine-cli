#!/bin/sh
# hello_iris - the Iris quickstart sample: upsert the seven rainbow colors into
# demo.colors over the engine-injected IRIS_DB_URL. The primary keys are fixed,
# so re-running the pipeline layers a second provenance stamp on the same seven
# rows -- that layering is the provenance lesson the tour ends on.
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
    echo "hello_iris: psql not found on PATH or under a workspace .iris/pg" >&2
    exit 1
  fi
fi

"$psql" "$IRIS_DB_URL" -v ON_ERROR_STOP=1 <<'SQL'
INSERT INTO demo.colors (name, hex, wavelength_nm) VALUES
  ('red',    '#e81416', 700),
  ('orange', '#ffa500', 620),
  ('yellow', '#faeb36', 580),
  ('green',  '#79c314', 530),
  ('blue',   '#487de7', 470),
  ('indigo', '#4b369d', 445),
  ('violet', '#70369d', 400)
ON CONFLICT (name) DO UPDATE
  SET hex = EXCLUDED.hex,
      wavelength_nm = EXCLUDED.wavelength_nm,
      noted_at = now();
SQL
