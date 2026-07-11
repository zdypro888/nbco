//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureProcessTree(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error { return killProcessTree(cmd.Process) }
}

// The PTY package creates a new session whose process group ID is the child
// PID. Only replace CommandContext's direct-process cancellation hook here;
// its SysProcAttr is configured by pty.StartWithSize.
func configurePTYProcessTree(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return killProcessTree(cmd.Process) }
}

func killProcessTree(process *os.Process) error {
	if process == nil {
		return os.ErrProcessDone
	}
	groupErr := syscall.Kill(-process.Pid, syscall.SIGKILL)
	processErr := process.Kill()
	if groupErr == nil || errors.Is(groupErr, syscall.ESRCH) {
		return nil
	}
	if processErr == nil || errors.Is(processErr, os.ErrProcessDone) {
		return nil
	}
	return errors.Join(groupErr, processErr)
}
