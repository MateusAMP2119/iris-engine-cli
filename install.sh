#!/bin/sh
# Iris installer: fetches the latest release binary for this platform.
# POSIX sh, works with bash, dash, ash/busybox.
#
# Recommended:
#   curl -fsSL https://install.iris-lakehouse.bymarreco.com | bash
#   curl -fsSL https://install.iris-lakehouse.bymarreco.com/snapshot | bash
#
# Current (raw GitHub):
#   curl -fsSL https://raw.githubusercontent.com/MateusAMP2119/iris-lakehouse/HEAD/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/MateusAMP2119/iris-lakehouse/HEAD/install.sh | bash -s snapshot
#
# Knobs:
#   first argument       release tag to install ("snapshot" → rolling development build)
#   IRIS_VERSION=<tag>   same as the argument; the argument wins if both are set
#   IRIS_BASE_URL=<url>  fetch the asset + checksums from here (local testing)
#   IRIS_DEST=<dir>      install into this directory (local testing)
#   NO_COLOR             plain output
set -eu

REPO="MateusAMP2119/iris-lakehouse"
if [ "$#" -gt 0 ] && [ -n "$1" ]; then
  IRIS_VERSION="$1"
fi
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

section "[1/2] Downloading"
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

section "[2/2] Installing"
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

# Plain next steps: bare `iris` once dest is on PATH, the absolute path until
# then.
iris_cmd="${dest}/iris"
case ":${PATH}:" in
  *":${dest}:"*) iris_cmd="iris" ;;
esac
echo "Next: ${iris_cmd} engine install && ${iris_cmd} engine start -d"
