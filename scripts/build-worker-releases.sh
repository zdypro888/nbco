#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${WORKER_RELEASE_DIR:-${ROOT}/dist/worker}"

mkdir -p "${OUT}"

build_one() {
  local goos="$1"
  local goarch="$2"
  local suffix="$3"
  local name="nbco-worker-${goos}-${goarch}${suffix}"
  echo "building ${name}"
  GOOS="${goos}" GOARCH="${goarch}" CGO_ENABLED=0 \
    go build -trimpath -ldflags="-s -w" -o "${OUT}/${name}" "${ROOT}/cmd/nbco-worker"
  (cd "${OUT}" && shasum -a 256 "${name}" > "${name}.sha256")
}

build_one darwin arm64 ""
build_one linux amd64 ""
build_one linux arm64 ""
build_one windows amd64 ".exe"

echo "worker releases written to ${OUT}"
