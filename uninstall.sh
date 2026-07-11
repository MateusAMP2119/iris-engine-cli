#!/bin/sh
# Iris uninstaller: removes the iris binary installed by install.sh.
#   curl -fsSL https://raw.githubusercontent.com/MateusAMP2119/iris-engine-cli/master/uninstall.sh | sh
#
# Engine state (managed Postgres, meta schema, workspaces) is NOT touched here;
# remove it first with `iris engine uninstall` while the binary still exists.
# Set IRIS_FORCE=1 to remove the binary anyway.
set -eu

bin=$(command -v iris 2>/dev/null) || {
  echo "iris: not found on PATH; nothing to uninstall."
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

# A running daemon or provisioned engine should be torn down while the binary
# can still do it. Best-effort probe: engine info answers only if a daemon runs.
if [ "${IRIS_FORCE:-0}" != "1" ] && "$bin" engine info >/dev/null 2>&1; then
  echo "iris: a daemon is reachable. Run these first, then re-run this script:" >&2
  echo "  iris engine stop" >&2
  echo "  iris engine uninstall" >&2
  echo "(or set IRIS_FORCE=1 to remove the binary anyway)" >&2
  exit 1
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

echo "Uninstalled ${bin}."
echo "Bye from the rainbow goddess."
