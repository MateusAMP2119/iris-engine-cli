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

# Colors only on terminal, never under NO_COLOR; G1..G6 = ocean gradient rows.
# Use a real ESC byte (not the two-char sequence \033): printf %s prints args
# literally, so a backslash-033 string shows up as garbage and wraps the banner.
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  ESC=$(printf '\033')
  DIM="${ESC}[2m" GRN="${ESC}[1;32m" CYN="${ESC}[1;36m" YLW="${ESC}[1;33m" RST="${ESC}[0m"
  G1="${ESC}[38;2;102;126;234m" G2="${ESC}[38;2;105;115;219m" G3="${ESC}[38;2;108;106;205m"
  G4="${ESC}[38;2;112;95;191m" G5="${ESC}[38;2;115;86;177m" G6="${ESC}[38;2;118;75;162m"
else
  DIM='' GRN='' CYN='' YLW='' RST=''
  G1='' G2='' G3='' G4='' G5='' G6=''
fi

say() { printf "  %s\n" "$1"; }
# ok: prefix-✓ style used only before the binary is on disk. After that, prefer
# ceremony_done so install lines share uninstall's right-aligned [✓] grid.
ok() { printf "  ${GRN}✓${RST} %s\n" "$1"; }
section() { printf "\n  ${CYN}%s${RST}\n" "$1"; }
kv() { printf "  • %-15s: %s\n" "$1" "$2"; }

# Ceremony grid must match internal/cli/progress.go (body 32, mark 21 = bar+pct).
# ceremony_done prints "  • {label}…[✓]" via the installed binary when available.
ceremony_done() {
  label=$1
  if [ -n "${bin:-}" ] && [ -x "$bin" ]; then
    "$bin" ceremony done "$label" 2>/dev/null || ok "$label"
    return
  fi
  ok "$label"
}
ceremony_quote() {
  if [ -n "${bin:-}" ] && [ -x "$bin" ]; then
    "$bin" ceremony quote 2>/dev/null || true
    return
  fi
}
# can_prompt: real tty exists; stdin is curl pipe so prompts use /dev/tty
can_prompt() {
  (: </dev/tty >/dev/tty) 2>/dev/null
}

# banner_line prints one gradient banner row ($1 = color, $2 = text).
# Color vars must already hold a real ESC byte (see G1..G6 setup above).
banner_line() {
  printf '%s%s%s\n' "$1" "$2" "${RST}"
}

# Banner: pre-rendered oh-my-logo art (no Node dep); ≥128 cols wide, ≥92 stacked, else plain
banner_wide() {
  banner_line "${G1}" '  ██╗  ██████╗   ██╗  ███████╗       ██╗        █████╗   ██╗  ██╗  ███████╗  ██╗  ██╗   ██████╗   ██╗   ██╗  ███████╗  ███████╗'
  banner_line "${G2}" '  ██║  ██╔══██╗  ██║  ██╔════╝       ██║       ██╔══██╗  ██║ ██╔╝  ██╔════╝  ██║  ██║  ██╔═══██╗  ██║   ██║  ██╔════╝  ██╔════╝'
  banner_line "${G3}" '  ██║  ██████╔╝  ██║  ███████╗       ██║       ███████║  █████╔╝   █████╗    ███████║  ██║   ██║  ██║   ██║  ███████╗  █████╗'
  banner_line "${G4}" '  ██║  ██╔══██╗  ██║  ╚════██║       ██║       ██╔══██║  ██╔═██╗   ██╔══╝    ██╔══██║  ██║   ██║  ██║   ██║  ╚════██║  ██╔══╝'
  banner_line "${G5}" '  ██║  ██║  ██║  ██║  ███████║       ███████╗  ██║  ██║  ██║  ██╗  ███████╗  ██║  ██║  ╚██████╔╝  ╚██████╔╝  ███████║  ███████╗'
  banner_line "${G6}" '  ╚═╝  ╚═╝  ╚═╝  ╚═╝  ╚══════╝       ╚══════╝  ╚═╝  ╚═╝  ╚═╝  ╚═╝  ╚══════╝  ╚═╝  ╚═╝   ╚═════╝    ╚═════╝   ╚══════╝  ╚══════╝'
}
banner_stacked() {
  banner_line "${G1}" '  ██╗  ██████╗   ██╗  ███████╗'
  banner_line "${G2}" '  ██║  ██╔══██╗  ██║  ██╔════╝'
  banner_line "${G3}" '  ██║  ██████╔╝  ██║  ███████╗'
  banner_line "${G4}" '  ██║  ██╔══██╗  ██║  ╚════██║'
  banner_line "${G5}" '  ██║  ██║  ██║  ██║  ███████║'
  banner_line "${G6}" '  ╚═╝  ╚═╝  ╚═╝  ╚═╝  ╚══════╝'
  printf '\n'
  banner_line "${G1}" '  ██╗        █████╗   ██╗  ██╗  ███████╗  ██╗  ██╗   ██████╗   ██╗   ██╗  ███████╗  ███████╗'
  banner_line "${G2}" '  ██║       ██╔══██╗  ██║ ██╔╝  ██╔════╝  ██║  ██║  ██╔═══██╗  ██║   ██║  ██╔════╝  ██╔════╝'
  banner_line "${G3}" '  ██║       ███████║  █████╔╝   █████╗    ███████║  ██║   ██║  ██║   ██║  ███████╗  █████╗'
  banner_line "${G4}" '  ██║       ██╔══██║  ██╔═██╗   ██╔══╝    ██╔══██║  ██║   ██║  ██║   ██║  ╚════██║  ██╔══╝'
  banner_line "${G5}" '  ███████╗  ██║  ██║  ██║  ██╗  ███████╗  ██║  ██║  ╚██████╔╝  ╚██████╔╝  ███████║  ███████╗'
  banner_line "${G6}" '  ╚══════╝  ╚═╝  ╚═╝  ╚═╝  ╚═╝  ╚══════╝  ╚═╝  ╚═╝   ╚═════╝    ╚═════╝   ╚══════╝  ╚══════╝'
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
  banner_line "${G1}" "  IRIS LAKEHOUSE"
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
say "• Fetching ${asset}"
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
# Binary not on disk yet — compact inline check (ceremony grid starts after install).
printf "  • Verifying checksum... ${GRN}✓${RST} Verified\n"

section "[2/3] Installing"
say "• Extracting binary..."
tar -xzf "${tmp}/${asset}" -C "$tmp"
mkdir -p "$dest"
if [ -w "$dest" ]; then
  mv "${tmp}/iris" "${dest}/iris"
elif command -v sudo >/dev/null 2>&1; then
  say "• Installing to ${dest} (sudo required)"
  sudo mv "${tmp}/iris" "${dest}/iris"
else
  echo "iris: cannot write to ${dest}: no permission and no sudo." >&2
  exit 1
fi
bin="${dest}/iris"
# Bubble Tea progress + done lines share uninstall's ceremony grid.
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  "$bin" ceremony progress "• Placing binary" || true
fi
ceremony_done "Binary installed"

on_path=""
case ":${PATH}:" in
  *":${dest}:"*) on_path=1 ;;
