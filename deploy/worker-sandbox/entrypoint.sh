#!/bin/sh
# 首次启动：用一次性绑定码兑换 token 写进容器卷；之后直接上线。
set -eu
CONFIG="$HOME/.nbco-worker.json"
if [ ! -f "$CONFIG" ]; then
  if [ -z "${NBCO_SERVER:-}" ] || [ -z "${NBCO_BIND_CODE:-}" ]; then
    echo "首次启动需要 NBCO_SERVER 与 NBCO_BIND_CODE 环境变量" >&2
    exit 1
  fi
  "$HOME/nbco-worker" bind "$NBCO_SERVER" "$NBCO_BIND_CODE"
fi
exec "$HOME/nbco-worker" run "$@"
