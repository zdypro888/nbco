#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_DIR="${HOME}/.local/bin"
APP_DIR="${HOME}/Library/Application Support/nbco"
LOG_DIR="${HOME}/Library/Logs/nbco"
AGENT="${HOME}/Library/LaunchAgents/com.zdypro.nbco.plist"
LABEL="com.zdypro.nbco"
GUI_DOMAIN="gui/$(id -u)"
LISTEN="${NBCO_LISTEN:-127.0.0.1:8900}"
VERSION="$(git -C "${ROOT}" rev-parse --short=12 HEAD 2>/dev/null || echo dev)"

mkdir -p "${BIN_DIR}" "${APP_DIR}" "${LOG_DIR}" "$(dirname "${AGENT}")"

tmp_nbco="$(mktemp "${TMPDIR:-/tmp}/nbco.XXXXXX")"
tmp_worker="$(mktemp "${TMPDIR:-/tmp}/nbco-worker.XXXXXX")"
rm -f "${tmp_nbco}" "${tmp_worker}"
trap 'rm -f "${tmp_nbco}" "${tmp_worker}"' EXIT

go build -ldflags="-X main.version=${VERSION}" -o "${tmp_nbco}" "${ROOT}/cmd/nbco"
go build -o "${tmp_worker}" "${ROOT}/cmd/nbco-worker"
install -m 0755 "${tmp_nbco}" "${BIN_DIR}/nbco"
install -m 0755 "${tmp_worker}" "${BIN_DIR}/nbco-worker"
install -m 0644 "${ROOT}/nbco.json" "${APP_DIR}/nbco.json"

xattr -c "${BIN_DIR}/nbco" "${BIN_DIR}/nbco-worker" "${APP_DIR}/nbco.json" 2>/dev/null || true
codesign --force --sign - "${BIN_DIR}/nbco" "${BIN_DIR}/nbco-worker" >/dev/null 2>&1 || true

cat > "${AGENT}" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>${BIN_DIR}:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
	</dict>
	<key>KeepAlive</key>
	<true/>
	<key>Label</key>
	<string>${LABEL}</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/zsh</string>
		<string>-lc</string>
		<string>exec ${BIN_DIR}/nbco -config '${APP_DIR}/nbco.json'</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>StandardErrorPath</key>
	<string>${LOG_DIR}/nbco.log</string>
	<key>StandardOutPath</key>
	<string>${LOG_DIR}/nbco.log</string>
	<key>WorkingDirectory</key>
	<string>${APP_DIR}</string>
</dict>
</plist>
PLIST
plutil -lint "${AGENT}" >/dev/null

launchctl bootout "${GUI_DOMAIN}/${LABEL}" 2>/dev/null || true
pkill -f "${BIN_DIR}/nbco -config ${APP_DIR}/nbco.json" 2>/dev/null || true
launchctl bootstrap "${GUI_DOMAIN}" "${AGENT}"

for _ in $(seq 1 20); do
	if curl -fsS --max-time 2 "http://${LISTEN}/readyz" >/dev/null; then
		echo "nbco deployed: http://${LISTEN}/readyz ok"
		exit 0
	fi
	sleep 1
done

echo "nbco deploy failed: readyz not ready" >&2
tail -80 "${LOG_DIR}/nbco.log" >&2 || true
exit 1
