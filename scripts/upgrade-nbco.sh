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
  NBCO_HEALTH_URL=https://host:port/readyz
  NBCO_HEALTH_TIMEOUT=60
  NBCO_KEEP_BACKUPS=10
  NBCO_ALLOW_DIRTY=0
  NBCO_REQUIRE_HEALTH_BEFORE=1
  NBCO_SKIP_TESTS=0
  NBCO_GO_JOBS=2                    # concurrent Go build/test package jobs
  NBCO_GO_MAX_PROCS=2               # CPU threads available to each Go command
  NBCO_LOCK_FILE=/var/lock/nbco-upgrade.lock
  NBCO_UPGRADE_WORKER=auto             # auto|1|0; auto upgrades a detected local worker service
  NBCO_WORKER_SERVICE=nbco-worker
  NBCO_WORKER_BIN=/path/to/nbco-worker # optional; default: detected from worker service ExecStart
  NBCO_WORKER_RESTART_MARKER=/run/nbco-worker-restart-required

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
go_jobs="${NBCO_GO_JOBS:-2}"
go_max_procs="${NBCO_GO_MAX_PROCS:-2}"
upgrade_worker="${NBCO_UPGRADE_WORKER:-auto}"
worker_service="${NBCO_WORKER_SERVICE:-nbco-worker}"
worker_restart_marker="${NBCO_WORKER_RESTART_MARKER:-/run/nbco-worker-restart-required}"

lock_file="${NBCO_LOCK_FILE:-/var/lock/nbco-upgrade.lock}"
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
stage_worker_bin=""
current_worker_bin=""
worker_bin_name=""
backup_worker_bin=""
build_worker=0

need() { command -v "$1" >/dev/null 2>&1 || die "missing command: $1"; }

positive_integer() {
	[[ "$1" =~ ^[1-9][0-9]*$ ]]
}

