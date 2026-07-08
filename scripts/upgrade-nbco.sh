#!/usr/bin/env bash
set -euo pipefail

usage() {
	cat <<'USAGE'
Usage: upgrade-nbco.sh [--dry-run] [ref]

Safely upgrade a systemd-managed nbco deployment.

The script is environment-neutral. It detects the running systemd service and
deployment config where possible; production-specific paths/domains should be
provided by environment variables or a host-local wrapper, not committed here.

Environment overrides:
  NBCO_REPO_DIR=/path/to/nbco/source
  NBCO_APP_DIR=/path/to/deployed/app
  NBCO_CONFIG=/path/to/nbco.json
  NBCO_SERVICE=nbco
  NBCO_BIN_NAME=<deployed-binary-name>  # default: symlink target of $NBCO_APP_DIR/bin/nbco
  NBCO_HEALTH_URL=https://host:port/healthz
  NBCO_HEALTH_TIMEOUT=60
  NBCO_KEEP_BACKUPS=10
  NBCO_ALLOW_DIRTY=0
  NBCO_REQUIRE_HEALTH_BEFORE=1
  NBCO_SKIP_TESTS=0

Examples:
  scripts/upgrade-nbco.sh
  scripts/upgrade-nbco.sh origin/main
  NBCO_SKIP_TESTS=1 scripts/upgrade-nbco.sh --dry-run HEAD
USAGE
}

log() { printf '[%s] %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*"; }
die() { log "ERROR: $*" >&2; exit 1; }

dry_run=0
if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
	usage
	exit 0
fi
if [[ "${1:-}" == "--dry-run" ]]; then
	dry_run=1
	shift
fi

ref="${1:-origin/main}"
service="${NBCO_SERVICE:-nbco}"
health_timeout="${NBCO_HEALTH_TIMEOUT:-60}"
keep_backups="${NBCO_KEEP_BACKUPS:-10}"
allow_dirty="${NBCO_ALLOW_DIRTY:-0}"
require_health_before="${NBCO_REQUIRE_HEALTH_BEFORE:-1}"
skip_tests="${NBCO_SKIP_TESTS:-0}"

lock_file="/var/lock/nbco-upgrade.lock"
ts="$(date -u '+%Y%m%d%H%M%S')"
repo_dir=""
app_dir=""
bin_dir=""
link_bin=""
bin_name=""
current_bin=""
config_file=""
health_url=""
stage_dir=""
stage_bin=""
backup_bin=""

need() { command -v "$1" >/dev/null 2>&1 || die "missing command: $1"; }

detect_exec_bin() {
	local line bin
	line="$(systemctl cat "${service}" 2>/dev/null | awk -F= '/^ExecStart=/ {print $2; exit}' || true)"
	bin="${line%% *}"
	if [[ -n "${bin}" && -x "${bin}" ]]; then
		printf '%s\n' "${bin}"
	fi
}

detect_config_file() {
	local line rest word prev
	line="$(systemctl cat "${service}" 2>/dev/null | awk -F= '/^ExecStart=/ {print $2; exit}' || true)"
	rest="${line#* }"
	prev=""
	for word in ${rest}; do
		if [[ "${prev}" == "-config" ]]; then
			printf '%s\n' "${word}"
			return 0
		fi
		case "${word}" in
			-config=*)
				printf '%s\n' "${word#-config=}"
				return 0
				;;
		esac
		prev="${word}"
	done
}

json_string_field() {
	local file="$1" field="$2"
	[[ -f "${file}" ]] || return 0
	sed -nE 's/.*"'"${field}"'"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/p' "${file}" | head -n 1
}

derive_health_url() {
	local cfg="$1" public listen tls scheme host port
	public="$(json_string_field "${cfg}" public_base_url)"
	if [[ -n "${public}" ]]; then
		printf '%s/healthz\n' "${public%/}"
		return 0
	fi
	listen="$(json_string_field "${cfg}" listen)"
	if [[ -z "${listen}" ]]; then
		listen="127.0.0.1:8900"
	fi
	tls="$(json_string_field "${cfg}" tls_cert_file)"
	scheme="http"
	if [[ -n "${tls}" ]]; then
		scheme="https"
	fi
	case "${listen}" in
		:*) host="127.0.0.1"; port="${listen#:}" ;;
		0.0.0.0:*) host="127.0.0.1"; port="${listen#0.0.0.0:}" ;;
		*:*) host="${listen%:*}"; port="${listen##*:}" ;;
		*) host="127.0.0.1"; port="${listen}" ;;
	esac
	printf '%s://%s:%s/healthz\n' "${scheme}" "${host}" "${port}"
}

wait_health() {
	local deadline=$((SECONDS + health_timeout))
	while (( SECONDS < deadline )); do
		if curl -fsS --max-time 2 "${health_url}" >/dev/null; then
			return 0
		fi
		sleep 2
	done
	return 1
}

collect_logs() {
	journalctl -u "${service}" --since '5 minutes ago' --no-pager | tail -n 120 || true
}

cleanup_stage() {
	rm -rf "${stage_dir}"
}

