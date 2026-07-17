---
name: local-build
description: Install iris from the working tree exactly like the public installer (curl … | bash) would — one script builds the binary, packages it as a release asset, and runs the repo's own install.sh against it. Use to try the engine from source, smoke-test a branch, or reproduce installer/quickstart behavior locally.
---

# local-build — the real installer, fed by the working tree

Mimics `curl -fsSL https://install.iris-lakehouse.bymarreco.com/snapshot | bash`
1:1, with the binary built from the current working tree instead of a GitHub
release. One command, from the repo root (script lives next to install.sh):

```bash
sh install-local.sh
```

The script builds with the release workflow's exact flags (CGO_ENABLED=0,
-trimpath, buildinfo.Version stamped `local.<date>.<sha>[-dirty]`), packages
`iris_<os>_<arch>.tar.gz` + `checksums.txt` into `.local/`, then execs the
repo's actual `install.sh` with `IRIS_BASE_URL=file://.local` — a knob the
installer ships with. Everything downstream is the genuine release path:
checksum verify, install to `/usr/local/bin` (or `~/.local/bin` fallback),
upgrade-in-place detection, plain next-steps lines.

## Knobs (pass through to install.sh)

- `IRIS_DEST=<dir>` — install somewhere other than `/usr/local/bin`.
- `NO_COLOR=1` — plain output.

## Rules

- This replaces the machine's installed `iris` — intended; the whole machine
  is a disposable dev environment.
- After installing a rebuilt binary, restart any running daemon — a resident
  daemon keeps executing the old binary (`iris engine stop` / `start -d`).
- Long runs (quickstart tour, pipeline runs): background with nohup + raw log
  file, never pipe through tail/grep.
- Rebuild + reinstall = rerun the script; it regenerates the tar + checksums
  every time so the installer never verifies a stale asset.

## Reset

```bash
iris engine uninstall --yes    # removes Postgres tree, object store, logs, socket, service unit
```

Engine home is `~/.iris` (`IRIS_HOME` relocates it; cwd never matters). Never
delete the home from under a live daemon — uninstall first.
