#!/bin/sh
# Iris installer: fetches the latest release binary for this platform.
# POSIX sh, works with bash, dash, ash/busybox.
#
# Recommended:
#   curl -fsSL https://install.iris-lakehouse.bymarreco.com | bash
#
# Current (raw GitHub):
#   curl -fsSL https://raw.githubusercontent.com/MateusAMP2119/iris-engine-cli/HEAD/install.sh | bash
#
# Knobs:
#   IRIS_VERSION=<tag>   install that release instead of latest
#   IRIS_NO_SETUP=1      install only; never hand off to the setup tour
#   IRIS_FORCE=1         legacy alias of IRIS_NO_SETUP
#   IRIS_BASE_URL=<url>  fetch the asset + checksums from here (local testing)
#   IRIS_DEST=<dir>      install into this directory (local testing)
#   NO_COLOR             plain output
set -eu

REPO="MateusAMP2119/iris-engine-cli"
if [ -n "${IRIS_VERSION:-}" ]; then
  BASE="https://github.com/${REPO}/releases/download/${IRIS_VERSION}"
  requested="${IRIS_VERSION}"
else
  BASE="https://github.com/${REPO}/releases/latest/download"
  requested="latest"
fi
if [ -n "${IRIS_BASE_URL:-}" ]; then
  BASE="${IRIS_BASE_URL}"
  requested="${IRIS_VERSION:-latest} (from ${BASE})"
fi

# Ceremony colors: only on a terminal and never when NO_COLOR is set.
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  DIM='\033[2m'; GRN='\033[1;32m'; CYN='\033[1;36m'; RST='\033[0m'
else
  DIM=''; GRN=''; CYN=''; RST=''
fi

say() { printf "${DIM}·${RST} %s\n" "$1"; }
ok() { printf "${GRN}✓${RST} %s\n" "$1"; }
section() { printf "\n${CYN}%s${RST}\n" "$1"; }
kv() { printf "  ${DIM}%-10s${RST} %s\n" "$1" "$2"; }

# Rainbow banner: Iris is the goddess of the rainbow. Colors only on a terminal
# and never when NO_COLOR is set.
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  printf '\033[1;31m  ██╗██████╗ ██╗███████╗     ██████╗██╗     ██╗\033[0m\n'
  printf '\033[1;33m  ██║██╔══██╗██║██╔════╝    ██╔════╝██║     ██║\033[0m\n'
  printf '\033[1;32m  ██║██████╔╝██║███████╗    ██║     ██║     ██║\033[0m\n'
  printf '\033[1;36m  ██║██╔══██╗██║╚════██║    ██║     ██║     ██║\033[0m\n'
  printf '\033[1;34m  ██║██║  ██║██║███████║    ╚██████╗███████╗██║\033[0m\n'
  printf '\033[1;35m  ╚═╝╚═╝  ╚═╝╚═╝╚══════╝     ╚═════╝╚══════╝╚═╝\033[0m\n'
  printf '\033[2m  lakehouse engine · provenance first\033[0m\n\n'
else
  printf '  Iris CLI: lakehouse engine, provenance first\n\n'
fi

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *)
    echo "iris: unsupported architecture: $arch" >&2
    exit 1
    ;;
esac
case "$os" in
  linux | darwin) ;;
  *)
    echo "iris: unsupported OS: $os (linux and macOS only)" >&2
    exit 1
    ;;
esac
ok "Detected: ${os} (${arch})"

# The install plan: what will happen, before anything does. An iris already on
# PATH turns the run into an announced upgrade.
installed=""
if command -v iris >/dev/null 2>&1; then
  installed=$(iris --version 2>/dev/null || true)
fi
section "Install plan"
kv "OS" "${os} / ${arch}"
kv "Method" "prebuilt static binary — no runtime dependencies"
kv "Version" "${requested}"
if [ -n "$installed" ]; then
  kv "Installed" "${installed} → upgrading in place"
fi
kv "Dest" "${IRIS_DEST:-/usr/local/bin (falls back to ~/.local/bin)}"

asset="iris_${os}_${arch}.tar.gz"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

section "[1/3] Downloading"
say "Fetching ${asset}"
curl -fsSL "${BASE}/${asset}" -o "${tmp}/${asset}"
curl -fsSL "${BASE}/checksums.txt" -o "${tmp}/checksums.txt"

