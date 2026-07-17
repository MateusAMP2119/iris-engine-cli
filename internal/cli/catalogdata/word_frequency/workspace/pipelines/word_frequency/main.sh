#!/bin/sh
# word_frequency - an Iris catalog starter: a resident worker speaking the
# engine's turn protocol. Emily Dickinson's "Hope is the thing with feathers"
# (Poems, 1891; public domain) is embedded below; plain POSIX text tools split
# it on non-letters (apostrophes stay inside words), lowercase, and count, and
# every run frame is answered with the counts as row frames the engine upserts
# into demo.word_counts on the fixed word primary keys. The poem is fixed, so
# the counts are deterministic: each seeding run produces the same rows and
# layers a new provenance stamp on them. The worker seeds on its FIRST run
# frame and answers later turns with a bare done, so the loop counts once and
# parks; a manual run is a fresh one-shot worker and counts again.
set -eu

# poem prints the embedded 1891 text, verbatim and fixed.
poem() {
  cat <<'POEM'
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
POEM
}

# The whole computation is one pipe: split on non-letters (apostrophes stay
# inside words), lowercase, count. The apostrophe rides a variable so the
# command substitution stays parseable to every /bin/sh.
apos=\'
counts=$(poem | tr -c "A-Za-z$apos" '\n' | tr 'A-Z' 'a-z' | sed '/^$/d' | sort | uniq -c)

turn=0
seeded=""
while read -r line; do
  case "$line" in
  *'"event":"go"'*) turn=$(printf '%s' "$line" | sed 's/.*"turn"://;s/[^0-9].*//') ;;
  *'"event":"run"'*)
    if [ -z "$seeded" ]; then
      seeded=1
      now=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
      printf '%s\n' "$counts" | while read -r n word; do
        printf '{"event":"row","table":"demo.word_counts","row":{"word":"%s","n":%s,"counted_at":"%s"}}\n' \
          "$word" "$n" "$now"
      done
    fi
    printf '{"event":"done","turn":%s}\n' "$turn"
    ;;
  esac
done
