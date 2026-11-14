#!/usr/bin/env bash
# 本地一键全平台构建：windows/linux/darwin × amd64/arm64，共 6 个产物到 dist/。
set -euo pipefail

cd "$(dirname "$0")"

VERSION_FILE="internal/version/version.txt"
VERSION="$(tr -d '[:space:]' < "$VERSION_FILE")"
if [[ -z "$VERSION" ]]; then
  echo "version file is empty: $VERSION_FILE" >&2
  exit 1
fi

mkdir -p dist

targets=()
for GOOS in windows linux darwin; do
  for GOARCH in amd64 arm64; do
    echo "=== building ${GOOS}/${GOARCH} ==="
    GOOS="$GOOS" GOARCH="$GOARCH" bash build.sh
    EXT=""
    if [[ "$GOOS" == "windows" ]]; then
      EXT=".exe"
    fi
    targets+=("dist/model-router-${VERSION}-${GOOS}-${GOARCH}${EXT}")
  done
done

echo
echo "=== artifacts (version ${VERSION}) ==="
for target in "${targets[@]}"; do
  if [[ ! -f "$target" ]]; then
    echo "missing artifact: $target" >&2
    exit 1
  fi
  ls -l "$target"
done
echo "built ${#targets[@]} artifacts into dist/"
