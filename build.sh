#!/usr/bin/env bash
# Build codemcp. With no arguments it builds for the host into ./codemcp; pass
# --all to cross-compile every release target into ./dist, which is what the
# GitHub Actions workflow does.
set -euo pipefail

cd "$(dirname "$0")"

# Release tags are the version itself (0.1.0042), so describe yields either
# that tag or "0.1.0042-3-gabc1234" a few commits later. An untagged clone gets
# a version in the same shape rather than a bare word.
version="$(git describe --tags --always 2>/dev/null || echo 0.1.0000-dev)"
ldflags="-s -w -X main.version=${version}"

build_one() {
  local goos="$1" goarch="$2" ext="${3:-}"
  local out="dist/codemcp-${goos}-${goarch}${ext}"
  echo "  ${out}"
  GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
    go build -trimpath -ldflags "$ldflags" -o "$out" .
}

case "${1:-}" in
  --all)
    echo "Building codemcp ${version} for all targets"
    mkdir -p dist
    build_one windows amd64 .exe
    build_one windows arm64 .exe
    build_one linux   amd64
    build_one linux   arm64
    build_one darwin  amd64
    build_one darwin  arm64
    if command -v sha256sum >/dev/null 2>&1; then
      (cd dist && sha256sum codemcp-* > SHA256SUMS)
      echo "Wrote dist/SHA256SUMS"
    fi
    ;;
  --test)
    gofmt -l .
    go vet ./...
    go test ./...
    ;;
  -h|--help)
    echo "Usage: ./build.sh [--all | --test | --help]"
    echo "  (no args)  build ./codemcp for this machine"
    echo "  --all      cross-compile every release target into ./dist"
    echo "  --test     gofmt, go vet and go test"
    exit 0
    ;;
  "")
    echo "Building codemcp ${version}"
    CGO_ENABLED=0 go build -trimpath -ldflags "$ldflags" -o codemcp .
    echo "Wrote ./codemcp"
    ;;
  *)
    echo "unknown argument: $1 (try --help)" >&2
    exit 1
    ;;
esac
