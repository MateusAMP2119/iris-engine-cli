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
#   IRIS_DEST=<dir>      install into this directory (default ~/.iris/bin)
#   IRIS_ENGINE_SETUP=<local|remote|skip>  answer the engine-setup menu without a prompt
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

# Colors only on terminal, never under NO_COLOR; G1..G6 = ocean gradient rows
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  DIM='\033[2m' GRN='\033[1;32m' CYN='\033[1;36m' YLW='\033[1;33m' RST='\033[0m'
  G1='\033[38;2;102;126;234m' G2='\033[38;2;105;115;219m' G3='\033[38;2;108;106;205m'
  G4='\033[38;2;112;95;191m' G5='\033[38;2;115;86;177m' G6='\033[38;2;118;75;162m'
else
  DIM='' GRN='' CYN='' YLW='' RST=''
  G1='' G2='' G3='' G4='' G5='' G6=''
fi

say() { printf "  %s\n" "$1"; }
ok() { printf "  ${GRN}✓${RST} %s\n" "$1"; }
section() { printf "\n  ${CYN}%s${RST}\n" "$1"; }
kv() { printf "  • %-15s: %s\n" "$1" "$2"; }

# can_prompt: real tty exists; stdin is curl pipe so prompts use /dev/tty
can_prompt() {
  (: </dev/tty >/dev/tty) 2>/dev/null
}

# Banner: pre-rendered oh-my-logo art (no Node dep); ≥128 cols wide, ≥92 stacked, else plain
banner_wide() {
  printf "${G1}%s${RST}\n" '  ██╗  ██████╗   ██╗  ███████╗       ██╗        █████╗   ██╗  ██╗  ███████╗  ██╗  ██╗   ██████╗   ██╗   ██╗  ███████╗  ███████╗'
  printf "${G2}%s${RST}\n" '  ██║  ██╔══██╗  ██║  ██╔════╝       ██║       ██╔══██╗  ██║ ██╔╝  ██╔════╝  ██║  ██║  ██╔═══██╗  ██║   ██║  ██╔════╝  ██╔════╝'
  printf "${G3}%s${RST}\n" '  ██║  ██████╔╝  ██║  ███████╗       ██║       ███████║  █████╔╝   █████╗    ███████║  ██║   ██║  ██║   ██║  ███████╗  █████╗'
  printf "${G4}%s${RST}\n" '  ██║  ██╔══██╗  ██║  ╚════██║       ██║       ██╔══██║  ██╔═██╗   ██╔══╝    ██╔══██║  ██║   ██║  ██║   ██║  ╚════██║  ██╔══╝'
  printf "${G5}%s${RST}\n" '  ██║  ██║  ██║  ██║  ███████║       ███████╗  ██║  ██║  ██║  ██╗  ███████╗  ██║  ██║  ╚██████╔╝  ╚██████╔╝  ███████║  ███████╗'
  printf "${G6}%s${RST}\n" '  ╚═╝  ╚═╝  ╚═╝  ╚═╝  ╚══════╝       ╚══════╝  ╚═╝  ╚═╝  ╚═╝  ╚═╝  ╚══════╝  ╚═╝  ╚═╝   ╚═════╝    ╚═════╝   ╚══════╝  ╚══════╝'
}
banner_stacked() {
  printf "${G1}%s${RST}\n" '  ██╗  ██████╗   ██╗  ███████╗'
  printf "${G2}%s${RST}\n" '  ██║  ██╔══██╗  ██║  ██╔════╝'
  printf "${G3}%s${RST}\n" '  ██║  ██████╔╝  ██║  ███████╗'
  printf "${G4}%s${RST}\n" '  ██║  ██╔══██╗  ██║  ╚════██║'
  printf "${G5}%s${RST}\n" '  ██║  ██║  ██║  ██║  ███████║'
  printf "${G6}%s${RST}\n" '  ╚═╝  ╚═╝  ╚═╝  ╚═╝  ╚══════╝'
  printf '\n'
  printf "${G1}%s${RST}\n" '  ██╗        █████╗   ██╗  ██╗  ███████╗  ██╗  ██╗   ██████╗   ██╗   ██╗  ███████╗  ███████╗'
  printf "${G2}%s${RST}\n" '  ██║       ██╔══██╗  ██║ ██╔╝  ██╔════╝  ██║  ██║  ██╔═══██╗  ██║   ██║  ██╔════╝  ██╔════╝'
  printf "${G3}%s${RST}\n" '  ██║       ███████║  █████╔╝   █████╗    ███████║  ██║   ██║  ██║   ██║  ███████╗  █████╗'
  printf "${G4}%s${RST}\n" '  ██║       ██╔══██║  ██╔═██╗   ██╔══╝    ██╔══██║  ██║   ██║  ██║   ██║  ╚════██║  ██╔══╝'
  printf "${G5}%s${RST}\n" '  ███████╗  ██║  ██║  ██║  ██╗  ███████╗  ██║  ██║  ╚██████╔╝  ╚██████╔╝  ███████║  ███████╗'
  printf "${G6}%s${RST}\n" '  ╚══════╝  ╚═╝  ╚═╝  ╚═╝  ╚═╝  ╚══════╝  ╚═╝  ╚═╝   ╚═════╝    ╚═════╝   ╚══════╝  ╚══════╝'
}

