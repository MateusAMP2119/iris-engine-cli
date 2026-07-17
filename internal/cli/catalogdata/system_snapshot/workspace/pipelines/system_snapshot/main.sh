#!/bin/sh
# system_snapshot - an Iris catalog starter: a resident worker speaking the
# engine's turn protocol. The worker's FIRST run frame is answered with fresh
# uname and df facts about the machine the engine runs on, as row frames the
# engine upserts into demo.machine_facts on the fixed fact primary keys; every
# later turn answers a bare done, so the loop snapshots once and parks. A
# manual run is a fresh one-shot worker, so it probes again -- re-running
# refreshes the same rows and layers a new provenance stamp. A fact this
# machine cannot answer degrades to 'unavailable' rather than failing the turn.
set -eu

# jesc makes a probed value safe inside a JSON string (backslash, double quote).
jesc() { printf '%s' "$1" | sed 's/\\/\\\\/g;s/"/\\"/g'; }

# fact answers one demo.machine_facts row frame; every field is in the declared writes.
fact() {
  printf '{"event":"row","table":"demo.machine_facts","row":{"fact":"%s","value":"%s","collected_at":"%s"}}\n' \
    "$1" "$(jesc "$2")" "$now"
}

turn=0
seeded=""
while read -r line; do
  case "$line" in
  *'"event":"go"'*) turn=$(printf '%s' "$line" | sed 's/.*"turn"://;s/[^0-9].*//') ;;
  *'"event":"run"'*)
    if [ -z "$seeded" ]; then
      seeded=1
      # Probe fresh on the seeding turn, degrading to 'unavailable' when a probe has no answer.
      os=$(uname -s 2>/dev/null) || os=""
      arch=$(uname -m 2>/dev/null) || arch=""
      hostname=$(uname -n 2>/dev/null) || hostname=""
      disk=$(df -P / 2>/dev/null | awk 'NR==2 {print $5}') || disk=""
      [ -n "$os" ] || os=unavailable
      [ -n "$arch" ] || arch=unavailable
      [ -n "$hostname" ] || hostname=unavailable
      [ -n "$disk" ] || disk=unavailable
      now=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
      fact os "$os"
      fact arch "$arch"
      fact hostname "$hostname"
      fact disk_used_percent "$disk"
    fi
    printf '{"event":"done","turn":%s}\n' "$turn"
    ;;
  esac
done
