#!/bin/sh
# Build iris from the working tree, package it like a release asset, and run
# the repo's own install.sh against it (IRIS_BASE_URL=file://). 1:1 with
# `curl -fsSL https://install.iris-lakehouse.bymarreco.com/snapshot | bash`,
# local bits. Extra knobs pass through: IRIS_NO_SETUP=1, IRIS_DEST, NO_COLOR.
set -eu

ROOT="$(git rev-parse --show-toplevel)"
DEV="${ROOT}/.local"
mkdir -p "$DEV"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) echo "install-local: unsupported architecture: $arch" >&2; exit 1 ;;
esac

VERSION="local.$(date +%Y%m%d).$(git -C "$ROOT" rev-parse --short=12 HEAD)$(git -C "$ROOT" diff --quiet || echo -dirty)"
echo "· Building ${VERSION} (${os}/${arch})"
CGO_ENABLED=0 go build -trimpath \
  -ldflags="-s -w -X github.com/MateusAMP2119/iris-lakehouse/internal/buildinfo.Version=${VERSION}" \
  -o "$DEV/iris" "${ROOT}/cmd/iris"

tar -czf "${DEV}/iris_${os}_${arch}.tar.gz" -C "$DEV" iris
(cd "$DEV" && shasum -a 256 "iris_${os}_${arch}.tar.gz" > checksums.txt)

exec env IRIS_BASE_URL="file://${DEV}" bash "${ROOT}/install.sh"