(
  cd "$tmp"
  if command -v sha256sum >/dev/null 2>&1; then
    grep " ${asset}\$" checksums.txt | sha256sum -c - >/dev/null
  else
    grep " ${asset}\$" checksums.txt | shasum -a 256 -c - >/dev/null
  fi
)
ok "Checksum verified"

tar -xzf "${tmp}/${asset}" -C "$tmp"

section "[2/3] Installing"
# Prefer /usr/local/bin; fall back to ~/.local/bin when not writable and sudo is unavailable.
dest="/usr/local/bin"
if [ -n "${IRIS_DEST:-}" ]; then
  dest="${IRIS_DEST}"
  mkdir -p "$dest"
fi
if [ -w "$dest" ]; then
  mv "${tmp}/iris" "${dest}/iris"
elif command -v sudo >/dev/null 2>&1; then
  say "Installing to ${dest} (sudo required)"
  sudo mv "${tmp}/iris" "${dest}/iris"
else
  dest="${HOME}/.local/bin"
  mkdir -p "$dest"
  mv "${tmp}/iris" "${dest}/iris"
  case ":${PATH}:" in
    *":${dest}:"*) ;;
    *) say "NOTE: add ${dest} to your PATH." ;;
  esac
fi
ok "Installed $("${dest}/iris" --version 2>/dev/null || echo iris) → ${dest}/iris"

# One line of personality, then back to work. awk carries the randomness:
# POSIX sh has no $RANDOM.
if [ -n "$installed" ]; then
  line=$(awk 'BEGIN{srand(); print int(rand()*3)+1}')
  case "$line" in
    1) msg="Upgraded. Same rainbow, fewer sharp edges." ;;
    2) msg="Molted clean. The provenance journal remembers everything anyway." ;;
    *) msg="New binary, same memory: every row still answers for itself." ;;
  esac
else
  line=$(awk 'BEGIN{srand(); print int(rand()*4)+1}')
  case "$line" in
    1) msg="Settled in. Your rows will never write anonymously again." ;;
    2) msg="The goddess has landed. Point her at your data." ;;
    3) msg="Installed. Somewhere, a mystery CSV just lost its alibi." ;;
    *) msg="One binary, zero dependencies. The rest is ceremony." ;;
  esac
fi
printf "${DIM}%s${RST}\n" "$msg"

section "[3/3] Starting setup"

# Version gate: probe the installed binary itself before any handoff. A binary
# that answers `quickstart --from-installer --json` gets the full continuation
# handoff; one that only answers plain `quickstart --json` gets the flagless
# handoff (an E15-era tour); anything older gets no handoff and no quickstart
# next-step line -- an old binary is never offered a verb it lacks.
handoff="none"
if "${dest}/iris" quickstart --from-installer --json >/dev/null 2>&1; then
  handoff="continuation"
elif "${dest}/iris" quickstart --json >/dev/null 2>&1; then
  handoff="plain"
fi

# The handoff: no shell question — the tour's own heads-up is the consent, and
# declining it there is the same clean exit. Hand off only with a controlling
# terminal (probed by opening /dev/tty, the uninstaller's rule) and neither
# opt-out set; otherwise print the plain next-steps lines.
can_prompt() {
  [ -z "${IRIS_FORCE:-}" ] && [ -z "${IRIS_NO_SETUP:-}" ] && (: </dev/tty >/dev/tty) 2>/dev/null
}

if [ "$handoff" != "none" ] && can_prompt; then
  say "Handing off to iris — the tour takes it from here (decline there to stop)"
  echo ""
  # The absolute path runs the tour even when dest is not on PATH yet, and the
  # re-tied stdin satisfies its terminal gate -- stdout is never re-tied. exec
  # replaces this shell, so the EXIT trap will not fire: clean up first.
  rm -rf "$tmp"
  trap - EXIT
  if [ "$handoff" = "continuation" ]; then
    exec "${dest}/iris" quickstart --from-installer </dev/tty
  fi
  exec "${dest}/iris" quickstart </dev/tty
fi

# Plain next steps: bare `iris` once dest is on PATH, the absolute path until
# then. The quickstart line rides the version gate: it is offered only when a
# probe passed.
iris_cmd="${dest}/iris"
case ":${PATH}:" in
  *":${dest}:"*) iris_cmd="iris" ;;
esac
if [ "$handoff" != "none" ]; then
  echo "Next: ${iris_cmd} quickstart   # 3-minute guided tour"
  echo "  or: ${iris_cmd} engine install && ${iris_cmd} engine start -d"
else
  echo "Next: ${iris_cmd} engine install && ${iris_cmd} engine start -d"
fi