# Width via stty on /dev/tty: tput inside $() sees pipe, lies 80
cols=${COLUMNS:-80}
if [ -t 1 ]; then
  sz=$(stty size </dev/tty 2>/dev/null || true)
  case "$sz" in
    *' '*) cols=${sz#* } ;;
  esac
fi
printf '\n'
if [ "$cols" -ge 128 ]; then
  banner_wide
elif [ "$cols" -ge 92 ]; then
  banner_stacked
else
  printf "${G1}  IRIS LAKEHOUSE${RST}\n"
fi
printf '\n'

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
printf "  🔍 Detected platform: %s/%s\n" "$os" "$arch"

# Plan first, actions after; existing iris on PATH = announced upgrade
installed=""
if command -v iris >/dev/null 2>&1; then
  installed=$(iris --version 2>/dev/null || true)
fi
dest="${IRIS_DEST:-${HOME}/.iris/bin}"
printf "  📋 Install plan\n"
kv "OS/Arch" "${os} / ${arch}"
kv "Method" "Prebuilt static binary"
kv "Version" "${requested}"
if [ -n "$installed" ]; then
  kv "Installed" "${installed} → upgrading"
fi
if [ -n "${IRIS_DEST:-}" ]; then
  kv "Destination" "${dest}"
else
  kv "Destination" "~/.iris"
fi

asset="iris_${os}_${arch}.tar.gz"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

section "[1/3] Downloading"
say "⬇  Fetching ${asset}"
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
printf "  🔄 Verifying checksum... ${GRN}✓${RST} Verified\n"

section "[2/3] Installing"
say "📦 Extracting & placing binary..."
tar -xzf "${tmp}/${asset}" -C "$tmp"
mkdir -p "$dest"
if [ -w "$dest" ]; then
  mv "${tmp}/iris" "${dest}/iris"
elif command -v sudo >/dev/null 2>&1; then
  say "Installing to ${dest} (sudo required)"
  sudo mv "${tmp}/iris" "${dest}/iris"
else
  echo "iris: cannot write to ${dest}: no permission and no sudo." >&2
  exit 1
fi
bin="${dest}/iris"
# Bubble Tea progress (aligned with uninstall checks) — binary is on disk now.
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  "$bin" ceremony progress "• Placing binary" || true
fi
ok "Installed $("$bin" --version 2>/dev/null || echo iris) → ${bin}"
on_path=""
case ":${PATH}:" in
  *":${dest}:"*) on_path=1 ;;
