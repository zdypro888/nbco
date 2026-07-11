//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	workerRestartMarkerEnv     = "NBCO_WORKER_RESTART_MARKER"
	defaultWorkerRestartMarker = "/run/nbco-worker-restart-required"
)

var systemdServiceNameRE = regexp.MustCompile(`^[A-Za-z0-9_.@:-]+$`)

type deferredWorkerRestart struct {
	service       string
	currentBinary string
	backupBinary  string
}

const deferredRestartScript = `
set -eu
service=$1
current_binary=$2
backup_binary=$3

healthy=0
if systemctl restart "$service"; then
	stable=0
	i=0
	while [ "$i" -lt 20 ]; do
		sleep 1
		if systemctl is-active --quiet "$service"; then
			stable=$((stable + 1))
			if [ "$stable" -ge 5 ]; then
				healthy=1
				break
			fi
		else
			stable=0
		fi
		i=$((i + 1))
	done
fi

if [ "$healthy" -eq 1 ]; then
	exit 0
fi

install -m 0755 "$backup_binary" "$current_binary"
systemctl restart "$service"
systemctl is-active --quiet "$service"
exit 1
`

var runDeferredSystemdRestart = func(unit string, restart deferredWorkerRestart) error {
	cmd := exec.Command("systemd-run", "--quiet", "--collect", "--unit="+unit,
		"--on-active=2s", "/bin/sh", "-c", deferredRestartScript, "nbco-worker-restart",
		restart.service, restart.currentBinary, restart.backupBinary)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func scheduleDeferredWorkerRestart() (bool, error) {
	marker := strings.TrimSpace(os.Getenv(workerRestartMarkerEnv))
	if marker == "" {
		marker = defaultWorkerRestartMarker
	}
	claimed := marker + ".claim." + strconv.Itoa(os.Getpid())
	if err := os.Rename(marker, claimed); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("claim restart marker: %w", err)
	}
	restore := func() {
		if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
			_ = os.Rename(claimed, marker)
		}
	}

	data, err := os.ReadFile(claimed)
	if err != nil {
		restore()
		return false, fmt.Errorf("read restart marker: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	if len(lines) != 4 || !systemdServiceNameRE.MatchString(lines[0]) {
		restore()
		return false, errors.New("restart marker has an invalid format or systemd service name")
	}
	oldPID, parseErr := strconv.Atoi(lines[1])
	if parseErr != nil || oldPID <= 0 {
		restore()
		return false, errors.New("restart marker contains an invalid worker pid")
	}
	if oldPID != os.Getpid() {
		// The service has already restarted and loaded the installed binary.
		return false, os.Remove(claimed)
	}
	restart := deferredWorkerRestart{
		service:       lines[0],
		currentBinary: filepath.Clean(lines[2]),
		backupBinary:  filepath.Clean(lines[3]),
	}
	if !filepath.IsAbs(restart.currentBinary) || !filepath.IsAbs(restart.backupBinary) || restart.currentBinary == restart.backupBinary {
		restore()
		return false, errors.New("restart marker contains invalid worker binary paths")
	}
	if info, statErr := os.Stat(restart.backupBinary); statErr != nil || !info.Mode().IsRegular() {
		restore()
		return false, errors.New("restart marker backup binary is unavailable")
	}
	unit := fmt.Sprintf("nbco-worker-restart-%d-%d", os.Getpid(), time.Now().UnixNano())
	if err := runDeferredSystemdRestart(unit, restart); err != nil {
		restore()
		return false, fmt.Errorf("schedule systemd restart: %w", err)
	}
	if err := os.Remove(claimed); err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, fmt.Errorf("restart scheduled but marker cleanup failed: %w", err)
	}
	return true, nil
}
