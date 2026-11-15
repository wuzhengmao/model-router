#!/usr/bin/env bash
# 一键构建脚本：构建当前平台（M1 为 windows/amd64）的可执行文件，产物输出到 dist/。
# 用法：
#   bash build.sh                 # 当前平台
#   GOOS=linux GOARCH=arm64 bash build.sh
set -euo pipefail

cd "$(dirname "$0")"

VERSION_FILE="internal/version/version.txt"
VERSION="$(tr -d '[:space:]' < "$VERSION_FILE")"
if [[ -z "$VERSION" ]]; then
  echo "version file is empty: $VERSION_FILE" >&2
  exit 1
fi

GOOS="${GOOS:-$(go env GOOS)}"
GOARCH="${GOARCH:-$(go env GOARCH)}"

EXT=""
if [[ "$GOOS" == "windows" ]]; then
  EXT=".exe"
fi

OUT_DIR="dist"
OUT="${OUT_DIR}/model-router-${VERSION}-${GOOS}-${GOARCH}${EXT}"
mkdir -p "$OUT_DIR"

echo "building ${OUT} (version ${VERSION}, GOOS=${GOOS}, GOARCH=${GOARCH}, CGO_ENABLED=0)"
CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" go build \
  -trimpath \
  -ldflags "-s -w -X model-router/internal/version.Version=${VERSION}" \
  -o "$OUT" .

echo "built ${OUT}"