esac
# Same-shell availability: link from /usr/local/bin (already on PATH) so iris works without a new shell
if [ -z "$on_path" ] && [ -z "${IRIS_DEST:-}" ] && [ -d /usr/local/bin ]; then
  case ":${PATH}:" in
    *:/usr/local/bin:*)
      if [ -w /usr/local/bin ]; then
        ln -sf "$bin" /usr/local/bin/iris && on_path=1
      elif command -v sudo >/dev/null 2>&1 && can_prompt; then
        say "Linking /usr/local/bin/iris (sudo may ask for your password)"
        sudo ln -sf "$bin" /usr/local/bin/iris </dev/tty && on_path=1 || true
      fi
      [ -n "$on_path" ] && ok "Linked /usr/local/bin/iris → ~/.iris/bin/iris"
      ;;
  esac
fi
# Still not reachable: wire the rc once (marked block, idempotent)
if [ -z "$on_path" ]; then
  rc=""
  case "${SHELL:-}" in
    */zsh) rc="${HOME}/.zshrc" ;;
    */bash) rc="${HOME}/.bashrc" ;;
  esac
  if [ -n "$rc" ] && [ -z "${IRIS_DEST:-}" ]; then
    if ! grep -qs '\.iris/bin' "$rc"; then
      printf '\n# iris\nexport PATH="$HOME/.iris/bin:$PATH"\n' >>"$rc"
    fi
    appended="$rc"
    ok "Added ~/.iris/bin to PATH (~/${rc##*/})"
  else
    printf "  ${YLW}!${RST} %s\n" "${dest} is not on your PATH; add: export PATH=\"${dest}:\$PATH\""
  fi
fi
if [ -n "$installed" ]; then
  prev=$(command -v iris 2>/dev/null || true)
  if [ -n "$prev" ] && [ "$prev" != "$bin" ]; then
    printf "  ${YLW}!${RST} %s\n" "another iris remains at ${prev}; remove it or keep ${dest} first in PATH"
  fi
fi

section "[3/3] Engine Setup"
case "$requested" in
  snapshot*)
    printf "  ${YLW}⚠️  Active Development Notice${RST}\n"
    say "This is a snapshot build. Features may change rapidly and some"
    say "functionality is still experimental."
    printf '\n'
    ;;
esac

# Engine setup menu lives in the binary (huh + viper-backed config + BT bars).
# IRIS_ENGINE_SETUP=local|remote|skip still works headless.
# Force a wide enough geometry for the oh-my-logo banner in nested recorders.
if ! "$bin" setup; then
  echo "iris: engine setup failed" >&2
  exit 1
fi

# Fresh PATH append can't reach this shell; ready line must work as pasted
if [ -n "${appended:-}" ]; then
  printf "\n  ✨ Iris is ready! Run: source %s && iris --help\n" "$appended"
  printf "  ${DIM}(new terminals just type: iris)${RST}\n\n"
else
  iris_cmd="$bin"
  if [ -n "$on_path" ]; then
    iris_cmd="iris"
  fi
  printf "\n  ✨ Iris is ready! Try: %s --help\n\n" "$iris_cmd"
fi

# Random quote via awk (POSIX sh has no $RANDOM); attribution right-aligned
n=$(awk 'BEGIN{srand(); print int(rand()*5)+1}')
case "$n" in
  1)
    quote='"He who has a why to live can bear almost any how."'
    author="— Friedrich Nietzsche"
    ;;
  2)
    quote='"No man ever steps in the same river twice."'
    author="— Heraclitus"
    ;;
  3)
    quote='"Well begun is half done."'
    author="— Aristotle"
    ;;
  4)
    quote='"The impediment to action advances action."'
    author="— Marcus Aurelius"
    ;;
  *)
    quote='"Luck is what happens when preparation meets opportunity."'
    author="— Seneca"
    ;;
esac
pad=$((3 + ${#quote} - ${#author}))
if [ "$pad" -lt 3 ]; then pad=3; fi
printf "   %s\n" "$quote"
printf "%*s${DIM}%s${RST}\n" "$pad" "" "$author"