rollback() {
	local reason="$1"
	log "upgrade failed: ${reason}"
	if [[ -f "${backup_bin}" ]]; then
		log "rolling back binary: ${backup_bin} -> ${current_bin}"
		install -m 0755 "${backup_bin}" "${current_bin}"
		ln -sfn "${current_bin}" "${link_bin}"
		systemctl restart "${service}" || true
		if wait_health; then
			log "rollback succeeded; old service is healthy"
		else
			log "rollback attempted but health check still failed"
			collect_logs >&2
		fi
	else
		log "no backup binary exists; cannot rollback"
		collect_logs >&2
	fi
	exit 1
}

main() {
	need flock
	need git
	need go
	need curl
	need systemctl
	need journalctl

	mkdir -p "$(dirname "${lock_file}")"
	exec 9>"${lock_file}"
	flock -n 9 || die "another nbco upgrade is already running"

	local exec_bin detected_app_dir
	exec_bin="$(detect_exec_bin || true)"
	if [[ -n "${NBCO_APP_DIR:-}" ]]; then
		app_dir="${NBCO_APP_DIR}"
	elif [[ -n "${exec_bin}" ]]; then
		detected_app_dir="$(dirname "$(dirname "${exec_bin}")")"
		app_dir="$(cd "${detected_app_dir}" && pwd -P)"
	else
		die "NBCO_APP_DIR is not set and ${service}.service ExecStart could not be inspected"
	fi
	bin_dir="${app_dir}/bin"
	link_bin="${bin_dir}/nbco"
	if [[ -n "${NBCO_BIN_NAME:-}" ]]; then
		bin_name="${NBCO_BIN_NAME}"
		current_bin="${bin_dir}/${bin_name}"
	elif [[ -L "${link_bin}" ]]; then
		current_bin="$(readlink -f "${link_bin}")"
		bin_name="$(basename "${current_bin}")"
	else
		current_bin="${link_bin}"
		bin_name="$(basename "${current_bin}")"
	fi
	config_file="${NBCO_CONFIG:-$(detect_config_file || true)}"
	if [[ -z "${config_file}" ]]; then
		config_file="${app_dir}/nbco.json"
	fi
	health_url="${NBCO_HEALTH_URL:-$(derive_health_url "${config_file}")}"
	repo_dir="${NBCO_REPO_DIR:-$(pwd)}"
	stage_dir="${bin_dir}/.upgrade-${ts}"
	stage_bin="${stage_dir}/${bin_name}"
	backup_bin="${current_bin}.bak.${ts}"

	[[ -d "${repo_dir}/.git" ]] || die "repo not found: ${repo_dir}"
	[[ -d "${bin_dir}" ]] || die "bin dir not found: ${bin_dir}"
	[[ -f "${current_bin}" ]] || die "current binary not found: ${current_bin}"

	if [[ "${require_health_before}" != "0" ]]; then
		log "checking current service health: ${health_url}"
		wait_health || die "current service is not healthy before upgrade; refusing to deploy"
	fi

	cd "${repo_dir}"
	if [[ "${allow_dirty}" != "1" ]] && [[ -n "$(git status --porcelain)" ]]; then
		git status --short >&2 || true
		die "repo has uncommitted changes; set NBCO_ALLOW_DIRTY=1 only for an intentional manual deploy"
	fi

	log "fetching origin"
	git fetch --prune origin
	case "${ref}" in
		main|origin/main)
			log "updating main with --ff-only"
			git checkout main
			git pull --ff-only origin main
			;;
		*)
			log "checking out ${ref}"
			git checkout --detach "${ref}"
			;;
	esac

	local rev
	rev="$(git rev-parse --short=12 HEAD)"
	log "target revision: ${rev}"

	if [[ "${skip_tests}" != "1" ]]; then
		log "running tests"
		go test ./... -count=1
	else
		log "skipping tests because NBCO_SKIP_TESTS=1"
	fi

	log "building ${stage_bin}"
	mkdir -p "${stage_dir}"
	trap cleanup_stage EXIT
	go build -trimpath -o "${stage_bin}" ./cmd/nbco
	chmod 0755 "${stage_bin}"

	if [[ "${dry_run}" == "1" ]]; then
		log "dry run complete; built ${stage_bin}, no restart performed"
		exit 0
	fi

	log "backing up ${current_bin} -> ${backup_bin}"
	cp -p "${current_bin}" "${backup_bin}"

	log "installing new binary"
	install -m 0755 "${stage_bin}" "${current_bin}"
	ln -sfn "${current_bin}" "${link_bin}"

	log "restarting ${service}"
	systemctl restart "${service}" || rollback "systemctl restart ${service} failed"

	log "waiting for health check: ${health_url}"
	if ! wait_health; then
		rollback "new service did not become healthy within ${health_timeout}s"
	fi

	log "upgrade succeeded: ${service} is healthy at ${rev}"
	collect_logs | tail -n 40 || true

	log "pruning old backups, keeping ${keep_backups}"
	find "${bin_dir}" -maxdepth 1 -type f -name "${bin_name}.bak.*" -print0 |
		xargs -0 ls -1t 2>/dev/null |
		awk "NR>${keep_backups}" |
		xargs -r rm -f
}

main "$@"
