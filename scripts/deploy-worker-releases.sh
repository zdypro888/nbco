#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${WORKER_RELEASE_DIR:-${ROOT}/dist/worker}"
REMOTE="${WORKER_RELEASE_REMOTE:-root@im.app:/root/nbco/downloads/worker}"

"${ROOT}/scripts/build-worker-releases.sh"

ssh "${REMOTE%%:*}" "mkdir -p '${REMOTE#*:}'"
scp "${OUT}"/nbco-worker-* "${REMOTE}/"

echo "worker releases deployed to ${REMOTE}"
