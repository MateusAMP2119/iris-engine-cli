#!/bin/sh
# Iris uninstaller: removes the iris binary installed by install.sh.
#
# Recommended:
#   curl -fsSL https://install.iris-lakehouse.bymarreco.com/uninstall.sh | bash
#
# Current (raw GitHub):
#   curl -fsSL https://raw.githubusercontent.com/MateusAMP2119/iris-engine-cli/HEAD/uninstall.sh | bash
#
# Engine state (managed Postgres, meta schema, workspaces) is NOT touched here;
# remove it first with `iris engine uninstall` while the binary still exists.
# Interactive terminals get a confirmation prompt (read from /dev/tty, so it
# works under `curl | sh`); without a terminal the old strict behavior applies.
# Set IRIS_FORCE=1 to skip every prompt and guard.
set -eu

# Colors only on a terminal and never when NO_COLOR is set.
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  R='\033[1;31m' Y='\033[1;33m' G='\033[1;32m' C='\033[1;36m' B='\033[1;34m' M='\033[1;35m' D='\033[2m' X='\033[0m'
else
  R='' Y='' G='' C='' B='' M='' D='' X=''
fi

# can_prompt: a controlling terminal exists to ask questions on. The -r/-w tests
# are not enough (the node can exist with no controlling terminal), so actually
# try to open it.
can_prompt() {
  [ "${IRIS_FORCE:-0}" != "1" ] && (: </dev/tty >/dev/tty) 2>/dev/null
}

# confirm <prompt>: ask on /dev/tty, default No.
confirm() {
  printf "%b" "$1" >/dev/tty
  IFS= read -r ans </dev/tty || ans=""
  case "$ans" in
    y | Y | yes | YES) return 0 ;;
    *) return 1 ;;
  esac
}

bin=$(command -v iris 2>/dev/null) || {
  printf "%b\n" "${D}iris: not found on PATH; nothing to uninstall.${X}"
  exit 0
}

case "$bin" in
  /usr/local/bin/iris | "${HOME}/.local/bin/iris") ;;
  *)
    echo "iris: found at ${bin}, which install.sh does not manage." >&2
    echo "Remove it with the tool that installed it (go install, package manager, manual build)." >&2
    exit 1
    ;;
esac

ver=$("$bin" --version 2>/dev/null || echo "iris")

# A running daemon or provisioned engine should be torn down while the binary
# can still do it. Best-effort probe: engine info answers only if a daemon runs.
if [ "${IRIS_FORCE:-0}" != "1" ] && "$bin" engine info >/dev/null 2>&1; then
  printf "%b\n" "${Y}  ┌──────────────────────────────────────────────┐${X}"
  printf "%b\n" "${Y}  │${X}  ${R}!${X} A live iris daemon answered on this host   ${Y}│${X}"
  printf "%b\n" "${Y}  │${X}    Engine state should be torn down first:   ${Y}│${X}"
  printf "%b\n" "${Y}  │${X}      ${C}iris engine stop${X}                        ${Y}│${X}"
  printf "%b\n" "${Y}  │${X}      ${C}iris engine uninstall${X}                   ${Y}│${X}"
  printf "%b\n" "${Y}  └──────────────────────────────────────────────┘${X}"
  if can_prompt; then
    if ! confirm "  ${M}Remove the binary anyway, leaving engine state behind?${X} ${D}(y/N)${X} "; then
      printf "%b\n" "${G}  Aborted. Tear the engine down first, then re-run.${X}"
      exit 0
    fi
  else
    echo "(no terminal to ask on; set IRIS_FORCE=1 to remove the binary anyway)" >&2
    exit 1
  fi
fi

if can_prompt; then
  printf "%b\n" "${C}  ┌──────────────────────────────────────────────┐${X}"
  printf "%b\n" "${C}  │${X}   Uninstall ${M}${ver}${X} from ${bin}?"
  printf "%b\n" "${C}  └──────────────────────────────────────────────┘${X}"
  if ! confirm "  ${M}Uninstall iris?${X} ${D}(y/N)${X} "; then
    printf "%b\n" "${G}  Aborted. Nothing removed.${X}"
    exit 0
  fi
fi

if [ -w "$bin" ] || [ -w "$(dirname "$bin")" ]; then
  rm "$bin"
elif command -v sudo >/dev/null 2>&1; then
  echo "Removing ${bin} (sudo required) ..."
  sudo rm "$bin"
else
  echo "iris: cannot remove ${bin}: no write permission and no sudo." >&2
  exit 1
fi

printf "%b\n" "  Uninstalled ${bin}."
printf "%b%b%b%b%b%b%b%b\n" "${R}  G" "${Y}o" "${G}o" "${C}d" "${B}b" "${M}y" "${R}e" "${X} from iris."
