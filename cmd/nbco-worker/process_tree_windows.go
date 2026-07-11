//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
)

func configureProcessTree(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return killProcessTree(cmd.Process) }
}

func configurePTYProcessTree(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return killProcessTree(cmd.Process) }
}

func killProcessTree(process *os.Process) error {
	if process == nil {
		return os.ErrProcessDone
	}
	treeErr := exec.Command("taskkill", "/PID", strconv.Itoa(process.Pid), "/T", "/F").Run()
	processErr := process.Kill()
	if treeErr == nil || processErr == nil || errors.Is(processErr, os.ErrProcessDone) {
		return nil
	}
	return errors.Join(treeErr, processErr)
}