esac
# Same-shell availability without root: prefer a user-writable dir already on
# PATH (~/.local/bin), then a writable /usr/local/bin. Never sudo — a root-owned
# link can't be uninstalled by the same user later.
if [ -z "$on_path" ] && [ -z "${IRIS_DEST:-}" ]; then
  link_path=""
  case ":${PATH}:" in
    *:"${HOME}/.local/bin":*)
      if mkdir -p "${HOME}/.local/bin" 2>/dev/null && [ -w "${HOME}/.local/bin" ]; then
        link_path="${HOME}/.local/bin/iris"
      fi
      ;;
  esac
  if [ -z "$link_path" ]; then
    case ":${PATH}:" in
      *:/usr/local/bin:*)
        if [ -w /usr/local/bin ]; then
          link_path="/usr/local/bin/iris"
        fi
        ;;
    esac
  fi
  if [ -n "$link_path" ]; then
    say "• Linking ${link_path}"
    # Prefer a hard link (same inode) so reinstalls refresh the stable PATH
    # entry in place; fall back to symlink. Primary shim is always user-owned.
    if ln -f "$bin" "$link_path" 2>/dev/null || ln -sf "$bin" "$link_path" 2>/dev/null; then
      on_path=1
      ceremony_done "PATH linked"
    fi
  fi
  # Same-shell `… && iris …`: bash may still hash `iris` → /usr/local/bin/iris from
  # an older install. When passwordless sudo is available, refresh that path so the
  # parent shell does not need `hash -r`. Uninstall removes it with `sudo -n` when
  # needed (no password prompt). Skip entirely when sudo -n is unavailable.
  case ":${PATH}:" in
    *:/usr/local/bin:*)
      if [ ! -w /usr/local/bin ] && command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
        sudo -n ln -sfn "$bin" /usr/local/bin/iris 2>/dev/null || true
      fi
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
    ceremony_done "PATH configured"
  else
    printf "  ${YLW}!${RST} %s\n" "${dest} is not on your PATH; add: export PATH=\"${dest}:\$PATH\""
  fi
fi
if [ -n "$installed" ]; then
  prev=$(command -v iris 2>/dev/null || true)
  if [ -n "$prev" ] && [ "$prev" != "$bin" ]; then
    # Hard-linked PATH shim shares an inode with $bin — not a second install.
    same=0
    if [ -e "$prev" ] && [ -e "$bin" ]; then
      iprev=$(ls -i "$prev" 2>/dev/null | awk '{print $1}')
      ibin=$(ls -i "$bin" 2>/dev/null | awk '{print $1}')
      [ -n "$iprev" ] && [ "$iprev" = "$ibin" ] && same=1
    fi
    if [ "$same" -eq 0 ]; then
      printf "  ${YLW}!${RST} %s\n" "another iris remains at ${prev}; remove it or keep ${dest} first in PATH"
    fi
  fi
fi

section "[3/3] Engine Setup"
case "$requested" in
  snapshot*)
    printf "  ${YLW}!${RST} Snapshot build — features may change; some are experimental.\n"
    printf '\n'
    ;;
esac

# Engine setup menu lives in the binary (huh + viper-backed config + BT bars).
# IRIS_ENGINE_SETUP=local|remote|skip still works headless.
if ! "$bin" setup; then
  echo "iris: engine setup failed" >&2
  exit 1
fi

# Ready line: prefer the short name when a PATH shim is in place; absolute path
# always works as a paste fallback.
printf "\n  ✨ Iris is ready! Try: "
if [ -n "${on_path:-}" ]; then
  printf "iris --help\n"
else
  printf "%s --help\n" "$bin"
fi
if [ -n "${appended:-}" ]; then
  printf "  ${DIM}New PATH in %s — this shell: source %s${RST}\n" "$appended" "$appended"
fi
printf '\n'

# Farewell quote: same wrap + ceremony-edge author as `iris uninstall`.
ceremony_quote

# Scrollable review when the transcript is taller than the terminal.
