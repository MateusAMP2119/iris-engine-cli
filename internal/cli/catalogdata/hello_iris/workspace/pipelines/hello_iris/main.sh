#!/bin/sh
# hello_iris - the Iris quickstart sample: a resident worker speaking the
# engine's turn protocol. stdin carries the engine's frames one JSON object per
# line; the worker's FIRST run frame is answered with the seven rainbow colors
# as row frames on stdout plus a done terminal echoing the turn, and every
# later turn answers a bare done -- a quiet turn records nothing, so the loop
# seeds once and parks instead of re-writing the same rows forever. A manual
# run is a fresh one-shot worker, so it seeds again: the engine upserts each
# row into demo.colors on its fixed name primary key, layering a second
# provenance stamp on the same seven rows -- that layering is the provenance
# lesson the tour ends on.
set -eu

# color answers one demo.colors row frame; every field is in the declared writes.
color() {
  printf '{"event":"row","table":"demo.colors","row":{"name":"%s","hex":"%s","wavelength_nm":%s,"noted_at":"%s"}}\n' \
    "$1" "$2" "$3" "$now"
}

turn=0
seeded=""
while read -r line; do
  case "$line" in
  *'"event":"go"'*) turn=$(printf '%s' "$line" | sed 's/.*"turn"://;s/[^0-9].*//') ;;
  *'"event":"run"'*)
    if [ -z "$seeded" ]; then
      seeded=1
      now=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
      color red    '#e81416' 700
      color orange '#ffa500' 620
      color yellow '#faeb36' 580
      color green  '#79c314' 530
      color blue   '#487de7' 470
      color indigo '#4b369d' 445
      color violet '#70369d' 400
    fi
    printf '{"event":"done","turn":%s}\n' "$turn"
    ;;
  esac
done
