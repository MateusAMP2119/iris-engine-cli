#!/bin/sh
# word_frequency - an Iris catalog starter: count every word of Emily
# Dickinson's "Hope is the thing with feathers" (Poems, 1891; public domain),
# wholly inside Postgres, into demo.word_counts over the engine-injected
# IRIS_DB_URL. The poem is embedded and fixed, so the counts are
# deterministic: every run upserts the same rows -- fixed word primary keys --
# and layers a new provenance stamp on them.
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
    echo "word_frequency: psql not found on PATH or under a workspace .iris/pg" >&2
    exit 1
  fi
fi

# The whole computation is one SQL statement: split the embedded poem on
# non-letters (apostrophes stay inside words), lowercase, count, upsert.
"$psql" "$IRIS_DB_URL" -v ON_ERROR_STOP=1 <<'SQL'
INSERT INTO demo.word_counts (word, n)
SELECT word, count(*)::int AS n
FROM regexp_split_to_table(
       lower(regexp_replace($poem$
Hope is the thing with feathers
That perches in the soul,
And sings the tune without the words,
And never stops at all,

And sweetest in the gale is heard;
And sore must be the storm
That could abash the little bird
That kept so many warm.

I've heard it in the chillest land,
And on the strangest sea;
Yet, never, in extremity,
It asked a crumb of me.
$poem$, '[^A-Za-z'']+', ' ', 'g')),
       '\s+') AS word
WHERE word <> ''
GROUP BY word
ON CONFLICT (word) DO UPDATE
  SET n = EXCLUDED.n,
      counted_at = now();
SQL
