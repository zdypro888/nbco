#!/bin/sh
# 首次启动：用一次性绑定码兑换 token 写进容器卷；之后直接上线。
set -eu
CONFIG="${NBCO_WORKER_CONFIG:-$HOME/.config/nbco/worker.json}"
mkdir -p "$(dirname "$CONFIG")" "$HOME/nbco-work"
if [ ! -f "$CONFIG" ]; then
  if [ -z "${NBCO_SERVER:-}" ] || [ -z "${NBCO_BIND_CODE:-}" ]; then
    echo "首次启动需要 NBCO_SERVER 与 NBCO_BIND_CODE 环境变量" >&2
    exit 1
  fi
  nbco-worker bind -config "$CONFIG" "$NBCO_SERVER" "$NBCO_BIND_CODE"
fi
exec nbco-worker run -config "$CONFIG" "$@"
