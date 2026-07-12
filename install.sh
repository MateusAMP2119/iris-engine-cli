#!/bin/sh
# Iris installer: fetches the latest release binary for this platform.
# POSIX sh, works with bash, dash, ash/busybox:
#   curl -fsSL https://raw.githubusercontent.com/MateusAMP2119/iris-engine-cli/master/install.sh | sh
set -eu

REPO="MateusAMP2119/iris-engine-cli"
BASE="https://github.com/${REPO}/releases/latest/download"

# Rainbow banner: Iris is the goddess of the rainbow. Colors only on a terminal
# and never when NO_COLOR is set.
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  printf '\033[1;31m  ██╗██████╗ ██╗███████╗\033[0m\n'
  printf '\033[1;33m  ██║██╔══██╗██║██╔════╝\033[0m\n'
  printf '\033[1;32m  ██║██████╔╝██║███████╗\033[0m\n'
  printf '\033[1;36m  ██║██╔══██╗██║╚════██║\033[0m\n'
  printf '\033[1;34m  ██║██║  ██║██║███████║\033[0m\n'
  printf '\033[1;35m  ╚═╝╚═╝  ╚═╝╚═╝╚══════╝\033[0m\n'
  printf '\033[2m  lakehouse engine · provenance first\033[0m\n\n'
else
  printf '  Iris: lakehouse engine, provenance first\n\n'
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

asset="iris_${os}_${arch}.tar.gz"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Downloading ${asset} ..."
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
echo "Checksum OK."

tar -xzf "${tmp}/${asset}" -C "$tmp"

# Prefer /usr/local/bin; fall back to ~/.local/bin when not writable and sudo is unavailable.
dest="/usr/local/bin"
if [ -w "$dest" ]; then
  mv "${tmp}/iris" "${dest}/iris"
elif command -v sudo >/dev/null 2>&1; then
  echo "Installing to ${dest} (sudo required) ..."
  sudo mv "${tmp}/iris" "${dest}/iris"
else
  dest="${HOME}/.local/bin"
  mkdir -p "$dest"
  mv "${tmp}/iris" "${dest}/iris"
  case ":${PATH}:" in
    *":${dest}:"*) ;;
    *) echo "NOTE: add ${dest} to your PATH." ;;
  esac
fi

echo "Installed $("${dest}/iris" --version 2>/dev/null || echo iris) to ${dest}/iris"

# Version gate: probe the installed binary itself before any offer. A binary
# that answers `quickstart --from-installer --json` gets the full ceremony
# handoff with the flag; one that only answers plain `quickstart --json` gets
# the flagless handoff (an E15-era tour); anything older gets no handoff and
# no quickstart next-step line -- an old binary is never offered a verb it
# lacks.
handoff="none"
if "${dest}/iris" quickstart --from-installer --json >/dev/null 2>&1; then
  handoff="continuation"
elif "${dest}/iris" quickstart --json >/dev/null 2>&1; then
  handoff="plain"
fi

# The handoff: one question, the single consent for the binary's acts. Prompt
# only with a controlling terminal (probed by opening /dev/tty, the
# uninstaller's rule) and IRIS_FORCE unset; decline, no terminal, or
# IRIS_FORCE prints the plain next-steps lines.
can_prompt() {
  [ -z "${IRIS_FORCE:-}" ] && (: </dev/tty >/dev/tty) 2>/dev/null
}

if [ "$handoff" != "none" ] && can_prompt; then
  printf 'Set up the engine now? (Y/n) ' >/dev/tty
  IFS= read -r ans </dev/tty || ans="n"
  case "$ans" in
    [nN]*) ;;
    *)
      # Accept is the default. The absolute path runs the tour even when dest is
      # not on PATH yet, and the re-tied stdin satisfies its terminal gate --
      # stdout is never re-tied. exec replaces this shell, so the EXIT trap will
      # not fire: clean up first.
      rm -rf "$tmp"
      trap - EXIT
      if [ "$handoff" = "continuation" ]; then
        exec "${dest}/iris" quickstart --from-installer </dev/tty
      fi
      exec "${dest}/iris" quickstart </dev/tty
      ;;
  esac
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