detect_service_exec_bin() {
	local svc="$1" line bin
	line="$(systemctl cat "${svc}" 2>/dev/null | awk -F= '/^ExecStart=/ {print $2; exit}' || true)"
	line="${line#"${line%%[![:space:]]*}"}"
	line="${line#-}" # systemd allows ExecStart=-/path to ignore command failure.
	if [[ "${line}" == \"* ]]; then
		bin="${line#\"}"
		bin="${bin%%\"*}"
	else
		bin="${line%% *}"
	fi
	bin="${bin%\"}"
	if [[ -n "${bin}" && -x "${bin}" ]]; then
		printf '%s\n' "${bin}"
	fi
}

detect_exec_bin() {
	detect_service_exec_bin "${service}"
}

service_exists() {
	systemctl cat "$1" >/dev/null 2>&1
}

valid_service_name() {
	[[ "$1" =~ ^[A-Za-z0-9_.@:-]+$ ]]
}

service_main_pid() {
	systemctl show --property=MainPID --value "$1" 2>/dev/null || true
}

is_descendant_process() {
	local pid="$$" ancestor="$1" parent
	[[ "${ancestor}" =~ ^[1-9][0-9]*$ ]] || return 1
	while [[ "${pid}" =~ ^[1-9][0-9]*$ ]] && (( pid > 1 )); do
		[[ "${pid}" == "${ancestor}" ]] && return 0
		[[ -r "/proc/${pid}/status" ]] || return 1
		parent="$(awk '/^PPid:/ {print $2; exit}' "/proc/${pid}/status")"
		[[ "${parent}" =~ ^[0-9]+$ ]] || return 1
		pid="${parent}"
	done
	return 1
}

defer_worker_restart_if_self() {
	local main_pid tmp
	valid_service_name "${worker_service}" || die "invalid NBCO_WORKER_SERVICE: ${worker_service}"
	main_pid="$(service_main_pid "${worker_service}")"
	if ! is_descendant_process "${main_pid}"; then
		return 1
	fi
	[[ "${current_worker_bin}" == /* && "${backup_worker_bin}" == /* ]] || die "worker binary and backup paths must be absolute"
	mkdir -p "$(dirname "${worker_restart_marker}")"
	tmp="${worker_restart_marker}.$$"
	if ! (umask 077; printf '%s\n%s\n%s\n%s\n' \
		"${worker_service}" "${main_pid}" "${current_worker_bin}" "${backup_worker_bin}" >"${tmp}") || \
		! mv -f "${tmp}" "${worker_restart_marker}"; then
		rm -f "${tmp}"
		return 2
	fi
	return 0
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
		printf '%s/readyz\n' "${public%/}"
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
	printf '%s://%s:%s/readyz\n' "${scheme}" "${host}" "${port}"
}

wait_health() {
	local expected_version="${1:-}" deadline=$((SECONDS + health_timeout)) version_url liveness_url body actual
	version_url="${health_url%/*}/version"
	liveness_url="${health_url%/*}/healthz"
	while (( SECONDS < deadline )); do
		if curl -fsS --max-time 2 "${health_url}" >/dev/null; then
			if [[ -z "${expected_version}" ]]; then
				return 0
			fi
			body="$(curl -fsS --max-time 2 "${version_url}" 2>/dev/null || true)"
			actual="$(printf '%s' "${body}" | sed -nE 's/.*"version"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/p')"
			if [[ "${actual}" == "${expected_version}" ]]; then
				return 0
			fi
		elif [[ -z "${expected_version}" ]] && curl -fsS --max-time 2 "${liveness_url}" >/dev/null; then
			# Backward compatibility for the first upgrade from a release that
			# predates /readyz. Post-upgrade validation always supplies an exact
			# version and therefore still requires the new readiness endpoint.
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

rollback_worker() {
	local reason="$1"
	log "worker upgrade failed: ${reason}"
	if [[ -f "${backup_worker_bin}" ]]; then
		log "rolling back worker binary: ${backup_worker_bin} -> ${current_worker_bin}"
		install -m 0755 "${backup_worker_bin}" "${current_worker_bin}" || true
		systemctl restart "${worker_service}" || true
		if systemctl is-active --quiet "${worker_service}"; then
			log "worker rollback succeeded"
		else
			log "worker rollback attempted but ${worker_service} is not active"
			journalctl -u "${worker_service}" --since '5 minutes ago' --no-pager | tail -n 120 || true
		fi
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
	positive_integer "${go_jobs}" || die "NBCO_GO_JOBS must be a positive integer"
	positive_integer "${go_max_procs}" || die "NBCO_GO_MAX_PROCS must be a positive integer"

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
	case "${upgrade_worker}" in
		0|false|FALSE|no|NO)
			build_worker=0
			;;
		auto|1|true|TRUE|yes|YES)
			current_worker_bin="${NBCO_WORKER_BIN:-$(detect_service_exec_bin "${worker_service}" || true)}"
			if [[ -n "${current_worker_bin}" && -x "${current_worker_bin}" ]] && service_exists "${worker_service}"; then
				build_worker=1
				worker_bin_name="$(basename "${current_worker_bin}")"
				stage_worker_bin="${stage_dir}/${worker_bin_name}"
				backup_worker_bin="${current_worker_bin}.bak.${ts}"
			elif [[ "${upgrade_worker}" == "1" || "${upgrade_worker}" == "true" || "${upgrade_worker}" == "TRUE" || "${upgrade_worker}" == "yes" || "${upgrade_worker}" == "YES" ]]; then
				die "worker upgrade requested but ${worker_service} or worker binary could not be detected; set NBCO_WORKER_SERVICE/NBCO_WORKER_BIN or NBCO_UPGRADE_WORKER=0"
			else
				log "worker upgrade skipped: ${worker_service} service/binary not detected"
			fi
			;;
		*)
			die "NBCO_UPGRADE_WORKER must be auto, 1, or 0"
			;;
	esac

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
		log "running tests (jobs=${go_jobs}, max_procs=${go_max_procs})"
		GOMAXPROCS="${go_max_procs}" go test -p "${go_jobs}" ./... -count=1
	else
		log "skipping tests because NBCO_SKIP_TESTS=1"
	fi

	log "building ${stage_bin}"
	mkdir -p "${stage_dir}"
	trap cleanup_stage EXIT
	GOMAXPROCS="${go_max_procs}" go build -p "${go_jobs}" -trimpath -ldflags="-X main.version=${rev}" -o "${stage_bin}" ./cmd/nbco
	chmod 0755 "${stage_bin}"
	if [[ "${build_worker}" == "1" ]]; then
		log "building ${stage_worker_bin}"
		GOMAXPROCS="${go_max_procs}" go build -p "${go_jobs}" -trimpath -ldflags="-s -w" -o "${stage_worker_bin}" ./cmd/nbco-worker
		chmod 0755 "${stage_worker_bin}"
	fi

	if [[ "${dry_run}" == "1" ]]; then
		if [[ "${build_worker}" == "1" ]]; then
			log "dry run complete; built ${stage_bin} and ${stage_worker_bin}, no restart performed"
		else
			log "dry run complete; built ${stage_bin}, no restart performed"
		fi
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
	if ! wait_health "${rev}"; then
		rollback "new service did not become healthy within ${health_timeout}s"
	fi

	log "upgrade succeeded: ${service} is healthy at ${rev}"
	collect_logs | tail -n 40 || true

	if [[ "${build_worker}" == "1" ]]; then
		log "backing up worker ${current_worker_bin} -> ${backup_worker_bin}"
		cp -p "${current_worker_bin}" "${backup_worker_bin}"
		log "installing new worker binary"
		install -m 0755 "${stage_worker_bin}" "${current_worker_bin}"
		if defer_worker_restart_if_self; then
			log "worker restart deferred until this task result is submitted (${worker_restart_marker})"
		else
			defer_status=$?
			if [[ "${defer_status}" == "2" ]]; then
				install -m 0755 "${backup_worker_bin}" "${current_worker_bin}" || true
				die "could not persist deferred worker restart marker; restored previous worker binary"
			fi
			log "restarting ${worker_service}"
			systemctl restart "${worker_service}" || rollback_worker "systemctl restart ${worker_service} failed"
			if ! systemctl is-active --quiet "${worker_service}"; then
				rollback_worker "${worker_service} is not active after restart"
			fi
			log "worker upgrade succeeded: ${worker_service} is active"
			journalctl -u "${worker_service}" --since '2 minutes ago' --no-pager | tail -n 40 || true
		fi
	fi

	log "pruning old backups, keeping ${keep_backups}"
	find "${bin_dir}" -maxdepth 1 -type f -name "${bin_name}.bak.*" -print0 |
		xargs -0 ls -1t 2>/dev/null |
		awk "NR>${keep_backups}" |
		xargs -r rm -f
	if [[ "${build_worker}" == "1" ]]; then
		find "$(dirname "${current_worker_bin}")" -maxdepth 1 -type f -name "${worker_bin_name}.bak.*" -print0 |
			xargs -0 ls -1t 2>/dev/null |
			awk "NR>${keep_backups}" |
			xargs -r rm -f
	fi
}

main "$@"
